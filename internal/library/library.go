// Package library holds the local mirror of an Instapaper reading list.
//
// The design follows pickpocket's key insight: picking an article is a purely
// local, offline, instant operation, and all network traffic is deferred to an
// explicit sync. Unlike pickpocket, nothing here destroys remote data — picked
// articles are archived on the next sync, and a tombstone list makes sure an
// article is never served up twice.
package library

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nilsjesper/samling/internal/config"
)

// Version is the schema version of the on-disk file.
//
// v1 mirrored at most the newest 500 articles per folder through API v1 and
// stored each one's "hash" for that API's delta sync. v2 mirrors whole folders
// through API v2, adds tags, and keeps a change-feed cursor. A v1 file loads
// with no cursor, which forces a full sync; pending picks and tombstones carry
// over untouched, since bookmark ids are the same number in both APIs.
const Version = 2

// Unread is the local name of Instapaper's home list. The API calls it "home";
// samling has always called it unread, and filters stored that way.
const Unread = "unread"

// Bookmark is an article in the local mirror.
type Bookmark struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Time        int64    `json:"time"`
	Progress    float64  `json:"progress,omitempty"`
	Starred     bool     `json:"starred,omitempty"` // "liked" in API v2
	Folder      string   `json:"folder,omitempty"`  // Unread or a numeric folder id
	Tags        []string `json:"tags,omitempty"`
}

// InFolder reports whether b is mirrored from folder. Bookmarks written before
// Folder was always set count as unread.
func (b Bookmark) InFolder(folder string) bool {
	if b.Folder == "" {
		return folder == Unread
	}
	return b.Folder == folder
}

