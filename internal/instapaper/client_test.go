package instapaper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nilsjesper/samling/internal/oauth1"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(oauth1.Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", Token: "tk", TokenSecret: "ts"})
	c.BaseURL = srv.URL
	c.Retries = 2
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	return c
}

func archive(c *Client) error {
	_, err := c.Move(context.Background(), "1", SectionArchive)
	return err
}

// --- v1: login only ---------------------------------------------------------------

func TestAccessTokenParsesFormEncodedBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "OAuth ") {
			t.Errorf("missing OAuth header, got %q", got)
		}
		r.ParseForm()
		if got := r.PostForm.Get("x_auth_mode"); got != "client_auth" {
			t.Errorf("x_auth_mode = %q, want client_auth", got)
		}
		if got := r.PostForm.Get("x_auth_username"); got != "me@example.com" {
			t.Errorf("x_auth_username = %q", got)
		}
		w.Write([]byte("oauth_token=aabbccdd&oauth_token_secret=efgh1234"))
	})

	token, secret, err := c.AccessToken(context.Background(), "me@example.com", "hunter2")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if token != "aabbccdd" || secret != "efgh1234" {
		t.Errorf("got %q/%q, want aabbccdd/efgh1234", token, secret)
	}
}

func TestAccessTokenRejectsEmptyResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(""))
	})
	if _, _, err := c.AccessToken(context.Background(), "u", "p"); err == nil {
		t.Fatal("expected an error when no token comes back")
	}
}

func TestVerifyCredentials(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"type": "user", "user_id": 54321, "username": "TestUserOMGLOL"},
		})
	})
	u, err := c.VerifyCredentials(context.Background())
	if err != nil {
		t.Fatalf("VerifyCredentials: %v", err)
	}
	if u.UserID != 54321 || u.Username != "TestUserOMGLOL" {
		t.Errorf("got %+v", u)
	}
}

