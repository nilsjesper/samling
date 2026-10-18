package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nilsjesper/samling/internal/instapaper"
	"github.com/nilsjesper/samling/internal/library"
	"github.com/nilsjesper/samling/internal/oauth1"
)

// fakeArticle is one bookmark on the fake server.
type fakeArticle struct {
	time     int64  // save time; a re-save bumps it
	folder   *int64 // nil is the home list
	archived bool
	modified int64 // last change, which is what the `since` feed filters on
	tags     []string
}

// fakeInstapaper models API v2's GET /bookmarks (offset paging by section, and
// the account-wide `since` feed with deleted_ids) and POST /bookmarks/{id}/move
// closely enough to exercise drain and archivePending.
type fakeInstapaper struct {
	mu       sync.Mutex
	articles map[string]*fakeArticle
	deleted  map[string]int64 // id -> when it was deleted
	failIDs  map[string]bool  // move requests that should fail
	failFeed bool             // `since` requests answer 500
	failTags bool             // tag writes answer 500
	archived []string

	tagIDs    map[string]int64 // the account's tags, by name; never deleted
	tagWrites []tagWrite

	listCalls, sinceCalls, moveCalls, tagListCalls int
}

// tagWrite is one POST /bookmarks/{id}/tags the fake accepted.
type tagWrite struct {
	id     string
	add    []string
	remove []int64
}

const pastTime = 1690000000

func newFakeServer(n int) *fakeInstapaper {
	f := &fakeInstapaper{
		articles: map[string]*fakeArticle{},
		deleted:  map[string]int64{},
		failIDs:  map[string]bool{},
		tagIDs:   map[string]int64{},
	}
	for _, id := range ids(n) {
		f.articles[id] = &fakeArticle{time: pastTime, modified: pastTime}
	}
	return f
}

// The helpers below change server state the way another device would, and
// stamp the change with the current time so the feed reports it.

func (f *fakeInstapaper) save(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().Unix()
	// Re-saving reuses the id, bumps the save time, and puts it back in home.
	// Tags survive it (verified live with the browser extension).
	a := &fakeArticle{time: now, modified: now}
	if old, ok := f.articles[id]; ok {
		a.tags = old.tags
	}
	f.articles[id] = a
	delete(f.deleted, id)
}

// tag puts tags on an article directly, as the web app would, without
// touching its modification time.
func (f *fakeInstapaper) tag(id string, names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		f.tagID(n)
	}
	f.articles[id].tags = append(f.articles[id].tags, names...)
}

// tagID returns a tag's id, creating the tag if need be. Ids are offset so
// that they can't be confused with bookmark ids in a test.
func (f *fakeInstapaper) tagID(name string) int64 {
	if id, ok := f.tagIDs[name]; ok {
		return id
	}
	id := int64(4220000 + len(f.tagIDs))
	f.tagIDs[name] = id
	return id
}

func (f *fakeInstapaper) tagJSON(names []string) []map[string]any {
	out := []map[string]any{}
	for _, n := range names {
		out = append(out, map[string]any{"id": f.tagID(n), "name": n, "slug": n, "count": 1})
	}
	return out
}

func (f *fakeInstapaper) archiveElsewhere(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.articles[id]
	a.archived, a.modified = true, time.Now().Unix()
}

func (f *fakeInstapaper) moveElsewhere(id string, folder int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.articles[id]
	a.folder, a.archived, a.modified = &folder, false, time.Now().Unix()
}

func (f *fakeInstapaper) deleteElsewhere(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.articles, id)
	f.deleted[id] = time.Now().Unix()
}

func (f *fakeInstapaper) json(id string, a *fakeArticle) map[string]any {
	n, _ := strconv.ParseInt(id, 10, 64)
	return map[string]any{
		"id": n, "url": "https://example.com/" + id, "title": "Article " + id,
		"progress": map[string]any{"percentage": 0.0, "timestamp": 0},
		"liked":    false, "archived": a.archived, "time": a.time,
		"pubtime": nil, "author": nil, "folder_id": a.folder,
		"tags": f.tagJSON(a.tags), "private_source": nil, "category": 0,
	}
}

