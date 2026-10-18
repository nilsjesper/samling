package instapaper

// API v2: everything samling does after login.
//
// Notes on v2 that shape this code, from the reference docs and confirmed
// read-only against the live API:
//   - Reads are GETs with query parameters, writes are POSTs with JSON bodies,
//     and the access token goes in an `Authorization: Bearer` header. The
//     oauth_token obtained through v1 xAuth works unchanged; its secret is not
//     used and nothing is signed.
//   - GET /bookmarks pages with `offset`/`limit` (limit at most 500) and reports
//     `total`, the size of the whole section. v1 could never see past the newest
//     500 of a folder; this can see all of it.
//   - GET /bookmarks?since=T is an account-wide change feed: every bookmark
//     modified since T in any section -- archived ones included, with
//     archived:true -- plus `deleted_ids`. `section` is ignored in this mode.
//     Bookmarks and deleted ids share each page's `limit`, so advance `offset`
//     by both and stop on a short page. A tag-only change shows up here too,
//     with `time` unchanged.
//   - A bookmark's v2 `id` is the same number as its v1 `bookmark_id`.
//   - Failures use real HTTP statuses with {"error":{"code","message"}}; 429
//     means back off, and 401 means the token is missing, unknown, or revoked.

import (
	"bytes"
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
)

// MaxPageSize is the largest `limit` GET /api/2/bookmarks accepts.
const MaxPageSize = 500

// Section names accepted by List and Move alongside numeric folder ids.
const (
	SectionHome    = "home"
	SectionArchive = "archive"
)

// Bookmark is a saved article as v2 reports it. Nullable strings decode as "".
type Bookmark struct {
	ID          int64    `json:"id"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Progress    Progress `json:"progress"`
	Liked       bool     `json:"liked"`
	Archived    bool     `json:"archived"`
	Time        int64    `json:"time"`
	FolderID    *int64   `json:"folder_id"` // nil when it's in the home list
	Tags        []Tag    `json:"tags"`
	Category    int      `json:"category"`
}

// Progress is how far into an article the reader got, as a 0-1 fraction.
type Progress struct {
	Percentage float64 `json:"percentage"`
	Timestamp  int64   `json:"timestamp"`
}

// Tag is a user label. A bookmark can carry several, unlike folders.
type Tag struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Count int    `json:"count"`
}

// Folder is a user-created folder. Home, archive and liked are sections, not
// folders, and never appear here.
type Folder struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Count int    `json:"count"`
}

// Page is one response from GET /bookmarks. DeletedIDs is only ever present
// in `since` mode, and there Total counts bookmarks and deletions together.
type Page struct {
	Bookmarks  []Bookmark `json:"bookmarks"`
	DeletedIDs []int64    `json:"deleted_ids"`
	Total      int        `json:"total"`
}

// ProgressFunc reports paging progress: entries received so far, and the total
// the server last claimed.
type ProgressFunc func(got, total int)

// List reads one page of a section: "home", "archive", "liked", or a numeric
// folder id.
func (c *Client) List(ctx context.Context, section string, offset, limit int) (*Page, error) {
	q := pageQuery(offset, limit)
	if isFolderID(section) {
		q.Set("section", "folder")
		q.Set("folder_id", section)
	} else {
		q.Set("section", section)
	}
	var p Page
	if err := c.get2(ctx, "/api/2/bookmarks", q, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Since reads one page of the change feed: everything modified after the unix
// time since, across the whole account.
func (c *Client) Since(ctx context.Context, since int64, offset, limit int) (*Page, error) {
	if since < 1 {
		since = 1 // the API rejects 0
	}
	q := pageQuery(offset, limit)
	q.Set("since", strconv.FormatInt(since, 10))
	var p Page
	if err := c.get2(ctx, "/api/2/bookmarks", q, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListAll pages through an entire section.
//
// Offset paging over a live list is not a snapshot. An article saved during
// the walk pushes everything down one place, so one item is seen twice (and
// deduplicated by the caller); an article removed from the section pulls
// everything up one place, so one item is skipped. A walk of 24,000 items
// takes about 48 requests, so the window for that is small.
func (c *Client) ListAll(ctx context.Context, section string, progress ProgressFunc) ([]Bookmark, error) {
	var all []Bookmark
	for offset := 0; ; {
		p, err := c.List(ctx, section, offset, MaxPageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, p.Bookmarks...)
		offset += len(p.Bookmarks)
		if progress != nil {
			progress(offset, p.Total)
		}
		if len(p.Bookmarks) < MaxPageSize || offset >= p.Total {
			return all, nil
		}
	}
}

// SinceAll reads the whole change feed since a unix time.
func (c *Client) SinceAll(ctx context.Context, since int64, progress ProgressFunc) (*Page, error) {
	all := &Page{}
	for offset := 0; ; {
		p, err := c.Since(ctx, since, offset, MaxPageSize)
		if err != nil {
			return nil, err
		}
		all.Bookmarks = append(all.Bookmarks, p.Bookmarks...)
		all.DeletedIDs = append(all.DeletedIDs, p.DeletedIDs...)
		all.Total = p.Total
		// Changed bookmarks and deleted ids share each page's limit.
		n := len(p.Bookmarks) + len(p.DeletedIDs)
		offset += n
		if progress != nil {
			progress(offset, p.Total)
		}
		if n < MaxPageSize || offset >= p.Total {
			return all, nil
		}
	}
}

// Move puts a bookmark in "archive", "home", or a numeric folder id. Archiving
// is not a delete: the article stays in the account and remains searchable.
func (c *Client) Move(ctx context.Context, id, section string) (Bookmark, error) {
	var b Bookmark
	err := c.post2(ctx, "/api/2/bookmarks/"+url.PathEscape(id)+"/move",
		map[string]string{"section": section}, &b)
	return b, err
}

// Tags lists every tag on the account. Tags with no bookmarks left are
// included: v2 has no endpoint for deleting a tag, so they linger until removed
// in the web app.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	var out struct {
		Tags []Tag `json:"tags"`
	}
	if err := c.get2(ctx, "/api/2/tags", nil, &out); err != nil {
		return nil, err
	}
	return out.Tags, nil
}

// TagBookmark adds tags by name and removes tags by id in a single request,
// and returns the bookmark's full tag list afterwards. A name that does not
// exist yet is created. Removal takes ids only; the API has no by-name form.
//
// Verified live: this changes neither the bookmark's `time` nor its place in
// its list, so it never looks like a re-save. It does surface in the next
// `since` feed, as a change with `time` unchanged. Repeating the same request
// leaves the same result, which is what makes the transport's retries safe.
func (c *Client) TagBookmark(ctx context.Context, id string, add []string, remove []int64) ([]Tag, error) {
	type ref struct {
		ID   int64  `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	}
	var body struct {
		Add    []ref `json:"add_tags,omitempty"`
		Remove []ref `json:"remove_tags,omitempty"`
	}
	for _, n := range add {
		body.Add = append(body.Add, ref{Name: n})
	}
	for _, r := range remove {
		body.Remove = append(body.Remove, ref{ID: r})
	}
	var out struct {
		Tags []Tag `json:"tags"`
	}
	err := c.post2(ctx, "/api/2/bookmarks/"+url.PathEscape(id)+"/tags", body, &out)
	return out.Tags, err
}

