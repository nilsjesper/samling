// OAuth 2 sign-in for API v2, which replaces xAuth.
//
// This is deliberately separate from Client: everything here runs before there
// is a token, or with nothing but a bearer token, so it has no use for the
// OAuth 1.0a credentials Client carries. The functions take the base URL and
// HTTP client explicitly so tests can point them at a fake server.
//
// Facts from Instapaper's v2 "Authentication" and "Migrating from v1" docs that
// shape this code:
//   - The consumer key and secret are the OAuth 2 client_id and client_secret.
//   - The flow is plain authorization code: authorize, redirect with ?code=,
//     then POST the code to /oauth2/token. No PKCE and no refresh tokens are
//     described; access tokens do not expire.
//   - redirect_uri must match a registered callback URI exactly, so the
//     loopback port cannot be random. Local callbacks are only accepted on an
//     "Owner Only" application, which is what a personal tool is anyway.
//   - An existing xAuth oauth_token also works as a v2 bearer token, as does
//     the "Generate access token" token from the application's page.
package instapaper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// OAuth2CallbackPath is the path the loopback redirect URI uses. It is part of
// the URI the user has to register, so it is fixed.
const OAuth2CallbackPath = "/callback"

// BearerToken is what /oauth2/token hands back for a redeemed code.
type BearerToken struct {
	AccessToken string
	UserID      int64
	Username    string
}

// AuthorizeURL is the consent page to send the user to. state is echoed back
// on the redirect so the caller can reject a callback it did not start.
func AuthorizeURL(baseURL, clientID, redirectURI, state string) string {
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"state":         {state},
	}
	return strings.TrimSuffix(baseURL, "/") + "/oauth2/authorize?" + q.Encode()
}

// oauth2ErrorBody is the standard OAuth 2 error shape the token endpoint uses,
// which is not the {"error": {"code", "message"}} shape of the rest of v2.
type oauth2ErrorBody struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// ExchangeCode redeems an authorization code. redirectURI must be byte-for-byte
// the value sent to the authorize page: it is part of the code's identity.
func ExchangeCode(ctx context.Context, hc *http.Client, baseURL, clientID, clientSecret, redirectURI, code string) (BearerToken, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
		"code":          {code},
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/oauth2/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return BearerToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauth2UserAgent)

	body, status, err := doOAuth2(hc, req)
	if err != nil {
		return BearerToken{}, fmt.Errorf("exchanging the authorization code: %w", err)
	}

	if status != http.StatusOK {
		var e oauth2ErrorBody
		_ = json.Unmarshal(body, &e)
		switch e.Error {
		case "invalid_client":
			return BearerToken{}, errors.New("Instapaper rejected the client credentials: " +
				"check consumer_key and consumer_secret")
		case "unauthorized_client":
			return BearerToken{}, errors.New("Instapaper says this application is suspended")
		case "invalid_grant":
			return BearerToken{}, errors.New("the authorization code was rejected (expired, already used, " +
				"or issued for a different redirect URI): run `samling login` again")
		}
		if e.Error != "" {
			return BearerToken{}, fmt.Errorf("token exchange failed (HTTP %d): %s %s",
				status, e.Error, e.Description)
		}
		return BearerToken{}, fmt.Errorf("token exchange failed: HTTP %d", status)
	}

	var ok struct {
		TokenType   string `json:"token_type"`
		AccessToken string `json:"access_token"`
		User        struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		return BearerToken{}, fmt.Errorf("decoding the token response: %w", err)
	}
	if ok.AccessToken == "" {
		return BearerToken{}, errors.New("the token response had no access_token")
	}
	if ok.TokenType != "" && !strings.EqualFold(ok.TokenType, "bearer") {
		return BearerToken{}, fmt.Errorf("unexpected token type %q", ok.TokenType)
	}
	return BearerToken{AccessToken: ok.AccessToken, UserID: ok.User.ID, Username: ok.User.Username}, nil
}

// VerifyAccessToken checks that a bearer token is accepted, using GET /tags as
// the cheapest authenticated v2 call there is. It returns an error wrapping
// ErrUnauthorized when the token is unknown or revoked.
func VerifyAccessToken(ctx context.Context, hc *http.Client, baseURL, token string) error {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/api/2/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauth2UserAgent)

	body, status, err := doOAuth2(hc, req)
	if err != nil {
		return fmt.Errorf("verifying the token: %w", err)
	}
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w (HTTP 401)", ErrUnauthorized)
	case status == http.StatusForbidden:
		// Not approved, or suspended. Only the message tells which.
		return fmt.Errorf("Instapaper refused this application (HTTP 403): %s", v2ErrorMessage(body))
	default:
		return fmt.Errorf("verifying the token: HTTP %d: %s", status, v2ErrorMessage(body))
	}
}

// v2ErrorMessage pulls the human-readable message out of a v2 error body,
// falling back to the raw text.
func v2ErrorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return truncate(string(body), 200)
}

const oauth2UserAgent = "samling/1.0 (+https://github.com/nilsjesper/samling)"

// doOAuth2 performs a single request. No retries: these run interactively, and
// an authorization code can only be redeemed once, so blindly retrying a POST
// that may have succeeded would only turn success into invalid_grant.
func doOAuth2(hc *http.Client, req *http.Request) ([]byte, int, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}