func (f *fakeInstapaper) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("Authorization = %q, want the v1 token as a bearer token", got)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":401,"message":"Authentication failed"}}`))
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/2/bookmarks":
			q := r.URL.Query()
			offset, _ := strconv.Atoi(q.Get("offset"))
			limit, _ := strconv.Atoi(q.Get("limit"))
			if limit < 1 || limit > 500 {
				t.Errorf("limit = %d", limit)
			}
			if s := q.Get("since"); s != "" {
				f.sinceCalls++
				if f.failFeed {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(`{"error":{"code":500,"message":"boom"}}`))
					return
				}
				since, _ := strconv.ParseInt(s, 10, 64)
				f.writeFeed(w, since, offset, limit)
				return
			}
			f.listCalls++
			f.writeSection(w, q.Get("section"), q.Get("folder_id"), offset, limit)

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/2/bookmarks/") &&
			strings.HasSuffix(r.URL.Path, "/move"):
			f.moveCalls++
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/2/bookmarks/"), "/move")
			var body struct{ Section string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Section != "archive" {
				t.Errorf("move body: %+v, %v", body, err)
			}
			a, ok := f.articles[id]
			if !ok || f.failIDs[id] {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":{"code":400,"message":"Invalid or missing bookmark_id"}}`))
				return
			}
			a.archived, a.modified = true, time.Now().Unix()
			f.archived = append(f.archived, id)
			json.NewEncoder(w).Encode(f.json(id, a))

		case r.Method == http.MethodGet && r.URL.Path == "/api/2/tags":
			f.tagListCalls++
			var names []string
			for n := range f.tagIDs {
				names = append(names, n)
			}
			sort.Strings(names)
			json.NewEncoder(w).Encode(map[string]any{"tags": f.tagJSON(names)})

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/2/bookmarks/") &&
			strings.HasSuffix(r.URL.Path, "/tags"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/2/bookmarks/"), "/tags")
			if f.failTags {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":{"code":500,"message":"boom"}}`))
				return
			}
			var body struct {
				Add    []struct{ Name string } `json:"add_tags"`
				Remove []struct{ ID int64 }    `json:"remove_tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Add)+len(body.Remove) == 0 {
				t.Errorf("tags body: %+v, %v", body, err)
			}
			a, ok := f.articles[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":{"code":404,"message":"Not found"}}`))
				return
			}
			tw := tagWrite{id: id}
			gone := map[int64]bool{}
			for _, rm := range body.Remove {
				gone[rm.ID] = true
				tw.remove = append(tw.remove, rm.ID)
			}
			var kept []string
			for _, n := range a.tags {
				if !gone[f.tagID(n)] {
					kept = append(kept, n)
				}
			}
			for _, ad := range body.Add {
				tw.add = append(tw.add, ad.Name)
				f.tagID(ad.Name)
				kept = append(kept, ad.Name)
			}
			// A tag change shows up in the feed, but leaves `time` alone.
			a.tags, a.modified = kept, time.Now().Unix()
			f.tagWrites = append(f.tagWrites, tw)
			json.NewEncoder(w).Encode(map[string]any{"created_tags": []any{}, "tags": f.tagJSON(a.tags)})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}
}

// writeSection answers a section read: newest save first, sliced by offset.
func (f *fakeInstapaper) writeSection(w http.ResponseWriter, section, folderID string, offset, limit int) {
	var in []string
	for id, a := range f.articles {
		switch {
		case a.archived:
		case section == "home" && a.folder == nil:
			in = append(in, id)
		case section == "folder" && a.folder != nil && strconv.FormatInt(*a.folder, 10) == folderID:
			in = append(in, id)
		}
	}
	sort.Slice(in, func(i, j int) bool {
		a, b := f.articles[in[i]], f.articles[in[j]]
		if a.time != b.time {
			return a.time > b.time
		}
		return atoi(in[i]) < atoi(in[j])
	})
	page := []map[string]any{}
	for _, id := range window(in, offset, limit) {
		page = append(page, f.json(id, f.articles[id]))
	}
	json.NewEncoder(w).Encode(map[string]any{"bookmarks": page, "total": len(in)})
}

