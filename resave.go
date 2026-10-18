package main

import (
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/nilsjesper/samling/internal/library"
)

// Instapaper reuses a bookmark id on a re-save only when the URL is
// identical. Old articles often redirect -- http to https, a www added or
// dropped, a whole new path -- and the browser extension saves where the
// redirect lands, so Instapaper makes a new bookmark (seen live: two of three
// re-saves in one triage did this). By id alone that looks like a fresh
// article plus a pick still due, and the keeper's rescue would go unreported
// and its samlingN count would start over.
//
// So fresh bookmarks saved after a pick are matched back to it: same host,
// and either the same address once scheme, www, query and trailing slash are
// ignored, or titles where one contains the other once case, punctuation and
// a " | Site" suffix are ignored.

// minTitleMatch is the shortest normalized title that may match by
// containment. Below it, titles like "Notes" or "Update" would pair up
// unrelated articles from the same site.
const minTitleMatch = 12

// newAddress is a pick that came back as a new bookmark.
type newAddress struct {
	old library.ReadEntry
	new library.Bookmark
}

// matchNewAddresses moves fresh bookmarks that are re-saves of pending picks
// out of p.fresh and into p.rescued, and records the pairing in p.moved. The
// old copy stays pending: it is archived as usual, so the pool never holds the
// article twice. The new one carries the count on from the old one.
func matchNewAddresses(lib *library.Library, p *plan) {
	if len(p.fresh) == 0 || len(lib.Read) == 0 {
		return
	}
	rescued := make(map[string]bool, len(p.rescued))
	for _, b := range p.rescued {
		rescued[b.ID] = true
	}
	byHost := map[string][]library.ReadEntry{}
	for id, e := range lib.Read {
		if h := e.Host(); h != "" && !rescued[id] {
			byHost[h] = append(byHost[h], e)
		}
	}
	if len(byHost) == 0 {
		return
	}

	type pair struct {
		pick, fresh int // indexes into picks and p.fresh
		sameURL     bool
		closeness   float64 // shorter title over longer, 1 for identical
	}
	var picks []library.ReadEntry
	pickIndex := map[string]int{}
	var pairs []pair
	for fi, f := range p.fresh {
		for _, e := range byHost[f.Host()] {
			if f.Time <= e.PickedAt {
				continue // saved before the pick, so not a re-save of it
			}
			sameURL := normalURL(f.URL) != "" && normalURL(f.URL) == normalURL(e.URL)
			closeness := titleMatch(f.Title, e.Title)
			if !sameURL && closeness == 0 {
				continue
			}
			pi, ok := pickIndex[e.ID]
			if !ok {
				pi = len(picks)
				pickIndex[e.ID] = pi
				picks = append(picks, e)
			}
			pairs = append(pairs, pair{pi, fi, sameURL, closeness})
		}
	}
	if len(pairs) == 0 {
		return
	}

	// Best pairs first: an address match outranks a title match, then the
	// closer title wins. Ties fall to the lower ids, to stay deterministic.
	sort.Slice(pairs, func(i, j int) bool {
		a, b := pairs[i], pairs[j]
		if a.sameURL != b.sameURL {
			return a.sameURL
		}
		if a.closeness != b.closeness {
			return a.closeness > b.closeness
		}
		if picks[a.pick].ID != picks[b.pick].ID {
			return picks[a.pick].ID < picks[b.pick].ID
		}
		return p.fresh[a.fresh].ID < p.fresh[b.fresh].ID
	})
	pickTaken := make([]bool, len(picks))
	freshTaken := make([]bool, len(p.fresh))
	for _, pr := range pairs {
		if pickTaken[pr.pick] || freshTaken[pr.fresh] {
			continue
		}
		pickTaken[pr.pick], freshTaken[pr.fresh] = true, true
		old, b := picks[pr.pick], p.fresh[pr.fresh]
		p.moved = append(p.moved, newAddress{old: old, new: b})
		p.rescued = append(p.rescued, b)
		if p.kept == nil {
			p.kept = map[string]int{}
		}
		p.kept[b.ID] = max(old.Kept(), lib.PendingKept[old.ID], b.Kept(), lib.PendingKept[b.ID]) + 1
	}

	fresh := p.fresh[:0]
	for i, b := range p.fresh {
		if !freshTaken[i] {
			fresh = append(fresh, b)
		}
	}
	p.fresh = fresh
}

// movedFrom returns the pick a rescued bookmark was re-saved from, if it came
// back at a new address.
func (p *plan) movedFrom(id string) (library.ReadEntry, bool) {
	for _, m := range p.moved {
		if m.new.ID == id {
			return m.old, true
		}
	}
	return library.ReadEntry{}, false
}

// normalURL reduces an address to host and path: no scheme, no www, no
// query or fragment, no trailing slash. Host comparison is case-insensitive;
// paths are left as they are.
func normalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return host + strings.TrimRight(u.EscapedPath(), "/")
}

// titleMatch reports how alike two titles are: 0 when neither normalized
// title contains the other as whole words (or the shorter is under
// minTitleMatch), otherwise the shorter's length over the longer's.
func titleMatch(a, b string) float64 {
	a, b = normalTitle(a), normalTitle(b)
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) < minTitleMatch || !strings.Contains(" "+b+" ", " "+a+" ") {
		return 0
	}
	return float64(len(a)) / float64(len(b))
}

// normalTitle lowercases a title, drops a trailing " | Site" or " - Site"
// that sites append in <title>, and folds punctuation to single spaces, so
// "Raw and Red Hot | Harvard Magazine" and "Raw and Red-Hot" agree.
func normalTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, sep := range []string{" | ", " - ", " – ", " — "} {
		if i := strings.LastIndex(s, sep); i > 0 {
			s = s[:i]
			break
		}
	}
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