// v1 reports failures as HTTP 200 with a typed error object in the body.
func TestV1TypedAPIErrorIsSurfaced(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"type":"error","error_code":1040,"message":"Rate-limit exceeded"}]`))
	})
	_, err := c.VerifyCredentials(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 1040 {
		t.Fatalf("expected *APIError 1040, got %T: %v", err, err)
	}
}

// --- v2 -----------------------------------------------------------------------------

// The v1 oauth_token is sent as a plain bearer token: no signature, no secret.
func TestV2SendsBearerToken(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tk" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer tk")
		}
		w.Write([]byte(`{"bookmarks":[],"total":0}`))
	})
	if _, err := c.List(context.Background(), SectionHome, 0, 10); err != nil {
		t.Fatalf("List: %v", err)
	}
}

// The response below is the shape of a real since-feed answer, nulls included.
func TestV2ListDecodesBookmarks(t *testing.T) {
	body := `{"bookmarks":[
	  {"id":1989761742,"url":"https://loopmaster.xyz/","title":"loopmaster - Code music",
	   "progress":{"percentage":0.0,"timestamp":0},"liked":false,"archived":false,
	   "time":1773238168,"pubtime":null,"author":null,"folder_id":null,"tags":[],
	   "private_source":null,"category":0},
	  {"id":7,"url":null,"title":null,"description":null,
	   "progress":{"percentage":0.5,"timestamp":1773000000},"liked":true,"archived":false,
	   "time":1773000001,"folder_id":99,"tags":[{"id":9,"name":"Recipes","slug":"recipes","count":12,"baton":null}],
	   "category":3}
	],"total":287}`
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/2/bookmarks" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("section") != "home" || q.Get("offset") != "1000" || q.Get("limit") != "500" {
			t.Errorf("query = %v", q)
		}
		w.Write([]byte(body))
	})

	p, err := c.List(context.Background(), SectionHome, 1000, 500)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if p.Total != 287 || len(p.Bookmarks) != 2 {
		t.Fatalf("total %d, %d bookmarks", p.Total, len(p.Bookmarks))
	}
	a, b := p.Bookmarks[0], p.Bookmarks[1]
	if a.ID != 1989761742 || a.Time != 1773238168 || a.FolderID != nil || a.Liked {
		t.Errorf("first = %+v", a)
	}
	if b.URL != "" || b.Title != "" || !b.Liked || b.Progress.Percentage != 0.5 {
		t.Errorf("second = %+v", b)
	}
	if b.FolderID == nil || *b.FolderID != 99 {
		t.Errorf("folder_id = %v, want 99", b.FolderID)
	}
	if len(b.Tags) != 1 || b.Tags[0].Name != "Recipes" || b.Tags[0].Count != 12 {
		t.Errorf("tags = %+v", b.Tags)
	}
}

func TestV2ListFolderUsesFolderSection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("section") != "folder" || q.Get("folder_id") != "1234567" {
			t.Errorf("query = %v", q)
		}
		w.Write([]byte(`{"bookmarks":[],"total":0}`))
	})
	if _, err := c.List(context.Background(), "1234567", 0, 0); err != nil {
		t.Fatalf("List: %v", err)
	}
}

// Bookmarks and deleted ids share each page's limit, so offset advances by
// both, and a page that is short in total ends the walk.
func TestV2SinceAllAdvancesByBookmarksPlusDeletions(t *testing.T) {
	var offsets []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("since") != "1755000000" {
			t.Errorf("since = %q", q.Get("since"))
		}
		offsets = append(offsets, q.Get("offset"))
		page := map[string]any{"total": 700}
		switch q.Get("offset") {
		case "0": // 400 bookmarks + 100 deletions fill a page of 500
			bms := make([]map[string]any, 400)
			for i := range bms {
				bms[i] = map[string]any{"id": i + 1}
			}
			dels := make([]int, 100)
			for i := range dels {
				dels[i] = 10000 + i
			}
			page["bookmarks"], page["deleted_ids"] = bms, dels
		case "500":
			page["bookmarks"] = []map[string]any{{"id": 501}}
			page["deleted_ids"] = []int{20000}
		default:
			t.Errorf("unexpected offset %q", q.Get("offset"))
		}
		json.NewEncoder(w).Encode(page)
	})

	var lastGot int
	p, err := c.SinceAll(context.Background(), 1755000000, func(got, total int) { lastGot = got })
	if err != nil {
		t.Fatalf("SinceAll: %v", err)
	}
	if strings.Join(offsets, ",") != "0,500" {
		t.Errorf("offsets = %v, want 0,500", offsets)
	}
	if len(p.Bookmarks) != 401 || len(p.DeletedIDs) != 101 {
		t.Errorf("got %d bookmarks and %d deletions, want 401 and 101", len(p.Bookmarks), len(p.DeletedIDs))
	}
	if lastGot != 502 {
		t.Errorf("progress ended at %d, want 502", lastGot)
	}
}

func TestV2MoveSendsJSONBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/2/bookmarks/42/move" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		b, _ := io.ReadAll(r.Body)
		var got map[string]string
		if err := json.Unmarshal(b, &got); err != nil || got["section"] != "archive" || len(got) != 1 {
			t.Errorf("body = %s", b)
		}
		w.Write([]byte(`{"id":42,"archived":true}`))
	})
	b, err := c.Move(context.Background(), "42", SectionArchive)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if b.ID != 42 || !b.Archived {
		t.Errorf("returned %+v", b)
	}
}

// The swap the live API accepted: add by name, remove by id, in one body.
func TestV2TagBookmarkSwapsInOneRequest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/2/bookmarks/42/tags" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if want := `{"add_tags":[{"name":"samling3"}],"remove_tags":[{"id":4220559}]}`; string(b) != want {
			t.Errorf("body = %s, want %s", b, want)
		}
		w.Write([]byte(`{"created_tags":[{"id":4220560,"name":"samling3","slug":"samling3","count":1}],` +
			`"tags":[{"id":7,"name":"ifttt","slug":"ifttt","count":15},{"id":4220560,"name":"samling3","slug":"samling3","count":1}]}`))
	})
	tags, err := c.TagBookmark(context.Background(), "42", []string{"samling3"}, []int64{4220559})
	if err != nil {
		t.Fatalf("TagBookmark: %v", err)
	}
	if len(tags) != 2 || tags[0].Name != "ifttt" || tags[1].Name != "samling3" || tags[1].ID != 4220560 {
		t.Errorf("returned %+v, want the full tag list afterwards", tags)
	}
}

// With nothing to remove, remove_tags is left out rather than sent empty.
func TestV2TagBookmarkOmitsAnEmptySide(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if want := `{"add_tags":[{"name":"samling1"}]}`; string(b) != want {
			t.Errorf("body = %s, want %s", b, want)
		}
		w.Write([]byte(`{"created_tags":[],"tags":[{"id":1,"name":"samling1","slug":"samling1","count":3}]}`))
	})
	if _, err := c.TagBookmark(context.Background(), "42", []string{"samling1"}, nil); err != nil {
		t.Fatalf("TagBookmark: %v", err)
	}
}

func TestV2Tags(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/2/tags" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"tags":[{"id":4220559,"name":"samling2","slug":"samling2","count":4,"baton":null}]}`))
	})
	tags, err := c.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 1 || tags[0].ID != 4220559 || tags[0].Name != "samling2" || tags[0].Count != 4 {
		t.Errorf("got %+v", tags)
	}
}

