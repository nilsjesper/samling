package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nilsjesper/samling/internal/instapaper"
	"github.com/nilsjesper/samling/internal/library"
)

// rescueOnce picks id, re-saves it from "the browser", and runs the sync steps
// that notice and record the rescue.
func rescueOnce(t *testing.T, fake *fakeInstapaper, client *instapaper.Client, lib *library.Library, id string) {
	t.Helper()
	b, ok := lib.Bookmarks[id]
	if !ok {
		t.Fatalf("setup: %s is not unread", id)
	}
	lib.MarkRead([]library.Bookmark{b}, time.Now().Add(-time.Hour))
	fake.save(id)
	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)
}

func mustMarkKept(t *testing.T, client *instapaper.Client, lib *library.Library, dryRun bool) {
	t.Helper()
	if err := markKept(context.Background(), client, lib, dryRun); err != nil {
		t.Fatalf("markKept: %v", err)
	}
}

func TestFirstRescueAddsSamling1(t *testing.T) {
	fake := newFakeServer(5)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	rescueOnce(t, fake, client, lib, "2")

	if len(fake.tagWrites) != 1 {
		t.Fatalf("made %d tag writes, want 1: %+v", len(fake.tagWrites), fake.tagWrites)
	}
	w := fake.tagWrites[0]
	if w.id != "2" || len(w.add) != 1 || w.add[0] != "samling1" || len(w.remove) != 0 {
		t.Errorf("tag write = %+v, want samling1 added to 2", w)
	}
	if fake.tagListCalls != 0 {
		t.Error("read the tag list although there was nothing to remove")
	}
	if got := lib.Bookmarks["2"].Kept(); got != 1 {
		t.Errorf("mirror shows kept ×%d, want 1 straight from the response", got)
	}
	if len(lib.PendingKept) != 0 {
		t.Errorf("tag queue = %v after a successful write", lib.PendingKept)
	}
	// Articles that were not rescued are left alone.
	for _, id := range []string{"1", "3", "4", "5"} {
		if tags := fake.articles[id].tags; len(tags) != 0 {
			t.Errorf("article %s was tagged %v", id, tags)
		}
	}
}

// A rescue swaps samlingN for samling(N+1) in one request, and leaves the
// user's own tags alone.
func TestRescueSwapsToTheNextCount(t *testing.T) {
	fake := newFakeServer(3)
	fake.tag("1", "ifttt", "samling2")
	fake.tag("2", "samling1", "samling4") // shouldn't happen, but heal it if it does
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)
	if got := lib.Bookmarks["1"].Kept(); got != 2 {
		t.Fatalf("setup: mirror shows kept ×%d, want 2", got)
	}

	rescueOnce(t, fake, client, lib, "1")
	rescueOnce(t, fake, client, lib, "2")

	if len(fake.tagWrites) != 2 {
		t.Fatalf("made %d tag writes, want 2", len(fake.tagWrites))
	}
	w := fake.tagWrites[0]
	if len(w.add) != 1 || w.add[0] != "samling3" ||
		len(w.remove) != 1 || w.remove[0] != fake.tagIDs["samling2"] {
		t.Errorf("first write = %+v, want samling3 in and samling2 (id %d) out", w, fake.tagIDs["samling2"])
	}
	if got := fmt.Sprint(fake.articles["1"].tags); got != "[ifttt samling3]" {
		t.Errorf("article 1 tags on the server = %s", got)
	}
	if got := fmt.Sprint(lib.Bookmarks["1"].Tags); got != "[ifttt samling3]" {
		t.Errorf("article 1 tags in the mirror = %s", got)
	}
	if got := fmt.Sprint(fake.articles["2"].tags); got != "[samling5]" {
		t.Errorf("article 2 tags on the server = %s, want both old counts replaced by samling5", got)
	}
	if fake.tagListCalls != 2 {
		t.Errorf("read the tag list %d times, want once per sync that removes a tag", fake.tagListCalls)
	}
}