// writeFeed answers `since`: every article changed after since, archived or
// not, then every deletion after it, sharing one offset/limit sequence.
func (f *fakeInstapaper) writeFeed(w http.ResponseWriter, since int64, offset, limit int) {
	var changed, deleted []string
	for id, a := range f.articles {
		if a.modified > since {
			changed = append(changed, id)
		}
	}
	for id, at := range f.deleted {
		if at > since {
			deleted = append(deleted, id)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return atoi(changed[i]) < atoi(changed[j]) })
	sort.Slice(deleted, func(i, j int) bool { return atoi(deleted[i]) < atoi(deleted[j]) })

	all := append(append([]string{}, changed...), deleted...)
	bms, dels := []map[string]any{}, []int{}
	for i, id := range window(all, offset, limit) {
		if offset+i < len(changed) {
			bms = append(bms, f.json(id, f.articles[id]))
		} else {
			dels = append(dels, atoi(id))
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"bookmarks": bms, "deleted_ids": dels, "total": len(all)})
}

func window(s []string, offset, limit int) []string {
	if offset >= len(s) {
		return nil
	}
	return s[offset:min(offset+limit, len(s))]
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func newFake(t *testing.T, f *fakeInstapaper) *instapaper.Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
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

var unread = syncOptions{Folder: library.Unread}

func mustDrain(t *testing.T, client *instapaper.Client, lib *library.Library, o syncOptions) {
	t.Helper()
	if err := drain(context.Background(), client, lib, o); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// v1 could never see past the newest 500 of a folder. v2 pages: 1,200 unread
// articles take three requests of 500, 500 and 200.
func TestFullSyncPagesPastFiveHundred(t *testing.T) {
	fake := newFakeServer(1200)
	client := newFake(t, fake)
	lib := library.New()

	before := time.Now()
	mustDrain(t, client, lib, unread)

	if len(lib.Bookmarks) != 1200 {
		t.Errorf("mirrored %d bookmarks, want all 1200", len(lib.Bookmarks))
	}
	if fake.listCalls != 3 || fake.sinceCalls != 0 {
		t.Errorf("made %d list and %d since calls, want 3 and 0", fake.listCalls, fake.sinceCalls)
	}
	if b := lib.Bookmarks["7"]; b.Folder != library.Unread || b.URL != "https://example.com/7" || b.Time != pastTime {
		t.Errorf("bookmark 7 = %+v", b)
	}
	if !lib.IsSynced(library.Unread) {
		t.Error("unread was not recorded as fully synced")
	}
	// The cursor is the start of the sync, minus the skew allowance.
	want := before.Add(-cursorOverlap).Unix()
	if lib.Cursor < want || lib.Cursor > time.Now().Add(-cursorOverlap).Unix() {
		t.Errorf("cursor = %d, want about %d", lib.Cursor, want)
	}
}

// Once synced, a sync reads only the change feed: new articles come in, and
// articles archived, deleted or moved out of the mirrored folders go away.
func TestIncrementalSyncAppliesTheChangeFeed(t *testing.T) {
	fake := newFakeServer(20)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	fake.save("21")
	fake.archiveElsewhere("3")
	fake.deleteElsewhere("4")
	fake.moveElsewhere("5", 555) // a folder samling does not mirror

	mustDrain(t, client, lib, unread)

	if fake.listCalls != 1 || fake.sinceCalls != 1 {
		t.Errorf("made %d list and %d since calls, want 1 and 1", fake.listCalls, fake.sinceCalls)
	}
	if _, ok := lib.Bookmarks["21"]; !ok {
		t.Error("a newly saved article was not picked up")
	}
	for _, id := range []string{"3", "4", "5"} {
		if _, ok := lib.Bookmarks[id]; ok {
			t.Errorf("article %s left unread remotely but is still mirrored", id)
		}
		if lib.IsArchived(id) {
			t.Errorf("article %s was never picked, so it should not be tombstoned", id)
		}
	}
	if len(lib.Bookmarks) != 18 {
		t.Errorf("%d unread, want 18", len(lib.Bookmarks))
	}
}

// The whole triage loop: pick a batch, re-save one of them, sync. The keeper
// must survive and everything else must be archived.
func TestTriageLoopKeepsReSavedArticles(t *testing.T) {
	now := time.Now()
	fake := newFakeServer(10)
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	ctx := context.Background()

	lib := library.New()
	mustDrain(t, client, lib, unread)

	picks := lib.Pick(4, library.Filter{}, newRNG(7, true))
	lib.MarkRead(picks, now.Add(-time.Hour))
	keeper := picks[0].ID

	// The browser extension re-saves the keeper: same id, fresh timestamp.
	fake.save(keeper)

	mustDrain(t, client, lib, unread)
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

// A pending pick that turns up in the feed without a newer save time -- read
// in the browser, say, which touches progress -- is still due.
func TestChangedButNotReSavedPickStaysDue(t *testing.T) {
	fake := newFakeServer(3)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	lib.MarkRead([]library.Bookmark{lib.Bookmarks["1"]}, time.Now())
	fake.mu.Lock()
	fake.articles["1"].modified = time.Now().Unix() + 1
	fake.mu.Unlock()

	mustDrain(t, client, lib, unread)
	if _, pending := lib.Pending("1"); !pending {
		t.Error("a touched but not re-saved pick was dropped from the archive queue")
	}
	if _, back := lib.Bookmarks["1"]; back {
		t.Error("a touched but not re-saved pick returned to unread")
	}
}

// An article samling archived that is re-saved later comes back, even though
// it has been tombstoned. This is the "re-save the keepers" habit applied
// after the sync rather than before it.
func TestTombstonedArticleReSavedIsRescued(t *testing.T) {
	fake := newFakeServer(5)
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	lib := library.New()
	mustDrain(t, client, lib, unread)

	lib.MarkRead([]library.Bookmark{lib.Bookmarks["2"]}, time.Now().Add(-time.Hour))
	if err := archivePending(context.Background(), client, lib, path, 1, false); err != nil {
		t.Fatalf("archivePending: %v", err)
	}
	if !lib.IsArchived("2") {
		t.Fatal("setup: expected a tombstone")
	}

	// The next sync sees our own archive in the feed; that changes nothing.
	mustDrain(t, client, lib, unread)
	if !lib.IsArchived("2") || len(lib.Bookmarks) != 4 {
		t.Fatalf("our own archive echoed back through the feed and disturbed the mirror")
	}

	fake.save("2")
	mustDrain(t, client, lib, unread)
	if _, ok := lib.Bookmarks["2"]; !ok {
		t.Error("a re-saved article was not rescued")
	}
	if lib.IsArchived("2") {
		t.Error("tombstone survived for a re-saved article")
	}
}

// The same rescue has to work on a full read, which is what a migrated library
// gets first.
func TestFullSyncRescuesTombstonedArticle(t *testing.T) {
	fake := newFakeServer(5) // "2" is in unread
	client := newFake(t, fake)
	lib := library.New()
	lib.MarkArchived("2")

	mustDrain(t, client, lib, unread)
	if _, ok := lib.Bookmarks["2"]; !ok || lib.IsArchived("2") {
		t.Error("a tombstoned article back in unread was not rescued")
	}
}

// A pick archived from another device needs no archive request: it is
// tombstoned on the spot. Checked for both the feed and a full read.
func TestPickArchivedElsewhereIsTombstonedWithoutARequest(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			fake := newFakeServer(5)
			client := newFake(t, fake)
			path := filepath.Join(t.TempDir(), "library.json")
			lib := library.New()
			mustDrain(t, client, lib, unread)

			lib.MarkRead([]library.Bookmark{lib.Bookmarks["1"], lib.Bookmarks["2"]}, time.Now())
			fake.archiveElsewhere("1")

			mustDrain(t, client, lib, syncOptions{Folder: library.Unread, Full: full})
			if err := archivePending(context.Background(), client, lib, path, 1, false); err != nil {
				t.Fatalf("archivePending: %v", err)
			}
			if !lib.IsArchived("1") {
				t.Error("the pick archived elsewhere was not tombstoned")
			}
			if fake.moveCalls != 1 || len(fake.archived) != 1 || fake.archived[0] != "2" {
				t.Errorf("archive requests went to %v, want only 2", fake.archived)
			}
		})
	}
}

