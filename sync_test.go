package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nilsjesper/samling/internal/instapaper"
	"github.com/nilsjesper/samling/internal/library"
	"github.com/nilsjesper/samling/internal/oauth1"
)

// fakeInstapaper models bookmarks/list closely enough to exercise the drain
// loop: it honours the "have" parameter as an exclusion filter and pages at a
// fixed limit.
type fakeInstapaper struct {
	mu       sync.Mutex
	order    []string        // folder contents, in order
	gone     map[string]bool // removed from the folder since the client last looked
	limit    int
	failIDs  map[string]bool // archive requests that should fail
	archived []string

	// hostile makes the server report every id in "have" as deleted whenever it
	// returns a full page — the pessimistic reading of the delete_ids docs.
	// The client must not act on that.
	hostile bool

	listCalls, archiveCalls int
}

func (f *fakeInstapaper) known(id string) bool {
	for _, o := range f.order {
		if o == id {
			return !f.gone[id]
		}
	}
	return false
}

func (f *fakeInstapaper) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()

		switch r.URL.Path {
		case "/api/1/bookmarks/list":
			f.listCalls++
			have := map[string]bool{}
			if v := r.PostForm.Get("have"); v != "" {
				for _, part := range strings.Split(v, ",") {
					have[strings.SplitN(part, ":", 2)[0]] = true
				}
			}

			var page []map[string]any
			for _, id := range f.order {
				if f.gone[id] || have[id] {
					continue
				}
				page = append(page, map[string]any{
					"type": "bookmark", "bookmark_id": id,
					"url": "https://example.com/" + id, "title": "Article " + id,
					"hash": "h" + id, "time": 1690000000, "starred": "0",
				})
				if len(page) == f.limit {
					break
				}
			}

			deleteIDs := []string{}
			if f.hostile && len(page) == f.limit {
				for id := range have {
					deleteIDs = append(deleteIDs, id)
				}
			} else {
				for id := range have {
					if !f.known(id) {
						deleteIDs = append(deleteIDs, id)
					}
				}
			}

			json.NewEncoder(w).Encode(map[string]any{
				"user":       map[string]any{"user_id": 1, "username": "t"},
				"bookmarks":  page,
				"delete_ids": deleteIDs,
			})

		case "/api/1/bookmarks/archive":
			f.archiveCalls++
			id := r.PostForm.Get("bookmark_id")
			if f.failIDs[id] {
				w.Write([]byte(`[{"type":"error","error_code":1241,"message":"nope"}]`))
				return
			}
			f.archived = append(f.archived, id)
			f.gone[id] = true
			w.Write([]byte(`[]`))

		default:
			http.NotFound(w, r)
		}
	}
}

func newFake(t *testing.T, f *fakeInstapaper) *instapaper.Client {
	t.Helper()
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	if f.failIDs == nil {
		f.failIDs = map[string]bool{}
	}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	c := instapaper.New(oauth1.Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", Token: "t", TokenSecret: "s"})
	c.BaseURL = srv.URL
	c.Retries = 0
	c.HTTP = &http.Client{Timeout: 10 * time.Second}
	return c
}

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(i + 1)
	}
	return out
}

// A backlog bigger than one page must be drained by repeatedly re-asking with a
// growing "have" list, since bookmarks/list has no offset parameter.
func TestDrainPagesThroughAFolderLargerThanOnePage(t *testing.T) {
	fake := &fakeInstapaper{order: ids(1200), limit: 500}
	client := newFake(t, fake)
	lib := library.New()

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(lib.Bookmarks) != 1200 {
		t.Errorf("collected %d bookmarks, want 1200", len(lib.Bookmarks))
	}
	if fake.listCalls != 3 { // 500 + 500 + 200
		t.Errorf("made %d list calls, want 3", fake.listCalls)
	}
	for _, id := range []string{"1", "500", "501", "1200"} {
		if _, ok := lib.Bookmarks[id]; !ok {
			t.Errorf("bookmark %s missing from the mirror", id)
		}
	}
	if b := lib.Bookmarks["7"]; b.Hash != "h7" || b.Folder != "unread" {
		t.Errorf("bookmark 7 = %+v, want hash h7 and folder unread", b)
	}
}

// An exactly-full final page still terminates: the follow-up call returns
// nothing new and the loop stops.
func TestDrainHandlesExactMultipleOfPageSize(t *testing.T) {
	fake := &fakeInstapaper{order: ids(1000), limit: 500}
	client := newFake(t, fake)
	lib := library.New()

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(lib.Bookmarks) != 1000 {
		t.Errorf("collected %d bookmarks, want 1000", len(lib.Bookmarks))
	}
}

