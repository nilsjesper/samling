package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nilsjesper/samling/internal/library"
)

// The two re-saves a live triage missed on 2026-10-18, as Instapaper reported
// them: a redirect moved each article, so the extension's re-save made a new
// bookmark instead of bumping the old one.
const (
	harvardOld, harvardNew = "1187008497", "2050046723"
	rekhiOld, rekhiNew     = "1114381665", "2050046688"
)

func seedRedirectedPicks(fake *fakeInstapaper) {
	fake.add(harvardOld, "https://harvardmagazine.com/2019/05/inflammation-disease-diet",
		"Raw and Red-Hot", pastTime)
	fake.add(rekhiOld, "http://www.sachinrekhi.com/the-art-of-being-compelling-as-a-product-manager",
		"The Art of Being Compelling as a Product Manager", pastTime)
}

func reSaveAtNewAddresses(fake *fakeInstapaper) {
	fake.add(harvardNew, "https://www.harvardmagazine.com/2019/04/inflammation-disease-diet",
		"Raw and Red Hot | Harvard Magazine", 0)
	fake.add(rekhiNew, "https://www.sachinrekhi.com/p/the-art-of-being-compelling-as-a-product-manager",
		"The Art of Being Compelling as a Product Manager", 0)
}

func TestReSaveAtANewAddressIsKept(t *testing.T) {
	fake := newFakeServer(0)
	seedRedirectedPicks(fake)
	fake.tag(harvardOld, "samling2")
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	ctx := context.Background()

	lib := library.New()
	mustDrain(t, client, lib, unread)
	lib.MarkRead([]library.Bookmark{lib.Bookmarks[harvardOld], lib.Bookmarks[rekhiOld]}, time.Now().Add(-time.Hour))

	reSaveAtNewAddresses(fake)
	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)
	if err := archivePending(ctx, client, lib, path, 1, false); err != nil {
		t.Fatalf("archivePending: %v", err)
	}

	// The old copies are archived, so the pool has each article once.
	slices.Sort(fake.archived)
	if got := fmt.Sprint(fake.archived); got != fmt.Sprint([]string{rekhiOld, harvardOld}) {
		t.Errorf("archived %s, want both old copies", got)
	}
	for _, id := range []string{harvardNew, rekhiNew} {
		if _, ok := lib.Bookmarks[id]; !ok {
			t.Errorf("new copy %s is not unread", id)
		}
	}
	// The new copies carry the count on from the old ones, bumped.
	if got := fmt.Sprint(fake.articles[harvardNew].tags); got != "[samling3]" {
		t.Errorf("harvard new copy tags = %s, want [samling3] (old one was samling2)", got)
	}
	if got := fmt.Sprint(fake.articles[rekhiNew].tags); got != "[samling1]" {
		t.Errorf("rekhi new copy tags = %s, want [samling1]", got)
	}
	if got := lib.Bookmarks[harvardNew].Kept(); got != 3 {
		t.Errorf("mirror shows the harvard copy kept ×%d, want 3", got)
	}
	if len(lib.PendingKept) != 0 {
		t.Errorf("tag queue = %v", lib.PendingKept)
	}
}

// The same match on a full read, which is how the first sync after an
// upgrade, or sync --full, sees things.
func TestReSaveAtANewAddressIsKeptOnAFullRead(t *testing.T) {
	fake := newFakeServer(0)
	seedRedirectedPicks(fake)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)
	lib.MarkRead([]library.Bookmark{lib.Bookmarks[harvardOld]}, time.Now().Add(-time.Hour))

	reSaveAtNewAddresses(fake)
	p, err := readFolder(context.Background(), client, lib, library.Unread)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.moved) != 1 || p.moved[0].old.ID != harvardOld || p.moved[0].new.ID != harvardNew {
		t.Errorf("moved = %+v, want harvard old -> new", p.moved)
	}
	if p.kept[harvardNew] != 1 {
		t.Errorf("kept = %v", p.kept)
	}
	// The rekhi copy was never picked, so its new copy is just a new article.
	if len(p.fresh) != 1 || p.fresh[0].ID != rekhiNew {
		t.Errorf("fresh = %+v, want only the unpicked rekhi copy", p.fresh)
	}
}

