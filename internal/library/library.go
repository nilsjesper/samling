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
	"sort"
	"strings"
	"time"

	"github.com/nilsjesper/samling/internal/config"
)

// Version is the schema version of the on-disk file.
const Version = 1

// Bookmark is an article in the local mirror.
type Bookmark struct {
	ID          string  `json:"id"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Hash        string  `json:"hash,omitempty"`
	Time        int64   `json:"time"`
	Progress    float64 `json:"progress,omitempty"`
	Starred     bool    `json:"starred,omitempty"`
	Folder      string  `json:"folder,omitempty"`
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
	l.normalize()
	return &l, nil
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

// HaveParam builds the delta-sync "have" value for bookmarks/list: every id the
// client already holds, with its hash where known so the server can report
// metadata changes. Unread bookmarks, pending-archive articles and tombstones
// are all included, since none of them should come back in the response.
func (l *Library) HaveParam() string {
	parts := make([]string, 0, len(l.Bookmarks)+len(l.Read)+len(l.Archived))
	for id, b := range l.Bookmarks {
		if b.Hash != "" {
			parts = append(parts, id+":"+b.Hash)
		} else {
			parts = append(parts, id)
		}
	}
	for id := range l.Read {
		parts = append(parts, id)
	}
	parts = append(parts, l.Archived...)
	sort.Strings(parts)
	return strings.Join(parts, ",")
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