// The load-bearing safety rule. delete_ids on a full page may just mean
// "further down the folder", so acting on it mid-drain would evict live
// bookmarks. Against a server that reports exactly that, nothing may be lost.
func TestDrainIgnoresDeleteIDsOnFullPages(t *testing.T) {
	fake := &fakeInstapaper{order: ids(1200), limit: 500, hostile: true}
	client := newFake(t, fake)
	lib := library.New()

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(lib.Bookmarks) != 1200 {
		t.Fatalf("collected %d bookmarks, want 1200 — spurious delete_ids were honoured", len(lib.Bookmarks))
	}
}

// On a short final page the server has covered the whole folder, so delete_ids
// is meaningful and articles removed elsewhere should disappear locally.
func TestDrainHonoursDeleteIDsOnTheFinalPage(t *testing.T) {
	fake := &fakeInstapaper{order: ids(10), limit: 500, gone: map[string]bool{"3": true}}
	client := newFake(t, fake)

	lib := library.New()
	// The client still believes 3 is unread, and 99 was never on the server.
	lib.Bookmarks["3"] = library.Bookmark{ID: "3", Hash: "h3"}
	lib.Bookmarks["99"] = library.Bookmark{ID: "99", Hash: "h99"}

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, ok := lib.Bookmarks["3"]; ok {
		t.Error("bookmark 3 was removed remotely but survived locally")
	}
	if _, ok := lib.Bookmarks["99"]; ok {
		t.Error("bookmark 99 does not exist remotely but survived locally")
	}
	if len(lib.Bookmarks) != 9 {
		t.Errorf("have %d bookmarks, want 9", len(lib.Bookmarks))
	}
}

// Articles already archived must not be pulled back in, even if the server
// still lists them.
func TestDrainSkipsTombstonedArticles(t *testing.T) {
	fake := &fakeInstapaper{order: ids(5), limit: 500}
	client := newFake(t, fake)

	lib := library.New()
	lib.MarkArchived("2")

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, ok := lib.Bookmarks["2"]; ok {
		t.Error("an archived article was resurrected by the drain")
	}
	if len(lib.Bookmarks) != 4 {
		t.Errorf("have %d bookmarks, want 4", len(lib.Bookmarks))
	}
}

func TestDrainDryRunChangesNothing(t *testing.T) {
	fake := &fakeInstapaper{order: ids(10), limit: 500}
	client := newFake(t, fake)
	lib := library.New()

	if err := drain(context.Background(), client, lib, "unread", true); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(lib.Bookmarks) != 0 {
		t.Errorf("dry run wrote %d bookmarks into the mirror", len(lib.Bookmarks))
	}
}

func TestArchivePendingArchivesAndTombstones(t *testing.T) {
	fake := &fakeInstapaper{order: ids(5), limit: 500}
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")

	lib := library.New()
	for _, id := range ids(5) {
		lib.Bookmarks[id] = library.Bookmark{ID: id, URL: "https://example.com/" + id}
	}
	lib.MarkRead([]library.Bookmark{lib.Bookmarks["1"], lib.Bookmarks["2"]}, time.Now())

	if err := archivePending(context.Background(), client, lib, path, 2, false); err != nil {
		t.Fatalf("archivePending: %v", err)
	}
	if fake.archiveCalls != 2 {
		t.Errorf("made %d archive calls, want 2", fake.archiveCalls)
	}
	if len(lib.Read) != 0 {
		t.Errorf("%d articles still pending", len(lib.Read))
	}
	for _, id := range []string{"1", "2"} {
		if !lib.IsArchived(id) {
			t.Errorf("bookmark %s was archived remotely but has no tombstone", id)
		}
	}
	// And it is durable: the on-disk copy agrees.
	reloaded, err := library.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reloaded.IsArchived("1") || len(reloaded.Read) != 0 {
		t.Error("archive progress was not persisted")
	}
}