// A full read is a snapshot, so mirrored articles it does not return are gone.
func TestFullSyncDropsWhatTheSnapshotLacks(t *testing.T) {
	fake := newFakeServer(10)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	// Removed with no trace in the feed, e.g. long before the cursor.
	fake.mu.Lock()
	delete(fake.articles, "3")
	fake.mu.Unlock()

	mustDrain(t, client, lib, unread)
	if _, ok := lib.Bookmarks["3"]; !ok {
		t.Fatal("an incremental sync should not notice a change the feed does not report")
	}
	mustDrain(t, client, lib, syncOptions{Folder: library.Unread, Full: true})
	if _, ok := lib.Bookmarks["3"]; ok {
		t.Error("sync --full kept an article the snapshot did not return")
	}
	if fake.listCalls != 2 {
		t.Errorf("made %d list calls, want 2", fake.listCalls)
	}
}

// Upgrading from schema v1: the library has a cursor-less mirror of the old
// 500-item window, a pending pick, and tombstones. The first sync must read
// everything, keep the pick due, and drop what is no longer there.
func TestV1LibraryMigratesIntoAFullSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.json")
	pickedAt := time.Now().Add(-time.Hour).Unix()
	v1 := fmt.Sprintf(`{
	  "version": 1,
	  "last_sync": 1760000000,
	  "bookmarks": {
	    "1": {"id":"1","url":"https://example.com/1","title":"Article 1","hash":"h1","time":%[1]d,"folder":"unread"},
	    "2": {"id":"2","url":"https://example.com/2","title":"Article 2","hash":"h2","time":%[1]d,"folder":"unread"},
	    "9999": {"id":"9999","url":"https://example.com/9999","title":"Gone","hash":"h99","time":%[1]d,"folder":"unread"}
	  },
	  "read": {
	    "3": {"id":"3","url":"https://example.com/3","title":"Article 3","hash":"h3","time":%[1]d,"folder":"unread",
	          "picked_at":%[2]d,"batch":"b-%[2]d"}
	  },
	  "archived": ["700"],
	  "last_batch": "b-%[2]d"
	}`, pastTime, pickedAt)
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := newFakeServer(800) // far more than v1 could see
	client := newFake(t, fake)
	fake.archiveElsewhere("700")

	lib, err := library.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mustDrain(t, client, lib, unread)

	if fake.listCalls != 2 || fake.sinceCalls != 0 {
		t.Errorf("made %d list and %d since calls; a migrated library needs a full read", fake.listCalls, fake.sinceCalls)
	}
	if e, ok := lib.Pending("3"); !ok || e.PickedAt != pickedAt {
		t.Error("the pending pick did not survive the migration")
	}
	if !lib.IsArchived("700") {
		t.Error("a tombstone did not survive the migration")
	}
	if _, ok := lib.Bookmarks["9999"]; ok {
		t.Error("an article no longer on the server survived the full read")
	}
	// 800 on the server, minus the pending pick and the archived one.
	if len(lib.Bookmarks) != 798 {
		t.Errorf("%d unread, want 798", len(lib.Bookmarks))
	}
	if lib.Cursor == 0 {
		t.Error("no cursor after the first v2 sync")
	}
}