// Host returns the bookmark's hostname with any leading "www." removed.
func (b Bookmark) Host() string {
	u, err := url.Parse(b.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// Age is how long ago the article was saved to Instapaper.
func (b Bookmark) Age() time.Duration {
	if b.Time <= 0 {
		return 0
	}
	return time.Since(time.Unix(b.Time, 0))
}

// keptTag matches the tags sync uses to count rescues: samling1, samling2, ...
// Strict on purpose, so that a tag like "samling" or "samling02" the user made
// by hand is never mistaken for a count, and never removed by a swap.
var keptTag = regexp.MustCompile(`^samling([1-9][0-9]*)$`)

// KeptTag is the tag that marks an article kept through n triages.
func KeptTag(n int) string { return "samling" + strconv.Itoa(n) }

// KeptCount parses a tag name written by KeptTag.
func KeptCount(tag string) (int, bool) {
	m := keptTag.FindStringSubmatch(tag)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false // too many digits to be ours
	}
	return n, true
}

// Kept is how many times the article has been re-saved after a pick, as its
// samlingN tag records; zero when it has none. An article should carry at most
// one such tag, but if it somehow has several the highest wins.
func (b Bookmark) Kept() int {
	kept := 0
	for _, t := range b.Tags {
		if n, ok := KeptCount(t); ok && n > kept {
			kept = n
		}
	}
	return kept
}

// ReadEntry is an article picked locally but not yet archived remotely.
type ReadEntry struct {
	Bookmark
	PickedAt int64  `json:"picked_at"`
	Batch    string `json:"batch"`
}

// Library is the whole on-disk state.
type Library struct {
	Version   int                  `json:"version"`
	LastSync  int64                `json:"last_sync,omitempty"`
	Bookmarks map[string]Bookmark  `json:"bookmarks"`
	Read      map[string]ReadEntry `json:"read"`
	Archived  []string             `json:"archived"`
	LastBatch string               `json:"last_batch,omitempty"`

	// Cursor is the `since` value for the next incremental sync: a unix time
	// shortly before the last successful one started. Zero means no sync has
	// completed under this schema, so the next one must be a full read.
	Cursor int64 `json:"cursor,omitempty"`
	// Synced lists the folders that have had a full read since Cursor was
	// first set, and so are kept current by the change feed. A folder not in
	// here has never been read whole and needs a full sync before the feed's
	// deltas mean anything for it.
	Synced []string `json:"synced_folders,omitempty"`

	// PendingKept maps a rescued article's id to the samlingN count it should
	// carry, until a sync manages to write that tag. Storing the target rather
	// than "add one" keeps a retry idempotent. The field is optional, so the
	// schema version stays at 2: a build that predates it just drops the queue
	// and leaves those tags unwritten.
	PendingKept map[string]int `json:"pending_kept,omitempty"`

	archived map[string]struct{} // index over Archived, built on load
}

// New returns an empty library.
func New() *Library {
	return &Library{
		Version:   Version,
		Bookmarks: map[string]Bookmark{},
		Read:      map[string]ReadEntry{},
		Archived:  []string{},
		archived:  map[string]struct{}{},
	}
}

// DefaultPath is where the library lives.
func DefaultPath() string { return config.Path("library.json") }

// Load reads the library from path, returning an empty one if it does not exist.
func Load(path string) (*Library, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var l Library
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if l.Version > Version {
		return nil, fmt.Errorf("%s was written by a newer samling (schema v%d, this build understands v%d)",
			path, l.Version, Version)
	}
	if l.Version < 2 {
		l.migrateV1()
	}
	l.normalize()
	return &l, nil
}

// migrateV1 upgrades a schema-v1 library in memory; the next Save writes v2.
//
// The unread mirror, pending picks and tombstones all stay: ids did not change
// between API versions. What goes is anything v2 sync can no longer maintain.
// v1 could mirror the "starred" and "archive" pseudo-folders, which v2 sync
// does not (starred is an attribute now, and archive is not a reading list),
// so those entries would otherwise sit in the mirror forever, never refreshed
// and never removed. The obsolete "hash" field is dropped by the decoder.
func (l *Library) migrateV1() {
	for id, b := range l.Bookmarks {
		if b.Folder == "starred" || b.Folder == "archive" {
			delete(l.Bookmarks, id)
		}
	}
	l.Cursor = 0
	l.Synced = nil
}

// IsSynced reports whether folder has had a full read that the change feed
// has kept current since.
func (l *Library) IsSynced(folder string) bool {
	for _, f := range l.Synced {
		if f == folder {
			return true
		}
	}
	return false
}

// MarkSynced records a completed full read of folder.
func (l *Library) MarkSynced(folder string) {
	if !l.IsSynced(folder) {
		l.Synced = append(l.Synced, folder)
		sort.Strings(l.Synced)
	}
}

// Drop removes an article from the mirror and from the pending set without
// tombstoning it: it is gone from Instapaper, or out of every mirrored folder,
// so there is nothing left to archive and nothing to suppress.
func (l *Library) Drop(id string) {
	delete(l.Bookmarks, id)
	delete(l.Read, id)
}

// Save writes the library atomically.
func (l *Library) Save(path string) error {
	l.normalize()
	sort.Strings(l.Archived)
	return config.WriteJSON(path, l)
}

func (l *Library) normalize() {
	l.Version = Version
	if l.Bookmarks == nil {
		l.Bookmarks = map[string]Bookmark{}
	}
	if l.Read == nil {
		l.Read = map[string]ReadEntry{}
	}
	if l.Archived == nil {
		l.Archived = []string{}
	}
	l.archived = make(map[string]struct{}, len(l.Archived))
	for _, id := range l.Archived {
		l.archived[id] = struct{}{}
	}
}

// IsArchived reports whether an article has already been archived remotely.
func (l *Library) IsArchived(id string) bool {
	if l.archived == nil {
		l.normalize()
	}
	_, ok := l.archived[id]
	return ok
}

// MarkArchived records that an article was successfully archived remotely and
// drops it from the pending-read set.
func (l *Library) MarkArchived(id string) {
	if l.archived == nil {
		l.normalize()
	}
	delete(l.Read, id)
	delete(l.Bookmarks, id)
	if _, ok := l.archived[id]; !ok {
		l.archived[id] = struct{}{}
		l.Archived = append(l.Archived, id)
	}
}

// Upsert adds or updates an unread bookmark, ignoring anything already archived
// or currently pending archive.
func (l *Library) Upsert(b Bookmark) {
	if l.IsArchived(b.ID) {
		return
	}
	if _, pending := l.Read[b.ID]; pending {
		return
	}
	l.Bookmarks[b.ID] = b
}

// Rescue restores an article to unread, clearing any tombstone and dropping it
// from the pending-archive set.
//
// This is what happens when an article we archived (or were about to) turns up
// in Instapaper's Unread folder again. Re-saving a URL reuses its bookmark id,
// so the triage habit of "open a batch, re-save the keepers" would otherwise be
// invisible here: the keeper would stay tombstoned and never be served again,
// or worse, get archived by the sync that follows. A deliberate re-save
// outranks our bookkeeping.
func (l *Library) Rescue(b Bookmark) {
	if l.archived == nil {
		l.normalize()
	}
	delete(l.Read, b.ID)
	if _, tombstoned := l.archived[b.ID]; tombstoned {
		delete(l.archived, b.ID)
		for i, id := range l.Archived {
			if id == b.ID {
				l.Archived = append(l.Archived[:i], l.Archived[i+1:]...)
				break
			}
		}
	}
	l.Bookmarks[b.ID] = b
}

// Pending returns the pending-archive entry for an id, if there is one.
func (l *Library) Pending(id string) (ReadEntry, bool) {
	e, ok := l.Read[id]
	return e, ok
}

// Entry returns the mirrored or pending-archive copy of an article.
func (l *Library) Entry(id string) (Bookmark, bool) {
	if b, ok := l.Bookmarks[id]; ok {
		return b, true
	}
	if e, ok := l.Read[id]; ok {
		return e.Bookmark, true
	}
	return Bookmark{}, false
}

// SetTags replaces the tags of the mirrored or pending-archive copy of an
// article, whichever exists.
func (l *Library) SetTags(id string, tags []string) {
	if b, ok := l.Bookmarks[id]; ok {
		b.Tags = tags
		l.Bookmarks[id] = b
	}
	if e, ok := l.Read[id]; ok {
		e.Tags = tags
		l.Read[id] = e
	}
}

// QueueKept records that an article should be tagged samling<n>.
func (l *Library) QueueKept(id string, n int) {
	if l.PendingKept == nil {
		l.PendingKept = map[string]int{}
	}
	l.PendingKept[id] = n
}

// KeptDone clears an article from the tag queue.
func (l *Library) KeptDone(id string) {
	delete(l.PendingKept, id)
	if len(l.PendingKept) == 0 {
		l.PendingKept = nil
	}
}

// MostKept returns up to n unread articles with a samlingN tag, highest count
// first.
func (l *Library) MostKept(n int) []Bookmark {
	var out []Bookmark
	for _, b := range l.Bookmarks {
		if b.Kept() > 0 {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ki, kj := out[i].Kept(), out[j].Kept(); ki != kj {
			return ki > kj
		}
		return out[i].ID < out[j].ID
	})
	if n >= 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Filter narrows which bookmarks are eligible to be picked.
type Filter struct {
	Folder    string        // match Bookmark.Folder exactly; empty matches all
	OlderThan time.Duration // saved at least this long ago
	NewerThan time.Duration // saved at most this long ago
	Domain    string        // hostname suffix match, e.g. "nytimes.com"
	Starred   *bool         // nil matches all
}

// Match reports whether b satisfies the filter.
func (f Filter) Match(b Bookmark) bool {
	if f.Folder != "" && b.Folder != f.Folder {
		return false
	}
	if f.Starred != nil && b.Starred != *f.Starred {
		return false
	}
	if f.OlderThan > 0 && b.Age() < f.OlderThan {
		return false
	}
	if f.NewerThan > 0 && (b.Age() > f.NewerThan || b.Time <= 0) {
		return false
	}
	if f.Domain != "" {
		host, want := b.Host(), strings.ToLower(strings.TrimPrefix(f.Domain, "www."))
		if host != want && !strings.HasSuffix(host, "."+want) {
			return false
		}
	}
	return true
}

// Candidates returns the unread bookmarks matching f, in a stable order so that
// a given seed always produces the same shuffle.
func (l *Library) Candidates(f Filter) []Bookmark {
	out := make([]Bookmark, 0, len(l.Bookmarks))
	for _, b := range l.Bookmarks {
		if f.Match(b) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Pick selects up to n distinct matching bookmarks at random. It does not
// mutate the library; call MarkRead to commit a pick.
func (l *Library) Pick(n int, f Filter, rng *rand.Rand) []Bookmark {
	c := l.Candidates(f)
	if n > len(c) {
		n = len(c)
	}
	if n <= 0 {
		return nil
	}
	// Partial Fisher-Yates: shuffle only the prefix we need, so results are
	// distinct without shuffling a 20,000-element slice to pick three.
	for i := 0; i < n; i++ {
		j := i + rng.IntN(len(c)-i)
		c[i], c[j] = c[j], c[i]
	}
	return c[:n]
}

// MarkRead moves bookmarks out of unread and into the pending-archive set,
// tagging them with a shared batch id so the pick can be undone. It returns the
// batch id.
func (l *Library) MarkRead(bs []Bookmark, now time.Time) string {
	if len(bs) == 0 {
		return ""
	}
	batch := fmt.Sprintf("b-%d", now.Unix())
	for _, b := range bs {
		delete(l.Bookmarks, b.ID)
		l.Read[b.ID] = ReadEntry{Bookmark: b, PickedAt: now.Unix(), Batch: batch}
	}
	l.LastBatch = batch
	return batch
}

// Undo reverses the most recent pick batch, provided it has not been synced
// yet. It returns the restored bookmarks.
func (l *Library) Undo() ([]Bookmark, error) {
	if l.LastBatch == "" {
		return nil, fmt.Errorf("nothing to undo")
	}
	var restored []Bookmark
	for id, e := range l.Read {
		if e.Batch == l.LastBatch {
			l.Bookmarks[id] = e.Bookmark
			delete(l.Read, id)
			restored = append(restored, e.Bookmark)
		}
	}
	if len(restored) == 0 {
		return nil, fmt.Errorf("the last pick (%s) was already archived; use Instapaper to unarchive it", l.LastBatch)
	}
	sort.Slice(restored, func(i, j int) bool { return restored[i].ID < restored[j].ID })
	l.LastBatch = ""
	return restored, nil
}

// PendingArchive lists the articles waiting to be archived remotely, oldest
// pick first.
func (l *Library) PendingArchive() []ReadEntry {
	out := make([]ReadEntry, 0, len(l.Read))
	for _, e := range l.Read {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PickedAt != out[j].PickedAt {
			return out[i].PickedAt < out[j].PickedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Stats summarizes the library for `samling status`.
type Stats struct {
	Unread   int
	Pending  int
	Archived int
	LastSync time.Time
	Domains  []DomainCount
	Oldest   *Bookmark
}

// DomainCount is a per-host unread tally.
type DomainCount struct {
	Host  string
	Count int
}

// Stats computes the summary, including the top domains by unread count.
func (l *Library) Stats(topDomains int) Stats {
	s := Stats{
		Unread:   len(l.Bookmarks),
		Pending:  len(l.Read),
		Archived: len(l.Archived),
	}
	if l.LastSync > 0 {
		s.LastSync = time.Unix(l.LastSync, 0)
	}

	counts := map[string]int{}
	for _, b := range l.Bookmarks {
		if h := b.Host(); h != "" {
			counts[h]++
		}
		if b.Time > 0 && (s.Oldest == nil || b.Time < s.Oldest.Time) {
			cp := b
			s.Oldest = &cp
		}
	}
	for h, n := range counts {
		s.Domains = append(s.Domains, DomainCount{Host: h, Count: n})
	}
	sort.Slice(s.Domains, func(i, j int) bool {
		if s.Domains[i].Count != s.Domains[j].Count {
			return s.Domains[i].Count > s.Domains[j].Count
		}
		return s.Domains[i].Host < s.Domains[j].Host
	})
	if topDomains > 0 && len(s.Domains) > topDomains {
		s.Domains = s.Domains[:topDomains]
	}
	return s
}
