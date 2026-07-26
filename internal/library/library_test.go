package library

import (
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"
)

func testRNG() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

func seed(t *testing.T, n int) *Library {
	t.Helper()
	l := New()
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		l.Bookmarks[id] = Bookmark{
			ID:    id,
			URL:   "https://example.com/" + id,
			Title: "Article " + id,
			Time:  now - int64(i)*86400,
		}
	}
	return l
}

func TestPickIsDistinctAndDoesNotMutate(t *testing.T) {
	l := seed(t, 10)
	got := l.Pick(4, Filter{}, testRNG())
	if len(got) != 4 {
		t.Fatalf("picked %d, want 4", len(got))
	}
	seen := map[string]bool{}
	for _, b := range got {
		if seen[b.ID] {
			t.Errorf("duplicate pick: %s", b.ID)
		}
		seen[b.ID] = true
	}
	if len(l.Bookmarks) != 10 {
		t.Errorf("Pick mutated the library: %d bookmarks left", len(l.Bookmarks))
	}
}

func TestPickCapsAtAvailable(t *testing.T) {
	l := seed(t, 3)
	if got := l.Pick(10, Filter{}, testRNG()); len(got) != 3 {
		t.Errorf("picked %d, want 3", len(got))
	}
	if got := New().Pick(5, Filter{}, testRNG()); len(got) != 0 {
		t.Errorf("picked %d from an empty library, want 0", len(got))
	}
}

// The same seed must produce the same picks, which is what --seed promises.
func TestPickIsReproducibleForASeed(t *testing.T) {
	a := seed(t, 20).Pick(5, Filter{}, rand.New(rand.NewPCG(42, 42)))
	b := seed(t, 20).Pick(5, Filter{}, rand.New(rand.NewPCG(42, 42)))
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("pick %d differs between runs: %s vs %s", i, a[i].ID, b[i].ID)
		}
	}
}

func TestMarkReadUndoRoundTrip(t *testing.T) {
	l := seed(t, 5)
	picks := l.Pick(2, Filter{}, testRNG())
	batch := l.MarkRead(picks, time.Now())
	if batch == "" {
		t.Fatal("MarkRead returned no batch id")
	}
	if len(l.Bookmarks) != 3 || len(l.Read) != 2 {
		t.Fatalf("after MarkRead: %d unread, %d pending; want 3 and 2", len(l.Bookmarks), len(l.Read))
	}

	restored, err := l.Undo()
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("restored %d, want 2", len(restored))
	}
	if len(l.Bookmarks) != 5 || len(l.Read) != 0 {
		t.Errorf("after Undo: %d unread, %d pending; want 5 and 0", len(l.Bookmarks), len(l.Read))
	}
	if _, err := l.Undo(); err == nil {
		t.Error("a second Undo should fail")
	}
}

// Once an article is archived remotely it must never come back, even if a later
// sync still reports it.
func TestArchivedArticlesAreNeverResurrected(t *testing.T) {
	l := seed(t, 3)
	picks := l.Pick(1, Filter{}, testRNG())
	l.MarkRead(picks, time.Now())
	id := picks[0].ID

	l.MarkArchived(id)
	if !l.IsArchived(id) {
		t.Fatal("MarkArchived did not record a tombstone")
	}
	if _, still := l.Read[id]; still {
		t.Error("archived article is still pending")
	}

	l.Upsert(Bookmark{ID: id, URL: "https://example.com/again", Title: "Back again"})
	if _, resurrected := l.Bookmarks[id]; resurrected {
		t.Error("an archived article came back through Upsert")
	}

	if _, err := l.Undo(); err == nil {
		t.Error("undoing an already-archived batch should fail")
	}
}

func TestUpsertSkipsPendingArticles(t *testing.T) {
	l := seed(t, 2)
	picks := l.Pick(1, Filter{}, testRNG())
	l.MarkRead(picks, time.Now())
	l.Upsert(Bookmark{ID: picks[0].ID, Title: "should not reappear"})
	if _, back := l.Bookmarks[picks[0].ID]; back {
		t.Error("a pending-archive article was re-added to unread")
	}
}

