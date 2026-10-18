// Package instapaper is a small typed client for the Instapaper API.
//
// Everything samling reads or changes goes through API v2 (see v2.go): JSON
// over GET/POST with a bearer token, real offset pagination, and a `since`
// change feed. Only login still speaks the Full API v1, because its xAuth
// exchange is how samling currently obtains a token -- and that token doubles
// as a v2 bearer token, verified against the live API.
//
// Notes on v1 that shape the code in this file:
//   - Every call is a POST with form-encoded parameters; OAuth 1.0a goes in the
//     Authorization header.
//   - Authentication is xAuth only: username + password -> access token. There
//     is no request-token/authorize browser flow.
//   - Responses are a JSON array of typed objects, except oauth/access_token,
//     which answers in form-encoded text.
//   - A response that is not valid JSON means "503, retry later" per the docs.
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
	"time"

	"github.com/nilsjesper/samling/internal/oauth1"
)

// BaseURL is the API root. Overridable per-client for tests.
const BaseURL = "https://www.instapaper.com"

// Client talks to the Instapaper API.
type Client struct {
	HTTP    *http.Client
	Creds   oauth1.Credentials
	BaseURL string

	// Retries is the number of extra attempts made for retryable failures
	// (network errors, 5xx and 429 responses, and unparseable bodies). Zero
	// means a single attempt.
	Retries int
}

// New returns a client with sensible defaults.
func New(creds oauth1.Credentials) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		Creds:   creds,
		BaseURL: BaseURL,
		Retries: 3,
	}
}

// User is an Instapaper account.
type User struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
}

// ErrUnauthorized reports an HTTP 401. What that means depends on the call: on
// the xAuth endpoint it is a bad username or password, and everywhere else it
// is a token that is no longer good, so callers phrase it themselves.
var ErrUnauthorized = errors.New("unauthorized")

// APIError is an error the API described in its response body. For v1 Code is
// the numeric error_code; for v2 it mirrors the HTTP status.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("instapaper error %d", e.Code)
	}
	return fmt.Sprintf("instapaper error %d: %s", e.Code, e.Message)
}

// AccessToken performs the xAuth exchange. This is the only call that does not
// return JSON: the body is form-encoded, matching OAuth conventions.
func (c *Client) AccessToken(ctx context.Context, username, password string) (token, secret string, err error) {
	body, err := c.raw(ctx, "/api/1/oauth/access_token", url.Values{
		"x_auth_username": {username},
		"x_auth_password": {password},
		"x_auth_mode":     {"client_auth"},
	}, false)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return "", "", fmt.Errorf("Instapaper rejected that username and password.\n" +
				"If you sign in to Instapaper with Google or Apple rather than a password,\n" +
				"set an Instapaper password at https://www.instapaper.com/user first.")
		}
		return "", "", err
	}
	vals, err := url.ParseQuery(strings.TrimSpace(string(body)))
	if err != nil {
		return "", "", fmt.Errorf("parsing access token response: %w", err)
	}
	token, secret = vals.Get("oauth_token"), vals.Get("oauth_token_secret")
	if token == "" || secret == "" {
		return "", "", fmt.Errorf("no access token in response (check your username and password): %s",
			truncate(string(body), 200))
	}
	return token, secret, nil
}

// VerifyCredentials returns the account the current token belongs to.
func (c *Client) VerifyCredentials(ctx context.Context) (User, error) {
	var users []User
	if err := c.call(ctx, "/api/1/account/verify_credentials", nil, &users); err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, fmt.Errorf("verify_credentials returned no user")
	}
	return users[0], nil
}

// call POSTs to an endpoint and decodes the JSON response into out, which may
// be nil when the caller does not care about the body.
func (c *Client) call(ctx context.Context, path string, params url.Values, out any) error {
	body, err := c.raw(ctx, path, params, true)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return fmt.Errorf("%s: token rejected — run `samling login` again", path)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := apiError(body); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decoding response: %w", path, err)
	}
	return nil
}

// raw performs the signed POST with retries, returning the response body.
// wantJSON makes an unparseable body a retryable failure, which per the API
// docs is how a "503, try again later" actually presents itself. The xAuth
// endpoint passes false, since it answers in form-encoded text by design.
func (c *Client) raw(ctx context.Context, path string, params url.Values, wantJSON bool) ([]byte, error) {
	if params == nil {
		params = url.Values{}
	}
	endpoint := strings.TrimSuffix(c.baseURL(), "/") + path
	return c.withRetries(ctx, func() ([]byte, error) {
		return c.attempt(ctx, endpoint, params, wantJSON)
	})
}

// withRetries makes one attempt and repeats it with exponential backoff for as
// long as it fails retryably and the budget lasts. Both API versions go
// through here, so they back off identically.
func (c *Client) withRetries(ctx context.Context, attempt func() ([]byte, error)) ([]byte, error) {
	var lastErr error
	for i := 0; i <= c.Retries; i++ {
		if i > 0 {
			delay := time.Duration(1<<uint(i-1)) * time.Second
			var r *retryableError
			if errors.As(lastErr, &r) && r.after > delay {
				delay = min(r.after, maxRetryAfter)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		body, err := attempt()
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) attempt(ctx context.Context, endpoint string, params url.Values, wantJSON bool) ([]byte, error) {
	auth, err := c.Creds.Authorization(http.MethodPost, endpoint, params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "samling/1.0 (+https://github.com/nilsjesper/samling)")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, &retryableError{err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &retryableError{err: err}
	}

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, &retryableError{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))}
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w (HTTP 401)", ErrUnauthorized)
	case resp.StatusCode >= 400:
		if err := apiError(body); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	if wantJSON && !json.Valid(body) {
		return nil, &retryableError{err: fmt.Errorf("malformed response: %s", truncate(string(body), 200))}
	}
	return body, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return BaseURL
}

// apiError returns a non-nil error if body is an array whose first element is a
// typed error object.
func apiError(body []byte) error {
	trimmed := strings.TrimSpace(string(body))
	if !strings.HasPrefix(trimmed, "[") {
		return nil
	}
	var objs []struct {
		Type      string `json:"type"`
		ErrorCode int    `json:"error_code"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(body, &objs); err != nil {
		return nil // not the error shape; let the caller decode it normally
	}
	for _, o := range objs {
		if o.Type == "error" {
			return &APIError{Code: o.ErrorCode, Message: o.Message}
		}
	}
	return nil
}

// retryableError marks a failure worth trying again. after, when set, is how
// long the server asked us to wait (a 429's Retry-After).
type retryableError struct {
	err   error
	after time.Duration
}

// maxRetryAfter caps how long a Retry-After can stall a sync per attempt.
const maxRetryAfter = time.Minute

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