// Unrelated new articles are not mistaken for re-saves: another article on
// the same site, the same title on another site, or a save from before the
// pick.
func TestUnrelatedNewArticlesAreNotReSaves(t *testing.T) {
	fake := newFakeServer(0)
	seedRedirectedPicks(fake)
	client := newFake(t, fake)
	path := filepath.Join(t.TempDir(), "library.json")
	lib := library.New()
	mustDrain(t, client, lib, unread)
	lib.MarkRead([]library.Bookmark{lib.Bookmarks[harvardOld], lib.Bookmarks[rekhiOld]}, time.Now().Add(-time.Hour))

	fake.add("3000000001", "https://www.harvardmagazine.com/2019/04/sleep-and-memory",
		"What Sleep Does for Memory | Harvard Magazine", 0)
	fake.add("3000000002", "https://medium.com/@someone/the-art-of-being-compelling-as-a-product-manager",
		"The Art of Being Compelling as a Product Manager", 0)
	mustDrain(t, client, lib, unread)
	mustMarkKept(t, client, lib, false)
	if err := archivePending(context.Background(), client, lib, path, 1, false); err != nil {
		t.Fatal(err)
	}

	if len(fake.archived) != 2 {
		t.Errorf("archived %v, want both picks", fake.archived)
	}
	if len(fake.tagWrites) != 0 {
		t.Errorf("tagged %+v", fake.tagWrites)
	}
	for _, id := range []string{"3000000001", "3000000002"} {
		if b, ok := lib.Bookmarks[id]; !ok || b.Kept() != 0 {
			t.Errorf("%s should be an ordinary new article", id)
		}
	}
}

// Each pick matches at most one new bookmark, and the address match wins
// over a title match.
func TestEachPickMatchesOnce(t *testing.T) {
	fake := newFakeServer(0)
	seedRedirectedPicks(fake)
	client := newFake(t, fake)
	lib := library.New()
	mustDrain(t, client, lib, unread)
	lib.MarkRead([]library.Bookmark{lib.Bookmarks[rekhiOld]}, time.Now().Add(-time.Hour))

	fake.add("3000000003", "https://sachinrekhi.com/the-art-of-being-compelling-as-a-product-manager/?utm_source=x",
		"Compelling", 0)
	fake.add(rekhiNew, "https://www.sachinrekhi.com/p/the-art-of-being-compelling-as-a-product-manager",
		"The Art of Being Compelling as a Product Manager", 0)
	p, err := readFeed(context.Background(), client, lib, lib.Cursor, map[string]bool{library.Unread: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.moved) != 1 || p.moved[0].new.ID != "3000000003" {
		t.Errorf("moved = %+v, want only the address match", p.moved)
	}
	if len(p.fresh) != 1 || p.fresh[0].ID != rekhiNew {
		t.Errorf("fresh = %+v", p.fresh)
	}
}

func TestNormalization(t *testing.T) {
	for _, tc := range [][2]string{
		{"https://harvardmagazine.com/2019/05/x", "harvardmagazine.com/2019/05/x"},
		{"http://WWW.Example.com/a/b/?q=1#f", "example.com/a/b"},
		{"not a url", ""},
	} {
		if got := normalURL(tc[0]); got != tc[1] {
			t.Errorf("normalURL(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
	for _, tc := range []struct {
		a, b  string
		match bool
	}{
		{"Raw and Red-Hot", "Raw and Red Hot | Harvard Magazine", true},
		{"The Art of Being Compelling", "The Art of Being Compelling as a Product Manager", true},
		{"Notes", "Notes | Some Site", false},                               // too short to trust
		{"The Art of Being", "The Artful Dodger of Being Elsewhere", false}, // not whole words
		{"Raw and Red-Hot", "Something Else Entirely", false},
	} {
		if got := titleMatch(tc.a, tc.b) > 0; got != tc.match {
			t.Errorf("titleMatch(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.match)
		}
	}
}