func TestFilters(t *testing.T) {
	now := time.Now().Unix()
	old := Bookmark{ID: "1", URL: "https://www.nytimes.com/a", Time: now - 200*86400}
	fresh := Bookmark{ID: "2", URL: "https://blog.example.com/b", Time: now - 2*3600, Starred: true}
	sub := Bookmark{ID: "3", URL: "https://cooking.nytimes.com/c", Time: now - 86400}

	l := New()
	for _, b := range []Bookmark{old, fresh, sub} {
		l.Bookmarks[b.ID] = b
	}

	yes, no := true, false
	for _, tc := range []struct {
		name string
		f    Filter
		want []string
	}{
		{"none", Filter{}, []string{"1", "2", "3"}},
		{"older than 100d", Filter{OlderThan: 100 * 24 * time.Hour}, []string{"1"}},
		{"newer than 1d", Filter{NewerThan: 24 * time.Hour}, []string{"2"}},
		{"domain matches subdomains", Filter{Domain: "nytimes.com"}, []string{"1", "3"}},
		{"domain ignores www", Filter{Domain: "www.nytimes.com"}, []string{"1", "3"}},
		{"starred", Filter{Starred: &yes}, []string{"2"}},
		{"unstarred", Filter{Starred: &no}, []string{"1", "3"}},
		{"combined", Filter{Domain: "nytimes.com", OlderThan: 100 * 24 * time.Hour}, []string{"1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := l.Candidates(tc.f)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d candidates, want %d", len(got), len(tc.want))
			}
			for i, b := range got { // Candidates sorts by id
				if b.ID != tc.want[i] {
					t.Errorf("candidate %d = %s, want %s", i, b.ID, tc.want[i])
				}
			}
		})
	}
}

func TestHost(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://www.NYTimes.com/x", "nytimes.com"},
		{"https://example.com:8443/x", "example.com"},
		{"not a url", ""},
	} {
		if got := (Bookmark{URL: tc.url}).Host(); got != tc.want {
			t.Errorf("Host(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// HaveParam must cover unread, pending and archived, so that none of them come
// back in the next bookmarks/list response.
func TestHaveParamCoversEveryKnownID(t *testing.T) {
	l := New()
	l.Bookmarks["1"] = Bookmark{ID: "1", Hash: "abc"}
	l.Bookmarks["2"] = Bookmark{ID: "2"} // no hash yet
	l.Read["3"] = ReadEntry{Bookmark: Bookmark{ID: "3"}}
	l.Archived = append(l.Archived, "4")
	l.normalize()

	if got, want := l.HaveParam(), "1:abc,2,3,4"; got != want {
		t.Errorf("HaveParam() = %q, want %q", got, want)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("SAMLING_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "library.json")

	l := seed(t, 4)
	picks := l.Pick(1, Filter{}, testRNG())
	l.MarkRead(picks, time.Now())
	l.MarkArchived("d")
	l.LastSync = 1700000000
	if err := l.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Bookmarks) != len(l.Bookmarks) || len(got.Read) != len(l.Read) {
		t.Errorf("round trip lost data: %d/%d unread, %d/%d pending",
			len(got.Bookmarks), len(l.Bookmarks), len(got.Read), len(l.Read))
	}
	if !got.IsArchived("d") {
		t.Error("tombstone did not survive the round trip")
	}
	if got.LastSync != 1700000000 {
		t.Errorf("LastSync = %d, want 1700000000", got.LastSync)
	}
}

func TestLoadMissingFileReturnsEmptyLibrary(t *testing.T) {
	l, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load of a missing file should succeed, got %v", err)
	}
	if len(l.Bookmarks) != 0 {
		t.Errorf("expected an empty library, got %d bookmarks", len(l.Bookmarks))
	}
}

func TestStats(t *testing.T) {
	l := New()
	now := time.Now().Unix()
	l.Bookmarks["1"] = Bookmark{ID: "1", URL: "https://a.com/x", Time: now - 500*86400}
	l.Bookmarks["2"] = Bookmark{ID: "2", URL: "https://a.com/y", Time: now - 86400}
	l.Bookmarks["3"] = Bookmark{ID: "3", URL: "https://b.com/z", Time: now - 2*86400}
	l.Read["4"] = ReadEntry{Bookmark: Bookmark{ID: "4"}}
	l.Archived = append(l.Archived, "5", "6")
	l.normalize()

	s := l.Stats(5)
	if s.Unread != 3 || s.Pending != 1 || s.Archived != 2 {
		t.Errorf("counts = %d/%d/%d, want 3/1/2", s.Unread, s.Pending, s.Archived)
	}
	if len(s.Domains) != 2 || s.Domains[0].Host != "a.com" || s.Domains[0].Count != 2 {
		t.Errorf("top domain = %+v, want a.com x2", s.Domains)
	}
	if s.Oldest == nil || s.Oldest.ID != "1" {
		t.Errorf("oldest = %+v, want id 1", s.Oldest)
	}
}
