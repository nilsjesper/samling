package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/nilsjesper/samling/internal/config"
	"github.com/nilsjesper/samling/internal/library"
	"github.com/nilsjesper/samling/internal/linkcheck"
)

// checkBudget bounds the time pick spends checking links, replacements
// included. Each round runs in parallel under the checker's own 5s timeout,
// so this allows one round of replacements; past it, anything unchecked is
// treated as alive, as a timeout always is.
const checkBudget = 10 * time.Second

// checkFunc checks addresses, returning results in the same order.
type checkFunc func(ctx context.Context, urls []string) []linkcheck.Result

// deadLink is a pick skipped, or for list flagged, because its link is dead.
type deadLink struct {
	library.Bookmark
	Reason string
}

// checkedPick is what checking a pick's links produced.
type checkedPick struct {
	picks []library.Bookmark // what to open (pick) or print (list)
	dead  []deadLink
	notes map[string]string // id -> a caveat to print under the article
}

// pickChecked chooses n articles matching filter and checks their links.
// With refill, as for pick, dead ones are swapped for fresh random matches
// until n are live or the pool runs out. Without it, as for list, the
// selection is exactly what an unchecked run would show, and dead links are
// only reported.
//
// A refill draws from the same shuffle: Pick's partial Fisher-Yates gives the
// same first n whether it is asked for n or for everything, so a seeded run
// still starts from the articles it always did.
func pickChecked(lib *library.Library, filter library.Filter, n int, rng *rand.Rand, refill bool) (checkedPick, error) {
	size := n
	if refill {
		size = len(lib.Bookmarks)
	}
	pool := lib.Pick(size, filter, rng)

	ctx, stop := signalContext()
	defer stop()
	budget, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()

	check := linkcheck.New().CheckAll
	var cp checkedPick
	var results []linkcheck.Result
	if refill {
		cp.picks, cp.dead, results = pickLive(budget, pool, n, check)
	} else {
		cp.picks = pool
		results = check(budget, urlsOf(pool))
		for i, r := range results {
			if r.Verdict == linkcheck.Dead {
				cp.dead = append(cp.dead, deadLink{pool[i], r.Reason})
			}
		}
	}
	// A cancelled check comes back "unknown", which would read as alive.
	if ctx.Err() != nil {
		return checkedPick{}, errors.New("interrupted while checking links; nothing was picked")
	}

	cp.notes = map[string]string{}
	byURL := map[string]linkcheck.Result{}
	for _, r := range results {
		byURL[r.URL] = r
	}
	for _, b := range cp.picks {
		if r := byURL[b.URL]; r.Homepage {
			cp.notes[b.ID] = "possibly dead: " + r.Reason
		}
	}
	logChecks(results)
	return cp, nil
}

// pickLive takes articles from the front of pool until n of them have links
// that are not dead. Each round checks just enough to fill the gap, all in
// parallel.
func pickLive(ctx context.Context, pool []library.Bookmark, n int, check checkFunc) (live []library.Bookmark, dead []deadLink, results []linkcheck.Result) {
	next := 0
	for len(live) < n && next < len(pool) {
		batch := pool[next:min(next+n-len(live), len(pool))]
		next += len(batch)
		res := check(ctx, urlsOf(batch))
		for i, r := range res {
			if r.Verdict == linkcheck.Dead {
				dead = append(dead, deadLink{batch[i], r.Reason})
			} else {
				live = append(live, batch[i])
			}
		}
		results = append(results, res...)
	}
	return live, dead, results
}

func urlsOf(bs []library.Bookmark) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.URL
	}
	return out
}

// queueDeadTags queues samling-dead for sync to write, so pick itself stays
// off the Instapaper API. The caller marks the dead picks read in the same
// batch as the live ones, so they are archived on the next sync and undone
// together.
func queueDeadTags(lib *library.Library, dead []deadLink) {
	for _, d := range dead {
		lib.QueueTag(d.ID, library.DeadTag)
	}
}

// reportDead lists the dead links with an archived copy to try instead.
func reportDead(dead []deadLink, skipped bool) {
	if len(dead) == 0 {
		return
	}
	if skipped {
		fmt.Printf("\nskipped %s\n", plural(len(dead), "dead link", "dead links"))
	} else {
		fmt.Printf("\n%s (pick would skip them)\n", plural(len(dead), "dead link", "dead links"))
	}
	for _, d := range dead {
		fmt.Printf("  %s (%s)\n    %s\n", titleOf(d.Bookmark), d.Reason, archiveLink(d.URL))
	}
	if skipped {
		fmt.Printf("They're marked read too; the next sync archives them and tags them %s.\n", library.DeadTag)
	}
}

// archiveLink is the Wayback Machine's newest copy of an address.
func archiveLink(u string) string { return "https://web.archive.org/web/" + u }

// logChecks appends every result to linkcheck.log, so the false-alarm rules
// can be tuned against real outcomes: how often a homepage redirect turns
// up, and which sites answer 403. Logging is best effort.
func logChecks(results []linkcheck.Result) {
	if len(results) == 0 {
		return
	}
	if err := config.EnsureHome(); err != nil {
		return
	}
	f, err := os.OpenFile(config.Path("linkcheck.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not log link checks: %v\n", err)
		return
	}
	defer f.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	var sb strings.Builder
	for _, r := range results {
		verdict := r.Verdict.String()
		if r.Homepage {
			verdict = "homepage"
		}
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\n", now, verdict, r.Reason, r.URL)
	}
	f.WriteString(sb.String())
}
