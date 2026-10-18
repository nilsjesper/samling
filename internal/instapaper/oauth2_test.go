package instapaper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newOAuth2Server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestAuthorizeURL(t *testing.T) {
	got := AuthorizeURL("https://www.instapaper.com/", "ck", "http://127.0.0.1:8919/callback", "s1")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://www.instapaper.com/oauth2/authorize" {
		t.Errorf("endpoint = %s", got)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":     "ck",
		"redirect_uri":  "http://127.0.0.1:8919/callback",
		"response_type": "code",
		"state":         "s1",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func TestExchangeCodeSendsFormAndParsesToken(t *testing.T) {
	srv := newOAuth2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth2/token" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", ct)
		}
		r.ParseForm()
		for k, want := range map[string]string{
			"client_id":     "ck",
			"client_secret": "cs",
			"redirect_uri":  "http://127.0.0.1:8919/callback",
			"code":          "1a2b3c4d",
		} {
			if got := r.PostForm.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token_type":"Bearer","access_token":"aabbccdd11223344",
			"user":{"id":42,"username":"reader@example.com"}}`))
	})

	bt, err := ExchangeCode(context.Background(), srv.Client(), srv.URL,
		"ck", "cs", "http://127.0.0.1:8919/callback", "1a2b3c4d")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	want := BearerToken{AccessToken: "aabbccdd11223344", UserID: 42, Username: "reader@example.com"}
	if bt != want {
		t.Errorf("got %+v, want %+v", bt, want)
	}
}

func TestExchangeCodeErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"invalid grant", 400, `{"error":"invalid_grant","error_description":"The authorization code is invalid, expired, or already used."}`, "authorization code was rejected"},
		{"invalid client", 401, `{"error":"invalid_client"}`, "client credentials"},
		{"suspended", 403, `{"error":"unauthorized_client"}`, "suspended"},
		{"unknown", 400, `{"error":"weird","error_description":"huh"}`, "weird huh"},
		{"no body", 502, ``, "HTTP 502"},
		{"no token", 200, `{"token_type":"Bearer"}`, "no access_token"},
		{"wrong type", 200, `{"token_type":"mac","access_token":"x"}`, "unexpected token type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOAuth2Server(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			_, err := ExchangeCode(context.Background(), srv.Client(), srv.URL, "ck", "cs", "http://x/callback", "c")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestExchangeCodeIsNotRetried(t *testing.T) {
	// A code can be redeemed once; a retry after a lost response would only
	// produce invalid_grant, so a server error must surface immediately.
	calls := 0
	srv := newOAuth2Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := ExchangeCode(context.Background(), srv.Client(), srv.URL, "ck", "cs", "http://x/callback", "c"); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("token endpoint called %d times, want 1", calls)
	}
}

func TestVerifyAccessTokenSendsBearer(t *testing.T) {
	srv := newOAuth2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/2/tags" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("Authorization = %q", got)
		}
		w.Write([]byte(`{"tags":[]}`))
	})
	if err := VerifyAccessToken(context.Background(), srv.Client(), srv.URL, "tok123"); err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
}

func TestVerifyAccessTokenRejected(t *testing.T) {
	srv := newOAuth2Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"Authentication failed"}}`))
	})
	err := VerifyAccessToken(context.Background(), srv.Client(), srv.URL, "bad")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestVerifyAccessTokenForbiddenShowsMessage(t *testing.T) {
	srv := newOAuth2Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"Application is suspended"}}`))
	})
	err := VerifyAccessToken(context.Background(), srv.Client(), srv.URL, "t")
	if err == nil || errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "Application is suspended") {
		t.Fatalf("err = %v", err)
	}
}
