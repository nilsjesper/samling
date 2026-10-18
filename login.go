package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nilsjesper/samling/internal/browse"
	"github.com/nilsjesper/samling/internal/config"
	"github.com/nilsjesper/samling/internal/instapaper"
	"github.com/nilsjesper/samling/internal/oauth1"
)

// defaultCallbackPort is the loopback port for the OAuth 2 redirect. It can't
// be chosen at random: Instapaper matches redirect_uri exactly against the
// application's registered callback URIs, so the user registers this one.
const defaultCallbackPort = 8919

// callbackTimeout bounds how long login waits for the browser. Long enough to
// sign in and find the consent button, short enough that a forgotten terminal
// doesn't hold the port forever.
const callbackTimeout = 10 * time.Minute

const applicationsURL = "https://www.instapaper.com/developers/applications"

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	useToken := fs.Bool("token", false, "save an access token from your application's page (read from stdin or a hidden prompt)")
	usePassword := fs.Bool("password", false, "use the deprecated xAuth username/password exchange (until 2027-09-30)")
	username := fs.String("username", "", "with --password: Instapaper username or email (prompted if omitted)")
	port := fs.Int("port", defaultCallbackPort, "loopback port for the browser sign-in redirect")
	noOpen := fs.Bool("no-open", false, "print the sign-in URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		// Most likely `login --token <value>`. Refuse it rather than accept a
		// secret that is now sitting in shell history.
		return errors.New("login takes no arguments; give an access token on stdin or at the prompt, " +
			"not on the command line, so it stays out of your shell history")
	}
	// --username only ever meant the password flow, so keep old invocations working.
	if *username != "" {
		*usePassword = true
	}
	if *useToken && *usePassword {
		return errors.New("--token and --password are mutually exclusive")
	}

	ctx, stop := signalContext()
	defer stop()

	switch {
	case *useToken:
		return loginWithToken(ctx)
	case *usePassword:
		return loginWithPassword(ctx, *username)
	default:
		return loginWithBrowser(ctx, *port, !*noOpen)
	}
}

// loginWithToken stores a token generated on the application's page. It needs
// no consumer credentials at all: the token alone is a v2 bearer token.
func loginWithToken(ctx context.Context) error {
	tok, err := promptPassword("Access token: ")
	if err != nil {
		return err
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return errors.New("no access token given")
	}

	if err := instapaper.VerifyAccessToken(ctx, loginHTTPClient(), instapaper.BaseURL, tok); err != nil {
		if errors.Is(err, instapaper.ErrUnauthorized) {
			return fmt.Errorf("Instapaper rejected that access token. It may have been regenerated or revoked;\n"+
				"generate a new one on your application's page at %s", applicationsURL)
		}
		return err
	}

	if err := config.SaveToken(config.Token{Token: tok}); err != nil {
		return err
	}
	fmt.Printf("Access token verified.\nToken saved to %s\nNext: samling sync\n", config.Path("token.json"))
	return nil
}

// loginWithBrowser runs the OAuth 2 authorization code flow with a loopback
// redirect: open the consent page, catch the code on 127.0.0.1, exchange it.
func loginWithBrowser(ctx context.Context, port int, open bool) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	// Bind before sending the user anywhere, so a busy port fails up front
	// rather than after they've clicked through the consent page.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("can't listen on 127.0.0.1:%d for the sign-in redirect: %w\n"+
			"Pick another port with --port and register its callback URI on your application's page.", port, err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, instapaper.OAuth2CallbackPath)

	state, err := randomState()
	if err != nil {
		ln.Close()
		return err
	}
	authURL := instapaper.AuthorizeURL(instapaper.BaseURL, cfg.ConsumerKey, redirectURI, state)

	fmt.Printf("Signing in through your browser. This needs\n    %s\n"+
		"listed under Callback URIs on your application's page (%s).\n"+
		"Use `samling login --token` instead if you'd rather paste a token.\n\n", redirectURI, applicationsURL)
	if open {
		if err := browse.Open(authURL); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		fmt.Printf("If the browser didn't open, visit:\n    %s\n\n", authURL)
	} else {
		fmt.Printf("Open this in a browser:\n    %s\n\n", authURL)
	}
	fmt.Println("Waiting for Instapaper to redirect back (Ctrl-C to cancel)...")

	waitCtx, cancel := context.WithTimeout(ctx, callbackTimeout)
	defer cancel()
	code, err := waitForCode(waitCtx, ln, state)
	if err != nil {
		return err
	}

	hc := loginHTTPClient()
	bt, err := instapaper.ExchangeCode(ctx, hc, instapaper.BaseURL,
		cfg.ConsumerKey, cfg.ConsumerSecret, redirectURI, code)
	if err != nil {
		return err
	}
	if err := instapaper.VerifyAccessToken(ctx, hc, instapaper.BaseURL, bt.AccessToken); err != nil {
		return fmt.Errorf("got a token but could not verify it: %w", err)
	}

	if err := config.SaveToken(config.Token{
		Token:    bt.AccessToken,
		UserID:   bt.UserID,
		Username: bt.Username,
	}); err != nil {
		return err
	}
	fmt.Printf("Logged in as %s.\nToken saved to %s\nNext: samling sync\n",
		accountName(bt.Username, bt.UserID), config.Path("token.json"))
	return nil
}