// Every synced folder stays current, even when another one is being read in
// full; and an article moving between mirrored folders follows along.
func TestFeedKeepsOtherSyncedFoldersCurrent(t *testing.T) {
	fake := newFakeServer(6)
	fake.moveElsewhere("5", 555)
	fake.moveElsewhere("6", 555)
	client := newFake(t, fake)
	lib := library.New()

	mustDrain(t, client, lib, unread)
	mustDrain(t, client, lib, syncOptions{Folder: "555"})
	if fake.sinceCalls != 1 {
		t.Errorf("reading 555 in full made %d feed calls, want 1 to keep unread current", fake.sinceCalls)
	}
	if b := lib.Bookmarks["5"]; b.Folder != "555" {
		t.Errorf("article 5 folder = %q, want 555", b.Folder)
	}

	fake.moveElsewhere("1", 555)
	fake.save("7")
	mustDrain(t, client, lib, unread)
	if b := lib.Bookmarks["1"]; b.Folder != "555" {
		t.Errorf("article 1 moved to 555 but is mirrored in %q", b.Folder)
	}
	if b := lib.Bookmarks["7"]; b.Folder != library.Unread {
		t.Errorf("new article 7 folder = %q", b.Folder)
	}
	if len(lib.Bookmarks) != 7 {
		t.Errorf("%d mirrored, want 7", len(lib.Bookmarks))
	}
}