// Folders lists the account's user-created folders, in the user's own order.
func (c *Client) Folders(ctx context.Context) ([]Folder, error) {
	var out struct {
		Folders []Folder `json:"folders"`
	}
	if err := c.get2(ctx, "/api/2/folders", nil, &out); err != nil {
		return nil, err
	}
	return out.Folders, nil
}

func pageQuery(offset, limit int) url.Values {
	if limit <= 0 || limit > MaxPageSize {
		limit = MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return url.Values{
		"limit":  {strconv.Itoa(limit)},
		"offset": {strconv.Itoa(offset)},
	}
}

func isFolderID(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// --- transport ------------------------------------------------------------------

func (c *Client) get2(ctx context.Context, path string, query url.Values, out any) error {
	return c.do2(ctx, http.MethodGet, path, query, nil, out)
}

func (c *Client) post2(ctx context.Context, path string, body, out any) error {
	return c.do2(ctx, http.MethodPost, path, nil, body, out)
}

// do2 makes a v2 request with retries and decodes the JSON response into out,
// which may be nil.
func (c *Client) do2(ctx context.Context, method, path string, query url.Values, in, out any) error {
	endpoint := strings.TrimSuffix(c.baseURL(), "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("%s: encoding request: %w", path, err)
		}
	}

	body, err := c.withRetries(ctx, func() ([]byte, error) {
		return c.attempt2(ctx, method, endpoint, payload)
	})
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return fmt.Errorf("%s: token rejected — run `samling login` again", path)
		}
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

func (c *Client) attempt2(ctx context.Context, method, endpoint string, payload []byte) ([]byte, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Creds.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "samling/1.0 (+https://github.com/nilsjesper/samling)")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, &retryableError{err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &retryableError{err: err}
	}

	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests:
		return nil, &retryableError{
			err:   statusError(code, body),
			after: retryAfter(resp.Header.Get("Retry-After")),
		}
	case code >= 500:
		return nil, &retryableError{err: statusError(code, body)}
	case code == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w (HTTP 401)", ErrUnauthorized)
	case code >= 400:
		return nil, statusError(code, body)
	}

	// An empty body is a legitimate success (DELETE answers 200 with nothing);
	// anything else must be JSON, or it is the HTML of a maintenance page.
	if len(bytes.TrimSpace(body)) > 0 && !json.Valid(body) {
		return nil, &retryableError{err: fmt.Errorf("malformed response: %s", truncate(string(body), 200))}
	}
	return body, nil
}

// statusError turns a v2 failure into an error. The message comes from the
// {"error":{"code","message"}} body when there is one; the status is what to
// branch on, since the docs warn that message text may be reworded.
func statusError(status int, body []byte) error {
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return &APIError{Code: status, Message: e.Error.Message}
	}
	return &APIError{Code: status, Message: truncate(string(body), 200)}
}

// retryAfter reads a Retry-After header given in seconds. The HTTP-date form is
// not worth supporting for a CLI; it falls back to ordinary backoff.
func retryAfter(h string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