func TestV2Folders(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/2/folders" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"folders":[{"id":1234567,"title":"Long Reads","slug":"long-reads","position":1,"public":false,"count":12}]}`))
	})
	fs, err := c.Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(fs) != 1 || fs[0].ID != 1234567 || fs[0].Title != "Long Reads" || fs[0].Count != 12 {
		t.Errorf("got %+v", fs)
	}
}

// v2 reports failures with the real status and an {"error":{...}} body.
func TestV2ErrorBodyIsParsed(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":400,"message":"Invalid or missing bookmark_id"}}`))
	})
	err := archive(c)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != 400 || apiErr.Message != "Invalid or missing bookmark_id" {
		t.Errorf("got %+v", apiErr)
	}
	if calls != 1 {
		t.Errorf("a 400 was retried: %d calls", calls)
	}
}

func TestV2RateLimitIsRetried(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":429,"message":"Rate limit exceeded"}}`))
			return
		}
		w.Write([]byte(`{"id":1}`))
	})
	if err := archive(c); err != nil {
		t.Fatalf("should have succeeded after the 429: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d calls, want 2", calls)
	}
}

func TestRetryAfterParsesSeconds(t *testing.T) {
	if got := retryAfter("3"); got != 3*time.Second {
		t.Errorf("retryAfter(3) = %v", got)
	}
	if got := retryAfter("Wed, 21 Oct 2015 07:28:00 GMT"); got != 0 {
		t.Errorf("HTTP-date form should fall back to backoff, got %v", got)
	}
}

// A non-JSON body is a maintenance page, not an answer.
func TestMalformedBodyIsRetried(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.Write([]byte("<html>maintenance</html>"))
			return
		}
		w.Write([]byte(`{"id":1}`))
	})
	c.Retries = 3
	c.HTTP = &http.Client{Timeout: 30 * time.Second}

	if err := archive(c); err != nil {
		t.Fatalf("Move should have succeeded on retry: %v", err)
	}
	if calls != 3 {
		t.Errorf("made %d calls, want 3", calls)
	}
}

func TestServerErrorIsRetriedThenGivesUp(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c.Retries = 1

	if err := archive(c); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if calls != 2 {
		t.Errorf("made %d calls, want 2 (initial + 1 retry)", calls)
	}
}

// A 401 means different things on different endpoints, and the message has to
// match: bad username/password during login, stale token everywhere else. And
// it is never retried.
func TestUnauthorizedIsPhrasedPerEndpoint(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":{"code":401,"message":"Authentication failed"}}`, http.StatusUnauthorized)
	})

	_, _, err := c.AccessToken(context.Background(), "u", "wrong")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "username and password") {
		t.Errorf("login error should blame the credentials, got: %v", err)
	}
	if strings.Contains(err.Error(), "samling login") {
		t.Errorf("login error should not tell the user to run login again, got: %v", err)
	}

	calls = 0
	err = archive(c)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "samling login") || !strings.Contains(err.Error(), "token rejected") {
		t.Errorf("v2 401 should point at re-authenticating, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("a 401 was retried: %d calls", calls)
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c.Retries = 5

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Move(ctx, "1", SectionArchive); err == nil {
		t.Fatal("expected an error")
	}
	// Without cancellation the backoff alone would take 1+2+4+8+16 seconds.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v; cancellation did not interrupt the backoff", elapsed)
	}
}
