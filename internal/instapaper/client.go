// Package instapaper is a small typed client for the Instapaper Full API v1.
//
// Notes on the API that shape this code:
//   - Every call is a POST with form-encoded parameters; OAuth goes in the
//     Authorization header.
//   - Authentication is xAuth only: username + password -> access token. There
//     is no request-token/authorize browser flow.
//   - Most responses are a JSON array of typed objects, but bookmarks/list
//     returns a bare object and oauth/access_token returns form-encoded text.
//   - Field types are inconsistent (starred arrives as "1" or 1), so scalars are
//     decoded through the flexible types at the bottom of this file.
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
	"strconv"
	"strings"
	"time"

	"github.com/nilsjesper/samling/internal/oauth1"
)

// BaseURL is the API root. Overridable per-client for tests.
const BaseURL = "https://www.instapaper.com"

// MaxListLimit is the largest page bookmarks/list will return.
const MaxListLimit = 500

// Client talks to the Instapaper API.
type Client struct {
	HTTP    *http.Client
	Creds   oauth1.Credentials
	BaseURL string

	// Retries is the number of extra attempts made for retryable failures
	// (5xx responses and unparseable bodies). Zero means a single attempt.
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

// Folder is a user-created organizational folder.
type Folder struct {
	FolderID flexString `json:"folder_id"`
	Title    string     `json:"title"`
}

// Bookmark is a saved article. These are the only fields the API returns; note
// in particular that there is no word count, so reading-time filters are not
// possible from bookmarks/list alone.
type Bookmark struct {
	BookmarkID        flexString `json:"bookmark_id"`
	URL               string     `json:"url"`
	Title             string     `json:"title"`
	Description       string     `json:"description"`
	Hash              string     `json:"hash"`
	Time              flexInt    `json:"time"`
	Progress          flexFloat  `json:"progress"`
	ProgressTimestamp flexInt    `json:"progress_timestamp"`
	Starred           flexBool   `json:"starred"`
	PrivateSource     string     `json:"private_source"`
}

// ListResult is the (non-standard) shape returned by bookmarks/list.
type ListResult struct {
	User      User         `json:"user"`
	Bookmarks []Bookmark   `json:"bookmarks"`
	DeleteIDs []flexString `json:"delete_ids"`
}

// APIError is a typed error object returned by the API.
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

// List fetches one page of bookmarks. folder is "unread", "starred", "archive",
// or a numeric folder id. have is the delta-sync parameter: a comma-separated
// list of "id" or "id:hash" entries the caller already holds, which the server
// omits from the response.
func (c *Client) List(ctx context.Context, folder string, limit int, have string) (*ListResult, error) {
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	params := url.Values{
		"limit":     {strconv.Itoa(limit)},
		"folder_id": {folder},
	}
	if have != "" {
		params.Set("have", have)
	}
	var out ListResult
	if err := c.call(ctx, "/api/1/bookmarks/list", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Archive moves a bookmark to the Archive folder. This is not a delete: the
// article stays in the account and remains searchable.
func (c *Client) Archive(ctx context.Context, bookmarkID string) error {
	return c.call(ctx, "/api/1/bookmarks/archive",
		url.Values{"bookmark_id": {bookmarkID}}, nil)
}

// Unarchive moves a bookmark back to the top of Unread.
func (c *Client) Unarchive(ctx context.Context, bookmarkID string) error {
	return c.call(ctx, "/api/1/bookmarks/unarchive",
		url.Values{"bookmark_id": {bookmarkID}}, nil)
}

// Folders lists the account's user-created folders.
func (c *Client) Folders(ctx context.Context) ([]Folder, error) {
	var folders []Folder
	if err := c.call(ctx, "/api/1/folders/list", nil, &folders); err != nil {
		return nil, err
	}
	return folders, nil
}

// call POSTs to an endpoint and decodes the JSON response into out, which may
// be nil when the caller does not care about the body.
func (c *Client) call(ctx context.Context, path string, params url.Values, out any) error {
	body, err := c.raw(ctx, path, params, true)
	if err != nil {
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

	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		body, err := c.attempt(ctx, endpoint, params, wantJSON)
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
		return nil, &retryableError{err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &retryableError{err}
	}

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, &retryableError{fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))}
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("HTTP 401: token rejected, run `samling login` again")
	case resp.StatusCode >= 400:
		if err := apiError(body); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	if wantJSON && !json.Valid(body) {
		return nil, &retryableError{fmt.Errorf("malformed response: %s", truncate(string(body), 200))}
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

type retryableError struct{ err error }

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

// --- flexible scalar decoding -------------------------------------------------
//
// Instapaper is inconsistent about whether numbers and booleans arrive as JSON
// scalars or as strings, so every scalar field goes through one of these.

type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexString(v)
		return nil
	}
	*f = flexString(strings.TrimSuffix(s, ".0"))
	return nil
}

func (f flexString) String() string { return string(f) }

type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("expected a number, got %s", string(b))
	}
	*f = flexInt(int64(v))
	return nil
}

func (f flexInt) Int64() int64 { return int64(f) }

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("expected a number, got %s", string(b))
	}
	*f = flexFloat(v)
	return nil
}

func (f flexFloat) Float64() float64 { return float64(f) }

type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch s := strings.Trim(string(b), `"`); s {
	case "", "null", "0", "false", "0.0":
		*f = false
	default:
		*f = true
	}
	return nil
}

func (f flexBool) Bool() bool { return bool(f) }
