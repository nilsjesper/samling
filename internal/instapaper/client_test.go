package instapaper

import (
	"context"
	"encoding/json"
	"errors"
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

// Instapaper is inconsistent about scalar types; every field must survive both
// the JSON-native and the stringly-typed form.
func TestListDecodesInconsistentScalarTypes(t *testing.T) {
	body := `{
      "user": {"user_id": 54321, "username": "TestUser"},
      "bookmarks": [
        {"type":"bookmark","bookmark_id":1234,"url":"https://a.com/1","title":"Numeric",
         "hash":"abc","time":1690000000,"progress":0.5,"starred":"1","description":""},
        {"type":"bookmark","bookmark_id":"5678","url":"https://b.com/2","title":"Stringy",
         "hash":"def","time":"1690000001","progress":"0","starred":0,"private_source":""}
      ],
      "delete_ids": [42, "43"]
    }`
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if got := r.PostForm.Get("limit"); got != "500" {
			t.Errorf("limit = %q, want 500", got)
		}
		if got := r.PostForm.Get("folder_id"); got != "unread" {
			t.Errorf("folder_id = %q, want unread", got)
		}
		if got := r.PostForm.Get("have"); got != "1:abc" {
			t.Errorf("have = %q, want 1:abc", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	res, err := c.List(context.Background(), "unread", 500, "1:abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.User.Username != "TestUser" {
		t.Errorf("username = %q", res.User.Username)
	}
	if len(res.Bookmarks) != 2 {
		t.Fatalf("got %d bookmarks, want 2", len(res.Bookmarks))
	}

	a, b := res.Bookmarks[0], res.Bookmarks[1]
	if a.BookmarkID.String() != "1234" || b.BookmarkID.String() != "5678" {
		t.Errorf("ids = %q/%q, want 1234/5678", a.BookmarkID, b.BookmarkID)
	}
	if a.Time.Int64() != 1690000000 || b.Time.Int64() != 1690000001 {
		t.Errorf("times = %d/%d", a.Time.Int64(), b.Time.Int64())
	}
	if a.Progress.Float64() != 0.5 || b.Progress.Float64() != 0 {
		t.Errorf("progress = %v/%v", a.Progress.Float64(), b.Progress.Float64())
	}
	if !a.Starred.Bool() || b.Starred.Bool() {
		t.Errorf("starred = %v/%v, want true/false", a.Starred.Bool(), b.Starred.Bool())
	}
	if len(res.DeleteIDs) != 2 || res.DeleteIDs[0].String() != "42" || res.DeleteIDs[1].String() != "43" {
		t.Errorf("delete_ids = %v", res.DeleteIDs)
	}
}

func TestTypedAPIErrorIsSurfaced(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"type":"error","error_code":1241,"message":"Invalid bookmark"}]`))
	})
	err := c.Archive(context.Background(), "999")
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != 1241 {
		t.Errorf("code = %d, want 1241", apiErr.Code)
	}
}

// A non-JSON body means "503, retry later" per the API docs.
func TestMalformedBodyIsRetried(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.Write([]byte("<html>maintenance</html>"))
			return
		}
		w.Write([]byte(`[]`))
	})
	c.Retries = 3
	c.HTTP = &http.Client{Timeout: 30 * time.Second}

	if err := c.Archive(context.Background(), "1"); err != nil {
		t.Fatalf("Archive should have succeeded on retry: %v", err)
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

	if err := c.Archive(context.Background(), "1"); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if calls != 2 {
		t.Errorf("made %d calls, want 2 (initial + 1 retry)", calls)
	}
}

// 4xx responses are the client's fault; retrying them just wastes time.
func TestClientErrorIsNotRetried(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "nope", http.StatusUnauthorized)
	})
	if err := c.Archive(context.Background(), "1"); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("made %d calls, want 1", calls)
	}
}

// A 401 means different things on different endpoints, and the message has to
// match: bad username/password during login, stale token everywhere else.
func TestUnauthorizedIsPhrasedPerEndpoint(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

	err = c.Archive(context.Background(), "1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "samling login") {
		t.Errorf("non-login error should point at re-authenticating, got: %v", err)
	}
	if !strings.Contains(err.Error(), "token rejected") {
		t.Errorf("unexpected message: %v", err)
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

func TestFolders(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"type":"folder","folder_id":1234567,"title":"Long Reads"}]`))
	})
	fs, err := c.Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(fs) != 1 || fs[0].FolderID.String() != "1234567" || fs[0].Title != "Long Reads" {
		t.Errorf("got %+v", fs)
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
	if err := c.Archive(ctx, "1"); err == nil {
		t.Fatal("expected an error")
	}
	// Without cancellation the backoff alone would take 1+2+4+8+16 seconds.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v; cancellation did not interrupt the backoff", elapsed)
	}
}