func TestDrainDryRunChangesNothing(t *testing.T) {
	fake := newFakeServer(10)
	client := newFake(t, fake)

	lib := library.New()
	mustDrain(t, client, lib, syncOptions{Folder: library.Unread, DryRun: true})
	if len(lib.Bookmarks) != 0 || lib.Cursor != 0 || len(lib.Synced) != 0 {
		t.Errorf("dry run wrote %d bookmarks, cursor %d, synced %v", len(lib.Bookmarks), lib.Cursor, lib.Synced)
	}

	// And on the incremental path.
	mustDrain(t, client, lib, unread)
	cursor := lib.Cursor
	fake.save("11")
	mustDrain(t, client, lib, syncOptions{Folder: library.Unread, DryRun: true})
	if _, ok := lib.Bookmarks["11"]; ok || lib.Cursor != cursor {
		t.Error("an incremental dry run changed the mirror or the cursor")
	}
}

// A failed sync must leave the cursor where it was, or the changes it failed
// to read would never be read.
func TestCursorAdvancesOnlyOnSuccess(t *testing.T) {
	fake := newFakeServer(5)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	lib.Cursor -= 3600 // as if the last sync was an hour ago
	cursor := lib.Cursor
	fake.save("6")
	fake.failFeed = true
	if err := drain(context.Background(), client, lib, unread); err == nil {
		t.Fatal("expected the feed failure to surface")
	}
	if lib.Cursor != cursor {
		t.Errorf("cursor moved from %d to %d on a failed sync", cursor, lib.Cursor)
	}

	fake.failFeed = false
	mustDrain(t, client, lib, unread)
	if _, ok := lib.Bookmarks["6"]; !ok {
		t.Error("the change missed by the failed sync was not picked up by the next")
	}
	if lib.Cursor <= cursor {
		t.Error("cursor did not advance after a successful sync")
	}
}

func TestSyncFolderValidation(t *testing.T) {
	for in, want := range map[string]string{"unread": "unread", "home": "unread", "1234567": "1234567"} {
		if got, err := syncFolder(in); err != nil || got != want {
			t.Errorf("syncFolder(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"starred", "archive", "liked", "Long Reads", "-1"} {
		if _, err := syncFolder(in); err == nil {
			t.Errorf("syncFolder(%q) should fail", in)
		}
	}
}

func TestArchivePendingArchivesAndTombstones(t *testing.T) {
	fake := newFakeServer(5)
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
	if fake.moveCalls != 2 {
		t.Errorf("made %d move calls, want 2", fake.moveCalls)
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
	fake := newFakeServer(3)
	fake.failIDs["2"] = true
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
	fake := newFakeServer(3)
	client := newFake(t, fake)

	lib := library.New()
	lib.Bookmarks["1"] = library.Bookmark{ID: "1"}
	lib.MarkRead([]library.Bookmark{{ID: "1"}}, time.Now())

	if err := archivePending(context.Background(), client, lib, "/nonexistent/library.json", 2, true); err != nil {
		t.Fatalf("archivePending: %v", err)
	}
	if fake.moveCalls != 0 {
		t.Errorf("dry run made %d move calls", fake.moveCalls)
	}
	if len(lib.Read) != 1 {
		t.Error("dry run mutated the pending set")
	}
}

// A full pick -> archive -> re-sync cycle: the article ends up archived
// remotely and never returns to the unread mirror.
func TestFullCycleDoesNotResurrectArchivedArticles(t *testing.T) {
	fake := newFakeServer(20)
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	ctx := context.Background()

	lib := library.New()
	mustDrain(t, client, lib, unread)
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
	mustDrain(t, client, lib, unread)                                          // incremental
	mustDrain(t, client, lib, syncOptions{Folder: library.Unread, Full: true}) // and full

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