// A failed write is reported, not fatal, and stays queued with its target
// count. The next sync writes it exactly once, without counting it twice.
func TestFailedTagWriteIsRetriedOnce(t *testing.T) {
	fake := newFakeServer(3)
	fake.tag("1", "samling1")
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	lib := library.New()
	mustDrain(t, client, lib, unread)

	fake.failTags = true
	rescueOnce(t, fake, client, lib, "1")
	if lib.PendingKept["1"] != 2 {
		t.Fatalf("tag queue = %v, want 1 -> 2", lib.PendingKept)
	}
	if _, unread := lib.Bookmarks["1"]; !unread {
		t.Error("a failed tag write undid the rescue")
	}

	// The queue survives a restart.
	if err := lib.Save(path); err != nil {
		t.Fatal(err)
	}
	lib, err := library.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if lib.PendingKept["1"] != 2 {
		t.Fatalf("tag queue after reload = %v", lib.PendingKept)
	}

	fake.failTags = false
	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)
	mustDrain(t, client, lib, unread) // our own write echoes back through the feed
	mustMarkKept(t, client, lib, false)

	if len(fake.tagWrites) != 1 {
		t.Fatalf("made %d tag writes, want 1: %+v", len(fake.tagWrites), fake.tagWrites)
	}
	if got := fmt.Sprint(fake.articles["1"].tags); got != "[samling2]" {
		t.Errorf("server tags = %s, want [samling2]", got)
	}
	if got := lib.Bookmarks["1"].Kept(); got != 2 {
		t.Errorf("mirror shows kept ×%d, want 2", got)
	}
	if len(lib.PendingKept) != 0 {
		t.Errorf("tag queue = %v after the retry", lib.PendingKept)
	}
}

// A second rescue while the first one's tag is still queued is a rescue of its
// own: it counts from the queued target, not from the stale tag.
func TestRescueWhileATagIsQueuedCountsAgain(t *testing.T) {
	fake := newFakeServer(3)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	fake.failTags = true
	rescueOnce(t, fake, client, lib, "1")
	fake.failTags = false
	rescueOnce(t, fake, client, lib, "1")

	if got := fmt.Sprint(fake.articles["1"].tags); got != "[samling2]" {
		t.Errorf("server tags = %s, want [samling2] for two rescues", got)
	}
}

// A dry run reports the count it would set and writes nothing, locally or
// remotely; an already queued tag stays queued.
func TestKeptDryRunWritesNothing(t *testing.T) {
	fake := newFakeServer(3)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	lib.MarkRead([]library.Bookmark{lib.Bookmarks["1"]}, time.Now().Add(-time.Hour))
	lib.QueueKept("2", 3)
	fake.save("1")

	mustDrain(t, client, lib, syncOptions{Folder: library.Unread, DryRun: true})
	mustMarkKept(t, client, lib, true)

	if len(fake.tagWrites) != 0 || fake.tagListCalls != 0 {
		t.Errorf("dry run made %d tag writes and %d tag reads", len(fake.tagWrites), fake.tagListCalls)
	}
	if fmt.Sprint(lib.PendingKept) != "map[2:3]" {
		t.Errorf("dry run changed the tag queue to %v", lib.PendingKept)
	}
}

// A queued tag for an article that has since left the mirror is dropped
// rather than retried forever.
func TestQueuedTagForAGoneArticleIsDropped(t *testing.T) {
	fake := newFakeServer(3)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	lib.QueueKept("2", 1)
	fake.deleteElsewhere("2")
	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)

	if len(lib.PendingKept) != 0 || len(fake.tagWrites) != 0 {
		t.Errorf("queue %v, writes %+v", lib.PendingKept, fake.tagWrites)
	}
}

func TestKeptSwap(t *testing.T) {
	ids := map[string]int64{"samling1": 11, "samling2": 12, "samling4": 14, "samling": 99}
	for _, tc := range []struct {
		tags        []string
		target      int
		add, remove string
	}{
		{nil, 1, "[samling1]", "[]"},
		{[]string{"ifttt", "samling2"}, 3, "[samling3]", "[12]"},
		{[]string{"samling1", "samling4"}, 5, "[samling5]", "[11 14]"},
		{[]string{"samling3"}, 3, "[]", "[]"},                                 // already done
		{[]string{"samling", "samling02", "Samling2"}, 1, "[samling1]", "[]"}, // not ours
	} {
		add, remove := keptSwap(tc.tags, tc.target, ids)
		if fmt.Sprint(add) != tc.add || fmt.Sprint(remove) != tc.remove {
			t.Errorf("keptSwap(%v, %d) = %v, %v; want %s, %s", tc.tags, tc.target, add, remove, tc.add, tc.remove)
		}
	}
}

// pick and list print describe() under each article.
func TestDescribeShowsKeptCount(t *testing.T) {
	b := library.Bookmark{URL: "https://example.com/x", Tags: []string{"ifttt", "samling3"}}
	if got := describe(b); !strings.HasSuffix(got, " · kept ×3") {
		t.Errorf("describe = %q, want it to end in kept ×3", got)
	}
	b.Tags = []string{"ifttt"}
	if got := describe(b); strings.Contains(got, "kept") {
		t.Errorf("describe = %q for an article never kept", got)
	}
}