// A failed archive must stay pending so the next sync retries it, rather than
// silently vanishing from both the local list and Instapaper.
func TestArchiveFailureLeavesTheArticlePending(t *testing.T) {
	fake := &fakeInstapaper{order: ids(3), limit: 500, failIDs: map[string]bool{"2": true}}
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")

	lib := library.New()
	for _, id := range ids(3) {
		lib.Bookmarks[id] = library.Bookmark{ID: id}
	}
	lib.MarkRead([]library.Bookmark{{ID: "1"}, {ID: "2"}}, time.Now())

	if err := archivePending(context.Background(), client, lib, path, 1, false); err != nil {
		t.Fatalf("archivePending should tolerate a partial failure: %v", err)
	}
	if !lib.IsArchived("1") {
		t.Error("the successful archive was not recorded")
	}
	if _, pending := lib.Read["2"]; !pending {
		t.Error("the failed archive was dropped instead of being left for retry")
	}
	if lib.IsArchived("2") {
		t.Error("a failed archive was tombstoned")
	}
}

func TestArchiveDryRunMakesNoRequests(t *testing.T) {
	fake := &fakeInstapaper{order: ids(3), limit: 500}
	client := newFake(t, fake)

	lib := library.New()
	lib.Bookmarks["1"] = library.Bookmark{ID: "1"}
	lib.MarkRead([]library.Bookmark{{ID: "1"}}, time.Now())

	if err := archivePending(context.Background(), client, lib, "/nonexistent/library.json", 2, true); err != nil {
		t.Fatalf("archivePending: %v", err)
	}
	if fake.archiveCalls != 0 {
		t.Errorf("dry run made %d archive calls", fake.archiveCalls)
	}
	if len(lib.Read) != 1 {
		t.Error("dry run mutated the pending set")
	}
}

// A full pick -> archive -> re-sync cycle: the article ends up archived
// remotely and never returns to the unread mirror.
func TestFullCycleDoesNotResurrectArchivedArticles(t *testing.T) {
	fake := &fakeInstapaper{order: ids(20), limit: 500}
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	ctx := context.Background()

	lib := library.New()
	if err := drain(ctx, client, lib, "unread", false); err != nil {
		t.Fatalf("initial drain: %v", err)
	}
	if len(lib.Bookmarks) != 20 {
		t.Fatalf("initial drain got %d, want 20", len(lib.Bookmarks))
	}

	picks := lib.Pick(3, library.Filter{}, newRNG(99, true))
	lib.MarkRead(picks, time.Now())
	if err := lib.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := archivePending(ctx, client, lib, path, 2, false); err != nil {
		t.Fatalf("archivePending: %v", err)
	}
	if err := drain(ctx, client, lib, "unread", false); err != nil {
		t.Fatalf("second drain: %v", err)
	}

	if len(lib.Bookmarks) != 17 {
		t.Errorf("after the cycle: %d unread, want 17", len(lib.Bookmarks))
	}
	for _, p := range picks {
		if _, back := lib.Bookmarks[p.ID]; back {
			t.Errorf("archived article %s reappeared in the unread mirror", p.ID)
		}
	}
	if len(fake.archived) != 3 {
		t.Errorf("server archived %d articles, want 3", len(fake.archived))
	}
}

func TestParseSpan(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"90d", 90 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"1y", 365 * 24 * time.Hour, false},
		{"36h", 36 * time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"banana", 0, true},
		{"d", 0, true},
	} {
		got, err := parseSpan(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSpan(%q) should have failed", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSpan(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("parseSpan(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestComma(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{{0, "0"}, {999, "999"}, {1000, "1,000"}, {1234567, "1,234,567"}, {-4321, "-4,321"}} {
		if got := comma(tc.in); got != tc.want {
			t.Errorf("comma(%d) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestAppendHaveDoesNotDuplicate(t *testing.T) {
	collected := map[string]library.Bookmark{
		"1": {ID: "1", Hash: "a"},
		"2": {ID: "2"},
	}
	got := appendHave("9:z", collected)
	parts := strings.Split(got, ",")
	seen := map[string]bool{}
	for _, p := range parts {
		if seen[p] {
			t.Errorf("duplicate entry %q in %q", p, got)
		}
		seen[p] = true
	}
	if len(parts) != 3 {
		t.Errorf("got %d entries (%q), want 3", len(parts), got)
	}
	if !seen["9:z"] || !seen["1:a"] || !seen["2"] {
		t.Errorf("unexpected have value %q", got)
	}
}

func BenchmarkPickFromLargeBacklog(b *testing.B) {
	lib := library.New()
	for i := 0; i < 20000; i++ {
		id := fmt.Sprint(i)
		lib.Bookmarks[id] = library.Bookmark{ID: id, URL: "https://example.com/" + id, Time: 1690000000}
	}
	rng := newRNG(1, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lib.Pick(5, library.Filter{}, rng)
	}
}