// loginWithPassword is the original xAuth exchange. Instapaper stops accepting
// new xAuth sign-ins on 2027-09-30; tokens it already issued keep working, so
// this can go once that date passes.
func loginWithPassword(ctx context.Context, user string) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	if user == "" {
		if user, err = prompt("Instapaper username or email: "); err != nil {
			return err
		}
	}
	password, err := promptPassword("Password (leave blank if your account has none): ")
	if err != nil {
		return err
	}

	client := instapaper.New(oauth1.Credentials{
		ConsumerKey:    cfg.ConsumerKey,
		ConsumerSecret: cfg.ConsumerSecret,
	})

	token, secret, err := client.AccessToken(ctx, user, password)
	if err != nil {
		return err
	}
	client.Creds.Token, client.Creds.TokenSecret = token, secret

	account, err := client.VerifyCredentials(ctx)
	if err != nil {
		return fmt.Errorf("got a token but could not verify it: %w", err)
	}

	if err := config.SaveToken(config.Token{
		Token:       token,
		TokenSecret: secret,
		UserID:      account.UserID,
		Username:    account.Username,
	}); err != nil {
		return err
	}

	fmt.Printf("Logged in as %s (user %d).\nToken saved to %s\nNext: samling sync\n",
		account.Username, account.UserID, config.Path("token.json"))
	return nil
}

// callbackResult is what the loopback handler hands back to the waiting login.
type callbackResult struct {
	code string
	err  error
}

// waitForCode serves the loopback redirect on ln until one callback arrives or
// ctx ends, and returns the authorization code. It always closes ln.
func waitForCode(ctx context.Context, ln net.Listener, state string) (string, error) {
	done := make(chan callbackResult, 1)
	srv := &http.Server{
		Handler:           callbackHandler(state, done),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(ln)
	defer func() {
		// Give the handler's response a moment to reach the browser.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		// Shutdown only closes listeners Serve has registered, and Serve may
		// not have started yet if ctx was already done.
		ln.Close()
	}()

	select {
	case r := <-done:
		return r.code, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("timed out waiting for the browser sign-in.\n" +
				"If Instapaper showed an error about the redirect URI, register it on your application's page.")
		}
		return "", ctx.Err()
	}
}

// callbackHandler answers the OAuth 2 redirect. It reports the first request
// on the callback path and ignores everything else (a browser will also ask
// for /favicon.ico). The state check rejects a redirect this login didn't
// start, which is what stops another page from feeding us its own code.
func callbackHandler(state string, done chan<- callbackResult) http.Handler {
	var once sync.Once
	finish := func(w http.ResponseWriter, status int, msg string, r callbackResult) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<!doctype html><title>samling</title><p>%s</p>\n", html.EscapeString(msg))
		once.Do(func() { done <- r })
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != instapaper.OAuth2CallbackPath {
			http.NotFound(w, req)
			return
		}
		q := req.URL.Query()
		if q.Get("state") != state {
			finish(w, http.StatusBadRequest, "This sign-in didn't come from samling's request. Run samling login again.",
				callbackResult{err: errors.New("the sign-in redirect had the wrong state, so it was rejected: run `samling login` again")})
			return
		}
		if e := q.Get("error"); e != "" {
			desc := q.Get("error_description")
			if desc == "" {
				desc = e
			}
			finish(w, http.StatusOK, "Sign-in cancelled. You can close this tab.",
				callbackResult{err: fmt.Errorf("Instapaper did not authorize samling: %s", desc)})
			return
		}
		code := q.Get("code")
		if code == "" {
			finish(w, http.StatusBadRequest, "No authorization code came back. Run samling login again.",
				callbackResult{err: errors.New("the sign-in redirect had no authorization code")})
			return
		}
		finish(w, http.StatusOK, "samling is signed in. You can close this tab.", callbackResult{code: code})
	})
}

// randomState returns an unguessable value for the OAuth 2 state parameter.
func randomState() (string, error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating state: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func loginHTTPClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

func accountName(username string, id int64) string {
	switch {
	case username != "" && id != 0:
		return fmt.Sprintf("%s (user %d)", username, id)
	case username != "":
		return username
	case id != 0:
		return fmt.Sprintf("user %d", id)
	default:
		return "your Instapaper account"
	}
}
