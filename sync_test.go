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
	order    []string         // folder contents, in order
	gone     map[string]bool  // removed from the folder since the client last looked
	limit    int              // how deep into the folder the API will look
	failIDs  map[string]bool  // archive requests that should fail
	times    map[string]int64 // per-article save time; defaults to a fixed past value
	archived []string

	listCalls, archiveCalls int
}

func (f *fakeInstapaper) timeOf(id string) int64 {
	if t, ok := f.times[id]; ok {
		return t
	}
	return 1690000000
}

func (f *fakeInstapaper) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()

		switch r.URL.Path {
		case "/api/1.1/bookmarks/list":
			f.listCalls++
			have := map[string]bool{}
			if v := r.PostForm.Get("have"); v != "" {
				for _, part := range strings.Split(v, ",") {
					have[strings.SplitN(part, ":", 2)[0]] = true
				}
			}

			// The real API takes the first `limit` items of the folder and only
			// then subtracts `have`, which is why it cannot page past `limit`.
			var window []string
			for _, id := range f.order {
				if f.gone[id] {
					continue
				}
				window = append(window, id)
				if len(window) == f.limit {
					break
				}
			}

			page := []map[string]any{}
			for _, id := range window {
				if have[id] {
					continue
				}
				page = append(page, map[string]any{
					"type": "bookmark", "bookmark_id": id,
					"url": "https://example.com/" + id, "title": "Article " + id,
					"hash": "h" + id, "time": f.timeOf(id), "starred": "0",
				})
			}

			// delete_ids reports ids from "have" that are not in the *window*.
			// Crucially that includes articles still in the folder but pushed
			// below the limit -- verified against the live API, and the source
			// of a real eviction bug.
			inWindow := make(map[string]bool, len(window))
			for _, id := range window {
				inWindow[id] = true
			}
			deleteIDs := []string{}
			for id := range have {
				if !inWindow[id] {
					deleteIDs = append(deleteIDs, id)
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

// The API exposes only the newest MaxListLimit items of a folder and offers no
// way past that, so one sync mirrors the window, not the whole backlog.
func TestDrainMirrorsTheVisibleWindow(t *testing.T) {
	fake := &fakeInstapaper{order: ids(1200), limit: 500}
	client := newFake(t, fake)
	lib := library.New()

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(lib.Bookmarks) != 500 {
		t.Errorf("mirrored %d bookmarks, want 500 (the window)", len(lib.Bookmarks))
	}
	if fake.listCalls != 1 {
		t.Errorf("made %d list calls, want 1 — there is nothing to page through", fake.listCalls)
	}
	if b := lib.Bookmarks["7"]; b.Hash != "h7" || b.Folder != "unread" {
		t.Errorf("bookmark 7 = %+v, want hash h7 and folder unread", b)
	}
}

// The mirror is cumulative: archiving slides the window, and the next sync
// picks up whatever moved into view. This is the only way to reach a backlog
// deeper than the window.
func TestDrainAccumulatesAsTheWindowSlides(t *testing.T) {
	fake := &fakeInstapaper{order: ids(600), limit: 500}
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	ctx := context.Background()

	lib := library.New()
	if err := drain(ctx, client, lib, "unread", false); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if len(lib.Bookmarks) != 500 {
		t.Fatalf("first sync mirrored %d, want 500", len(lib.Bookmarks))
	}

	// Read and archive 100 of them, which slides the window.
	picks := lib.Pick(100, library.Filter{}, newRNG(1, true))
	lib.MarkRead(picks, time.Now())
	if err := archivePending(ctx, client, lib, path, 4, false); err != nil {
		t.Fatalf("archivePending: %v", err)
	}
	if len(lib.Archived) != 100 {
		t.Fatalf("archived %d, want 100", len(lib.Archived))
	}

	if err := drain(ctx, client, lib, "unread", false); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if len(lib.Bookmarks) != 500 {
		t.Errorf("after the window slid: %d unread, want 500", len(lib.Bookmarks))
	}
	if total := len(lib.Bookmarks) + len(lib.Archived); total != 600 {
		t.Errorf("saw %d distinct articles across two syncs, want 600", total)
	}
}

// A saturated window is the dangerous case: an id missing from the response may
// simply have been pushed below the cut. Regression test for a real eviction --
// a re-saved article rejoining the top of Unread displaced the last article,
// which came back as a delete_id and was wrongly dropped from the mirror.
func TestDrainIgnoresDeleteIDsWhenWindowIsSaturated(t *testing.T) {
	// The mirror holds exactly a full window, then one article joins the top
	// and pushes the last one below the cut.
	fake := &fakeInstapaper{order: append([]string{"new"}, ids(500)...), limit: 500}
	client := newFake(t, fake)

	lib := library.New()
	for _, id := range ids(500) {
		lib.Bookmarks[id] = library.Bookmark{ID: id, Hash: "h" + id}
	}

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, ok := lib.Bookmarks["500"]; !ok {
		t.Error("evicted a live article that was merely displaced from the window")
	}
	if _, ok := lib.Bookmarks["new"]; !ok {
		t.Error("the newly arrived article was not picked up")
	}
	if len(lib.Bookmarks) != 501 {
		t.Errorf("have %d bookmarks, want 501", len(lib.Bookmarks))
	}
}

// Once the mirror outgrows the window, nothing reported missing can be trusted.
func TestDrainIgnoresDeleteIDsWhenMirrorExceedsWindow(t *testing.T) {
	fake := &fakeInstapaper{order: ids(600), limit: 500}
	client := newFake(t, fake)

	lib := library.New()
	for _, id := range ids(600) {
		lib.Bookmarks[id] = library.Bookmark{ID: id, Hash: "h" + id}
	}

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(lib.Bookmarks) != 600 {
		t.Errorf("have %d bookmarks, want all 600 kept", len(lib.Bookmarks))
	}
}

// With plenty of headroom the signal is unambiguous, so articles removed
// elsewhere really should disappear locally.
func TestDrainHonoursDeleteIDsWhenWellInsideWindow(t *testing.T) {
	fake := &fakeInstapaper{order: ids(10), limit: 500, gone: map[string]bool{"3": true}}
	client := newFake(t, fake)

	lib := library.New()
	lib.Bookmarks["3"] = library.Bookmark{ID: "3", Hash: "h3"}    // archived elsewhere
	lib.Bookmarks["99"] = library.Bookmark{ID: "99", Hash: "h99"} // never existed

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, id := range []string{"3", "99"} {
		if _, ok := lib.Bookmarks[id]; ok {
			t.Errorf("bookmark %s is gone remotely but survived locally", id)
		}
	}
	if len(lib.Bookmarks) != 9 {
		t.Errorf("have %d bookmarks, want 9", len(lib.Bookmarks))
	}
}

// An article we archived that is no longer in the folder stays tombstoned.
func TestDrainKeepsTombstoneWhileArticleStaysArchived(t *testing.T) {
	fake := &fakeInstapaper{order: ids(5), limit: 500, gone: map[string]bool{"2": true}}
	client := newFake(t, fake)

	lib := library.New()
	lib.MarkArchived("2")

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, ok := lib.Bookmarks["2"]; ok {
		t.Error("an archived article was resurrected by the drain")
	}
	if !lib.IsArchived("2") {
		t.Error("tombstone was cleared for an article still archived")
	}
	if len(lib.Bookmarks) != 4 {
		t.Errorf("have %d bookmarks, want 4", len(lib.Bookmarks))
	}
}

// ...but if it turns up in Unread again, it was re-saved by hand and must come
// back. This is the "close the junk, re-save the keepers" triage habit.
func TestDrainRescuesTombstonedArticleThatReappears(t *testing.T) {
	fake := &fakeInstapaper{order: ids(5), limit: 500} // "2" is still in Unread
	client := newFake(t, fake)

	lib := library.New()
	lib.MarkArchived("2")

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, ok := lib.Bookmarks["2"]; !ok {
		t.Error("a re-saved article was not rescued")
	}
	if lib.IsArchived("2") {
		t.Error("tombstone survived for a re-saved article")
	}
	if len(lib.Bookmarks) != 5 {
		t.Errorf("have %d bookmarks, want 5", len(lib.Bookmarks))
	}
}

// A pending article whose save time is newer than the pick was re-saved during
// triage: it must be rescued rather than archived.
func TestDrainRescuesPendingArticleReSavedAfterThePick(t *testing.T) {
	now := time.Now()
	fake := &fakeInstapaper{order: ids(3), limit: 500}
	fake.times = map[string]int64{"1": now.Add(time.Minute).Unix()} // re-saved a minute ago
	client := newFake(t, fake)

	lib := library.New()
	for _, id := range ids(3) {
		lib.Bookmarks[id] = library.Bookmark{ID: id}
	}
	lib.MarkRead([]library.Bookmark{{ID: "1"}, {ID: "2"}}, now.Add(-time.Hour))

	if err := drain(context.Background(), client, lib, "unread", false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, pending := lib.Pending("1"); pending {
		t.Error("re-saved article is still queued for archiving")
	}
	if _, unread := lib.Bookmarks["1"]; !unread {
		t.Error("re-saved article did not return to unread")
	}
	// The one that was not re-saved is untouched and still due for archiving.
	if _, pending := lib.Pending("2"); !pending {
		t.Error("an untouched pick was wrongly rescued")
	}
}

// The whole triage loop: pick a batch, re-save one of them, sync. The keeper
// must survive and everything else must be archived.
func TestTriageLoopKeepsReSavedArticles(t *testing.T) {
	now := time.Now()
	fake := &fakeInstapaper{order: ids(10), limit: 500}
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	ctx := context.Background()

	lib := library.New()
	if err := drain(ctx, client, lib, "unread", false); err != nil {
		t.Fatalf("seed drain: %v", err)
	}

	picks := lib.Pick(4, library.Filter{}, newRNG(7, true))
	lib.MarkRead(picks, now.Add(-time.Hour))
	keeper := picks[0].ID

	// The browser extension re-saves the keeper: same id, fresh timestamp.
	fake.mu.Lock()
	fake.times = map[string]int64{keeper: now.Unix()}
	fake.mu.Unlock()

	if err := drain(ctx, client, lib, "unread", false); err != nil {
		t.Fatalf("sync drain: %v", err)
	}
	if err := archivePending(ctx, client, lib, path, 2, false); err != nil {
		t.Fatalf("archivePending: %v", err)
	}

	if _, unread := lib.Bookmarks[keeper]; !unread {
		t.Error("the re-saved keeper was not preserved")
	}
	if lib.IsArchived(keeper) {
		t.Error("the re-saved keeper was archived anyway")
	}
	for _, id := range fake.archived {
		if id == keeper {
			t.Error("an archive request was sent for the keeper")
		}
	}
	if len(fake.archived) != 3 {
		t.Errorf("archived %d articles, want 3 (the batch minus the keeper)", len(fake.archived))
	}
	if len(lib.Bookmarks) != 7 {
		t.Errorf("%d unread, want 7", len(lib.Bookmarks))
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

// The count is a bare argument, and it has to survive being written on either
// side of the flags. Go's flag package stops at the first non-flag argument, so
// `pick 20 --older-than 1y` would otherwise parse the count and silently drop
// the filter.
func TestPickCountArgument(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    int
		wantErr bool
	}{
		{"bare count", []string{"20"}, 20, false},
		{"count then flags", []string{"20", "--older-than", "1y"}, 20, false},
		{"flags then count", []string{"--older-than", "1y", "20"}, 20, false},
		{"legacy -n", []string{"-n", "7"}, 7, false},
		{"default", nil, 1, false},
		{"count twice", []string{"3", "4"}, 0, true},
		{"not a number", []string{"banana"}, 0, true},
		{"zero", []string{"0"}, 0, true},
		{"negative", []string{"-n", "-3"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePickCount(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got count %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}
