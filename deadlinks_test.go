package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nilsjesper/samling/internal/library"
	"github.com/nilsjesper/samling/internal/linkcheck"
)

// noSuchDomain resolves everything except gone.example, without real DNS.
type noSuchDomain struct{}

func (noSuchDomain) LookupHost(_ context.Context, host string) ([]string, error) {
	if host == "gone.example" {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []string{"127.0.0.1"}, nil
}

// A dead pick is replaced by the next article in the shuffle until n are
// live, and the checks run against a fake site, never the real addresses.
func TestPickLiveRefillsDeadPicks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/live"):
		case r.URL.Path == "/paywall":
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/removed":
			w.WriteHeader(http.StatusGone)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pool := []library.Bookmark{
		{ID: "1", URL: srv.URL + "/live1"},
		{ID: "2", URL: srv.URL + "/missing"},
		{ID: "3", URL: srv.URL + "/paywall"}, // bot-blocked, not dead
		{ID: "4", URL: "https://gone.example/2012/story"},
		{ID: "5", URL: srv.URL + "/removed"},
		{ID: "6", URL: srv.URL + "/live6"},
		{ID: "7", URL: srv.URL + "/live7"},
	}
	c := linkcheck.New()
	c.Resolver = noSuchDomain{}

	live, dead, results := pickLive(context.Background(), pool, 3, c.CheckAll)
	if got := idsOf(live); got != "[1 3 6]" {
		t.Errorf("live = %s, want [1 3 6]", got)
	}
	var reasons []string
	for _, d := range dead {
		reasons = append(reasons, d.ID+": "+d.Reason)
	}
	want := "[2: HTTP 404 4: domain no longer resolves 5: HTTP 410]"
	if got := fmt.Sprint(reasons); got != want {
		t.Errorf("dead = %s, want %s", got, want)
	}
	if len(results) != 6 {
		t.Errorf("checked %d links, want 6: the seventh was never needed", len(results))
	}
}

func TestPickLiveStopsWhenThePoolRunsOut(t *testing.T) {
	pool := []library.Bookmark{{ID: "1", URL: "dead"}, {ID: "2", URL: "live"}, {ID: "3", URL: "dead"}}
	var rounds [][]string
	check := func(_ context.Context, urls []string) []linkcheck.Result {
		rounds = append(rounds, urls)
		out := make([]linkcheck.Result, len(urls))
		for i, u := range urls {
			out[i] = linkcheck.Result{URL: u, Verdict: linkcheck.Live}
			if u == "dead" {
				out[i].Verdict = linkcheck.Dead
			}
		}
		return out
	}
	live, dead, _ := pickLive(context.Background(), pool, 3, check)
	if idsOf(live) != "[2]" || len(dead) != 2 {
		t.Errorf("live %s, %d dead; want [2] and 2", idsOf(live), len(dead))
	}
	if len(rounds) != 1 {
		t.Errorf("made %d rounds of checks; the first one used up the pool", len(rounds))
	}
}

func idsOf(bs []library.Bookmark) string {
	var out []string
	for _, b := range bs {
		out = append(out, b.ID)
	}
	return fmt.Sprint(out)
}

// pick marks a dead link read and queues samling-dead; the next sync writes
// the tag before archiving it, through the same queue as samlingN.
func TestDeadPickIsTaggedAndArchivedOnSync(t *testing.T) {
	fake := newFakeServer(3)
	fake.tag("2", "ifttt")
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	lib := library.New()
	mustDrain(t, client, lib, unread)

	dead := []deadLink{{Bookmark: lib.Bookmarks["2"], Reason: "HTTP 404"}}
	lib.MarkRead([]library.Bookmark{lib.Bookmarks["1"], dead[0].Bookmark}, time.Now().Add(-time.Hour))
	queueDeadTags(lib, dead)

	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)
	if err := archivePending(context.Background(), client, lib, path, 1, false); err != nil {
		t.Fatal(err)
	}

	if len(fake.tagWrites) != 1 {
		t.Fatalf("made %d tag writes, want 1: %+v", len(fake.tagWrites), fake.tagWrites)
	}
	if w := fake.tagWrites[0]; w.id != "2" || fmt.Sprint(w.add) != "[samling-dead]" || len(w.remove) != 0 {
		t.Errorf("tag write = %+v, want only samling-dead added to 2", w)
	}
	if got := fmt.Sprint(fake.articles["2"].tags); got != "[ifttt samling-dead]" {
		t.Errorf("server tags = %s", got)
	}
	if !lib.IsArchived("1") || !lib.IsArchived("2") {
		t.Error("the dead pick and its batch should both be archived")
	}
	if lib.PendingTags != nil {
		t.Errorf("tag queue = %v after a successful write", lib.PendingTags)
	}
}

// A failed write stays queued across a restart and lands on the next sync,
// even though the article was archived in between and left the mirror.
func TestDeadTagIsRetriedAfterArchiving(t *testing.T) {
	fake := newFakeServer(2)
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	lib := library.New()
	mustDrain(t, client, lib, unread)

	dead := []deadLink{{Bookmark: lib.Bookmarks["1"], Reason: "HTTP 410"}}
	lib.MarkRead([]library.Bookmark{dead[0].Bookmark}, time.Now().Add(-time.Hour))
	queueDeadTags(lib, dead)

	fake.failTags = true
	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)
	if err := archivePending(context.Background(), client, lib, path, 1, false); err != nil {
		t.Fatal(err)
	}
	if !lib.IsArchived("1") {
		t.Fatal("a failed tag write held up archiving")
	}
	if err := lib.Save(path); err != nil {
		t.Fatal(err)
	}
	lib, err := library.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(lib.PendingTags); got != "map[1:[samling-dead]]" {
		t.Fatalf("tag queue after reload = %s", got)
	}

	fake.failTags = false
	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)
	if got := fmt.Sprint(fake.articles["1"].tags); got != "[samling-dead]" {
		t.Errorf("server tags = %s, want [samling-dead]", got)
	}
	if lib.PendingTags != nil {
		t.Errorf("tag queue = %v after the retry", lib.PendingTags)
	}

	// Gone for good by the next retry: a 404 clears the entry.
	lib.QueueTag("1", library.DeadTag+"-again")
	fake.deleteElsewhere("1")
	mustMarkKept(t, client, lib, false)
	if lib.PendingTags != nil {
		t.Errorf("tag queue = %v for a deleted article", lib.PendingTags)
	}
}

// An article with both a samlingN bump and a plain tag queued gets both in
// one request.
func TestQueuedTagsShareOneWrite(t *testing.T) {
	fake := newFakeServer(1)
	fake.tag("1", "samling1")
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	lib.QueueKept("1", 2)
	lib.QueueTag("1", library.DeadTag)
	mustMarkKept(t, client, lib, false)

	if len(fake.tagWrites) != 1 {
		t.Fatalf("made %d tag writes, want 1", len(fake.tagWrites))
	}
	if got := fmt.Sprint(fake.articles["1"].tags); got != "[samling2 samling-dead]" {
		t.Errorf("server tags = %s", got)
	}
	if lib.PendingKept != nil || lib.PendingTags != nil {
		t.Errorf("queues = %v, %v", lib.PendingKept, lib.PendingTags)
	}
}

func TestDeadTagDryRunWritesNothing(t *testing.T) {
	fake := newFakeServer(1)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)

	lib.QueueTag("1", library.DeadTag)
	mustMarkKept(t, client, lib, true)
	if len(fake.tagWrites) != 0 || lib.PendingTags == nil {
		t.Errorf("dry run wrote %d tags, queue %v", len(fake.tagWrites), lib.PendingTags)
	}
}
