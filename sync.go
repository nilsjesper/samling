package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/nilsjesper/samling/internal/instapaper"
	"github.com/nilsjesper/samling/internal/library"
)

// cursorOverlap is how far before a sync's start the next change feed begins.
// It absorbs clock skew between this machine and Instapaper's servers. Reading
// a change twice is harmless, because classifying a bookmark is idempotent;
// missing one is not.
const cursorOverlap = 10 * time.Minute

// syncOptions selects what drain reads.
type syncOptions struct {
	Folder string // library.Unread or a numeric folder id
	Full   bool   // force a full read even when the change feed would do
	DryRun bool
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	folder := fs.String("folder", library.Unread, "Instapaper folder to mirror: unread, or a numeric id")
	full := fs.Bool("full", false, "re-read the whole folder instead of only what changed")
	dryRun := fs.Bool("dry-run", false, "report what would happen without changing anything")
	concurrency := fs.Int("concurrency", 4, "parallel archive requests")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *concurrency < 1 {
		return errors.New("--concurrency must be at least 1")
	}
	f, err := syncFolder(*folder)
	if err != nil {
		return err
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	path := library.DefaultPath()
	lib, err := library.Load(path)
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	// Fetch before archiving. A re-saved article looks identical to a pending
	// one except for its timestamp, so the archive pass has to know what the
	// server currently considers unread before it starts archiving.
	if err := drain(ctx, client, lib, syncOptions{Folder: f, Full: *full, DryRun: *dryRun}); err != nil {
		return err
	}
	// Tag before archiving: a rescued article whose tag write keeps failing may
	// have been picked again since, and once archived it leaves the mirror and
	// its queued tag with it.
	if err := markKept(ctx, client, lib, *dryRun); err != nil {
		return err
	}
	if err := archivePending(ctx, client, lib, path, *concurrency, *dryRun); err != nil {
		return err
	}

	if *dryRun {
		fmt.Println("\nDry run: nothing was written locally or remotely.")
		return nil
	}

	lib.LastSync = time.Now().Unix()
	if err := lib.Save(path); err != nil {
		return err
	}
	fmt.Printf("\n%s unread · %s archived\n", comma(len(lib.Bookmarks)), comma(len(lib.Archived)))
	return nil
}

// syncFolder validates --folder. v1 also accepted the "starred" and "archive"
// pseudo-folders; v2 sync drops both rather than translating them.
func syncFolder(s string) (string, error) {
	switch s {
	case library.Unread, "home", "":
		return library.Unread, nil
	case "starred", "liked":
		return "", errors.New("starred is an attribute, not a folder: sync unread and use `pick --starred`")
	case "archive":
		return "", errors.New("the archive is not mirrored: samling picks from what you have not read yet")
	}
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return "", fmt.Errorf("--folder must be unread or a numeric id from `samling folders`, not %q", s)
	}
	return s, nil
}

// archivePending archives every article picked since the last sync. Instapaper
// has no bulk action endpoint, so this is one request per article; failures
// stay in the pending set and are retried next time.
func archivePending(ctx context.Context, client *instapaper.Client, lib *library.Library, path string, concurrency int, dryRun bool) error {
	pending := lib.PendingArchive()
	if len(pending) == 0 {
		return nil
	}

	if dryRun {
		fmt.Printf("Would archive %s:\n", plural(len(pending), "article", "articles"))
		for _, e := range pending {
			fmt.Printf("  %s  %s\n", e.ID, titleOf(e.Bookmark))
		}
		return nil
	}

	fmt.Printf("Archiving %s...\n", plural(len(pending), "article", "articles"))

	var (
		mu       sync.Mutex // guards lib and the counters below
		done     int
		failed   int
		firstErr error
		wg       sync.WaitGroup
	)
	jobs := make(chan library.ReadEntry)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				_, err := client.Move(ctx, e.ID, instapaper.SectionArchive)

				mu.Lock()
				switch {
				case err == nil:
					lib.MarkArchived(e.ID)
					done++
					// Persist periodically so Ctrl-C mid-sync keeps its progress.
					if done%25 == 0 {
						if serr := lib.Save(path); serr != nil && firstErr == nil {
							firstErr = serr
						}
					}
				default:
					failed++
					if firstErr == nil {
						firstErr = err
					}
					fmt.Fprintf(os.Stderr, "  could not archive %s (%s): %v\n",
						e.ID, titleOf(e.Bookmark), err)
				}
				mu.Unlock()
			}
		}()
	}

feed:
	for _, e := range pending {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- e:
		}
	}
	close(jobs)
	wg.Wait()

	if err := lib.Save(path); err != nil {
		return err
	}

	fmt.Printf("  archived %d", done)
	if failed > 0 {
		fmt.Printf(", %d failed (left pending, will retry next sync)", failed)
	}
	fmt.Println()

	if ctx.Err() != nil {
		return fmt.Errorf("interrupted after archiving %d of %d (progress saved)", done, len(pending))
	}
	// A single failure is not fatal — the drain half is still worth running —
	// but a total wipeout usually means the token or network is broken.
	if done == 0 && failed > 0 {
		return fmt.Errorf("every archive request failed: %w", firstErr)
	}
	return nil
}

// markKept writes the samlingN tags that rescues queued, so that clicking
// samling4 in Instapaper shows everything kept through four triages. It also
// takes the samlingN off the old copy of a keeper that came back at a new
// address, so the survivor isn't listed twice under its tag.
//
// It runs after the drain plan is applied, so each article's tags are as fresh
// as this sync can make them. A failed write is reported and stays queued for
// the next sync; it never fails this one, since the tag is a record of the
// rescue and not the rescue itself. Only ctx cancellation is returned.
//
// Each write is one request that removes every samlingN tag the article has
// and adds samling<target>. remove_tags takes ids, which the mirror does not
// keep, so they come from one GET /tags per sync, made only when an old
// samlingN has to come off. Tag names are unique per account, so a name is enough
// to find its id. Rescues are a handful per sync, so the writes go one at a
// time.
func markKept(ctx context.Context, client *instapaper.Client, lib *library.Library, dryRun bool) error {
	var todo []string
	for id := range lib.PendingKept {
		if _, ok := lib.Entry(id); ok {
			todo = append(todo, id)
		} else if !dryRun {
			// Deleted, archived elsewhere, or moved out of every mirrored
			// folder since it was rescued. Its tags are no longer visible
			// here, so a swap cannot be worked out; let it go.
			lib.KeptDone(id)
		}
	}
	var untag []string
	for id := range lib.PendingUntag {
		untag = append(untag, id)
	}
	if len(todo) == 0 && len(untag) == 0 {
		return nil
	}
	sort.Strings(todo)
	sort.Strings(untag)

	if dryRun {
		if len(todo) > 0 {
			fmt.Printf("Would retry marking %s as kept:\n", plural(len(todo), "article", "articles"))
		}
		for _, id := range todo {
			b, _ := lib.Entry(id)
			fmt.Printf("  %s · %s\n", titleOf(b), keptTimes(lib.PendingKept[id]))
		}
		for _, id := range untag {
			fmt.Printf("Would remove %s from %s, the old copy of a keeper\n",
				strings.Join(lib.PendingUntag[id], ", "), id)
		}
		return nil
	}

	if len(todo) > 0 {
		fmt.Printf("Marking %s as kept...\n", plural(len(todo), "article", "articles"))
	} else {
		fmt.Println("Updating samling tags...")
	}

	// Tag ids are only needed to remove an old samlingN, so a sync whose
	// rescues are all first-timers never asks for them.
	var ids map[string]int64
	tagIDs := func() (map[string]int64, error) {
		if ids != nil {
			return ids, nil
		}
		tags, err := client.Tags(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading your tags: %w", err)
		}
		ids = make(map[string]int64, len(tags))
		for _, t := range tags {
			ids[t.Name] = t.ID
		}
		return ids, nil
	}

	done, failed := 0, 0
	for _, id := range todo {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b, _ := lib.Entry(id)
		target := lib.PendingKept[id]
		var lookup map[string]int64
		if needsRemoval(b.Tags, target) {
			var err error
			if lookup, err = tagIDs(); err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "  could not mark %s (%s) as kept: %v\n", id, titleOf(b), err)
				continue
			}
		}
		add, remove := keptSwap(b.Tags, target, lookup)
		if len(add) == 0 && len(remove) == 0 {
			// Already right, e.g. an earlier write whose response was lost.
			lib.KeptDone(id)
			done++
			continue
		}
		after, err := client.TagBookmark(ctx, id, add, remove)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  could not mark %s (%s) as kept: %v\n", id, titleOf(b), err)
			continue
		}
		names := make([]string, 0, len(after))
		for _, t := range after {
			names = append(names, t.Name)
		}
		// The response is the article's full tag list, so pick, list and
		// status show the new count before the next sync reads it back.
		lib.SetTags(id, names)
		lib.KeptDone(id)
		done++
	}

	// Old copies of keepers that came back at a new address: their samlingN
	// moved to the new copy, so it comes off here and each survivor appears
	// once under its tag. Only these copies lose a tag; an ordinary archived
	// pick keeps whatever it had. The names are queued rather than read from
	// the mirror because the old copy leaves it once archived.
	for _, id := range untag {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lookup, err := tagIDs()
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  could not untag old copy %s: %v\n", id, err)
			continue
		}
		var remove []int64
		for _, name := range lib.PendingUntag[id] {
			if tid, ok := lookup[name]; ok {
				remove = append(remove, tid)
			}
		}
		if len(remove) > 0 {
			_, err := client.TagBookmark(ctx, id, nil, remove)
			var apiErr *instapaper.APIError
			switch {
			case errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound:
				// Deleted since: nothing left to untag.
			case err != nil:
				failed++
				fmt.Fprintf(os.Stderr, "  could not untag old copy %s: %v\n", id, err)
				continue
			}
		}
		if e, ok := lib.Pending(id); ok {
			var left []string
			for _, t := range e.Tags {
				if _, kept := library.KeptCount(t); !kept {
					left = append(left, t)
				}
			}
			lib.SetTags(id, left)
		}
		lib.UntagDone(id)
	}

	fmt.Printf("  marked %d", done)
	if failed > 0 {
		fmt.Printf(", %d failed (left queued, will retry next sync)", failed)
	}
	fmt.Println()
	return nil
}

// keptSwap works out the tag request that leaves an article carrying exactly
// one samlingN tag, samling<target>. Other tags are never touched. A samlingN
// name with no id in ids can't be removed, and doesn't need to be: it no
// longer exists under that name on the account.
func keptSwap(current []string, target int, ids map[string]int64) (add []string, remove []int64) {
	want := library.KeptTag(target)
	has := false
	for _, t := range current {
		if _, ok := library.KeptCount(t); !ok {
			continue
		}
		if t == want {
			has = true
			continue
		}
		if id, ok := ids[t]; ok {
			remove = append(remove, id)
		}
	}
	if !has {
		add = []string{want}
	}
	return add, remove
}

// needsRemoval reports whether current has a samlingN tag other than
// samling<target>.
func needsRemoval(current []string, target int) bool {
	want := library.KeptTag(target)
	for _, t := range current {
		if _, ok := library.KeptCount(t); ok && t != want {
			return true
		}
	}
	return false
}

func keptTimes(n int) string { return fmt.Sprintf("kept ×%d", n) }

// drain brings the local mirror up to date with Instapaper.
//
// There are two ways to do that, and a sync may need both:
//
//   - A full read pages through the whole folder. The result is a snapshot, so
//     it is authoritative: anything mirrored from that folder that does not
//     come back has left it. This is how a folder gets into the mirror in the
//     first place, and what --full forces.
//   - The change feed (`since`) returns only what changed since the cursor,
//     across the whole account, plus ids deleted outright. That keeps every
//     folder that has had a full read current, in a request or two.
//
// The feed alone is enough once the requested folder has been read in full
// and a cursor exists. Otherwise the folder gets a full read, and the feed
// still runs for any other synced folder so that the cursor advancing past
// their changes does not lose them.
//
// The cursor only advances when everything succeeded, and never on a dry run.
func drain(ctx context.Context, client *instapaper.Client, lib *library.Library, o syncOptions) error {
	start := time.Now()

	// With no cursor nothing is current, whatever Synced says.
	synced := map[string]bool{}
	if lib.Cursor > 0 {
		for _, f := range lib.Synced {
			synced[f] = true
		}
	}
	full := o.Full || !synced[o.Folder]

	if full {
		p, err := readFolder(ctx, client, lib, o.Folder)
		if err != nil {
			return err
		}
		finish(lib, p, o.DryRun)
		delete(synced, o.Folder) // the snapshot covered it; the feed must not
	}
	if len(synced) > 0 {
		skip := ""
		if full {
			skip = o.Folder
		}
		p, err := readFeed(ctx, client, lib, lib.Cursor, synced, skip)
		if err != nil {
			return err
		}
		finish(lib, p, o.DryRun)
	}

	if o.DryRun {
		fmt.Printf("Local mirror currently holds %s unread, %s awaiting archive.\n",
			comma(len(lib.Bookmarks)), comma(len(lib.Read)))
		return nil
	}
	if lib.Cursor == 0 {
		lib.Synced = nil
	}
	if full {
		lib.MarkSynced(o.Folder)
	}
	lib.Cursor = start.Add(-cursorOverlap).Unix()
	return nil
}

// plan is what one read of the server means for the mirror. Building it is
// read-only, so a dry run can report it and a real run can apply it.
type plan struct {
	source   string             // what was read, for messages
	fresh    []library.Bookmark // not seen before
	refresh  []library.Bookmark // already mirrored
	rescued  []library.Bookmark // re-saved by hand; must not be archived
	kept     map[string]int     // rescued id -> the samlingN count it moves to
	moved    []newAddress       // rescued, but re-saved under a new id
	stillDue int                // picked, not re-saved, still to archive

	gone      []string            // mirrored, but no longer in any mirrored folder
	goneDue   []library.ReadEntry // picked, then archived or moved elsewhere
	deleted   []string            // deleted from Instapaper outright
	unchanged bool                // the feed came back empty
}

// readFolder pages through a whole folder and plans the mirror against it.
func readFolder(ctx context.Context, client *instapaper.Client, lib *library.Library, folder string) (*plan, error) {
	section := folder
	if folder == library.Unread {
		section = instapaper.SectionHome
	}
	fmt.Printf("Reading all of %s...\n", folder)
	bms, err := client.ListAll(ctx, section, progressLine())
	if err != nil {
		return nil, err
	}
	endProgress()

	p := &plan{source: folder}
	seen := make(map[string]bool, len(bms))
	for _, ib := range bms {
		if ib.ID == 0 || ib.Archived {
			continue
		}
		b := toLibrary(ib, folder)
		if seen[b.ID] {
			continue // offset paging shifted under us; see ListAll
		}
		seen[b.ID] = true
		classify(lib, b, p)
	}

	// The snapshot is authoritative for its folder.
	for id, b := range lib.Bookmarks {
		if b.InFolder(folder) && !seen[id] {
			p.gone = append(p.gone, id)
		}
	}
	for id, e := range lib.Read {
		if e.InFolder(folder) && !seen[id] {
			// Picked here, and now gone: archived or deleted from another
			// device. Nothing left to do remotely, so tombstone it as is.
			p.goneDue = append(p.goneDue, e)
		}
	}
	matchNewAddresses(lib, p)
	return p, nil
}

// readFeed reads the change feed since cursor and plans the mirror against it
// for the folders in synced. Changes located in skip are left alone, because a
// full read of that folder has just covered them.
func readFeed(ctx context.Context, client *instapaper.Client, lib *library.Library, cursor int64, synced map[string]bool, skip string) (*plan, error) {
	fmt.Println("Reading changes since the last sync...")
	feed, err := client.SinceAll(ctx, cursor, progressLine())
	if err != nil {
		return nil, err
	}
	endProgress()

	p := &plan{source: "the change feed"}
	if len(feed.Bookmarks) == 0 && len(feed.DeletedIDs) == 0 {
		p.unchanged = true
		return p, nil
	}

	// A bookmark changed twice while we paged can appear twice; the later
	// entry is the newer state.
	latest := make(map[string]instapaper.Bookmark, len(feed.Bookmarks))
	var order []string
	for _, ib := range feed.Bookmarks {
		if ib.ID == 0 {
			continue
		}
		id := strconv.FormatInt(ib.ID, 10)
		if _, dup := latest[id]; !dup {
			order = append(order, id)
		}
		latest[id] = ib
	}

	for _, id := range order {
		ib := latest[id]
		loc := location(ib)
		switch {
		case skip != "" && loc == skip:
		case synced[loc]:
			classify(lib, toLibrary(ib, loc), p)
		default:
			// Archived, or moved to a folder that is not mirrored. Either way
			// it is out of the pool; a pending pick needs no archive request.
			if e, pending := lib.Pending(id); pending {
				p.goneDue = append(p.goneDue, e)
			} else if _, ok := lib.Bookmarks[id]; ok {
				p.gone = append(p.gone, id)
			}
		}
	}

	for _, n := range feed.DeletedIDs {
		id := strconv.FormatInt(n, 10)
		_, mirrored := lib.Bookmarks[id]
		_, pending := lib.Pending(id)
		if mirrored || pending {
			p.deleted = append(p.deleted, id)
		}
	}
	matchNewAddresses(lib, p)
	return p, nil
}

// classify sorts one bookmark that is currently in a mirrored folder.
func classify(lib *library.Library, b library.Bookmark, p *plan) {
	switch e, pending := lib.Pending(b.ID); {
	case pending && b.Time > e.PickedAt:
		// Saved again after we handed it to the browser: the triage gesture
		// for "actually, I want to read this". Keep it.
		p.rescue(lib, b)
	case pending:
		// Still sitting where we left it, awaiting archive.
		p.stillDue++
	case lib.IsArchived(b.ID):
		// Tombstoned but back in a mirrored folder, so it was re-saved or
		// unarchived deliberately. The user's action outranks our bookkeeping.
		p.rescue(lib, b)
	default:
		if _, known := lib.Bookmarks[b.ID]; known {
			p.refresh = append(p.refresh, b)
		} else {
			p.fresh = append(p.fresh, b)
		}
	}
}

// rescue plans keeping b and bumping its samlingN count. The count starts
// from the higher of the tag the server reports and a bump still queued from
// an earlier sync, so a rescue that lands while the previous tag write is
// still failing counts as a rescue of its own rather than repeating it.
func (p *plan) rescue(lib *library.Library, b library.Bookmark) {
	p.rescued = append(p.rescued, b)
	if p.kept == nil {
		p.kept = map[string]int{}
	}
	p.kept[b.ID] = max(b.Kept(), lib.PendingKept[b.ID]) + 1
}

// location is where a bookmark lives, in the mirror's terms: library.Unread,
// a numeric folder id, or "archive", which syncFolder never lets be mirrored.
// archived wins over folder_id, since an archived bookmark is out of the pool
// wherever it was filed. liked is an attribute and does not affect it.
func location(ib instapaper.Bookmark) string {
	switch {
	case ib.Archived:
		return instapaper.SectionArchive
	case ib.FolderID == nil:
		return library.Unread
	default:
		return strconv.FormatInt(*ib.FolderID, 10)
	}
}

func toLibrary(ib instapaper.Bookmark, folder string) library.Bookmark {
	b := library.Bookmark{
		ID:          strconv.FormatInt(ib.ID, 10),
		URL:         ib.URL,
		Title:       ib.Title,
		Description: ib.Description,
		Time:        ib.Time,
		Progress:    ib.Progress.Percentage,
		Starred:     ib.Liked,
		Folder:      folder,
	}
	for _, t := range ib.Tags {
		b.Tags = append(b.Tags, t.Name)
	}
	return b
}

// finish reports a plan and, unless this is a dry run, applies it.
func finish(lib *library.Library, p *plan, dryRun bool) {
	report(p, dryRun)
	if dryRun {
		return
	}
	for _, b := range p.fresh {
		lib.Upsert(b)
	}
	for _, b := range p.refresh {
		lib.Upsert(b)
	}
	for _, b := range p.rescued {
		lib.Rescue(b)
		lib.QueueKept(b.ID, p.kept[b.ID])
	}
	for _, m := range p.moved {
		// The count now lives on the new copy; the old one is archived as
		// usual, so a tag still queued for it would only be dropped later.
		lib.KeptDone(m.old.ID)
		var counters []string
		for _, t := range m.old.Tags {
			if _, ok := library.KeptCount(t); ok {
				counters = append(counters, t)
			}
		}
		if len(counters) > 0 {
			lib.QueueUntag(m.old.ID, counters)
		}
	}
	for _, id := range p.gone {
		lib.Drop(id)
	}
	for _, e := range p.goneDue {
		lib.MarkArchived(e.ID)
	}
	for _, id := range p.deleted {
		lib.Drop(id)
	}
}

func report(p *plan, dryRun bool) {
	if p.unchanged {
		fmt.Println("  nothing changed")
		return
	}
	would := func(s string) string {
		if dryRun {
			return "would " + s
		}
		return s
	}

	fmt.Printf("  %s %s", would("add"), plural(len(p.fresh), "new article", "new articles"))
	if len(p.refresh) > 0 {
		fmt.Printf(", %s %s", would("refresh"), comma(len(p.refresh)))
	}
	if len(p.gone) > 0 {
		fmt.Printf(", %s %s no longer in %s", would("drop"),
			plural(len(p.gone), "article", "articles"), sourceFolder(p))
	}
	if len(p.deleted) > 0 {
		fmt.Printf(", %s %s deleted from Instapaper", would("forget"), plural(len(p.deleted), "article", "articles"))
	}
	fmt.Println()

	if len(p.rescued) > 0 {
		fmt.Printf("  %s %s you re-saved:\n", would("keep"), plural(len(p.rescued), "article", "articles"))
		for _, b := range p.rescued {
			mark := keptTimes(p.kept[b.ID])
			if dryRun {
				mark = "would mark as " + mark
			}
			if _, moved := p.movedFrom(b.ID); moved {
				mark += " (re-saved at a new address)"
			}
			fmt.Printf("    %s · %s\n", titleOf(b), mark)
		}
	}
	if len(p.goneDue) > 0 {
		fmt.Printf("  %s %s already archived or moved elsewhere:\n",
			would("stop tracking"), plural(len(p.goneDue), "picked article", "picked articles"))
		for _, e := range p.goneDue {
			fmt.Printf("    %s\n", titleOf(e.Bookmark))
		}
	}
	if dryRun && p.stillDue > 0 {
		fmt.Printf("  %s still due for archiving\n", plural(p.stillDue, "picked article", "picked articles"))
	}
}

func sourceFolder(p *plan) string {
	if p.source == "the change feed" {
		return "a mirrored folder"
	}
	return p.source
}

// progressLine returns a callback that keeps a running "12,000 / 23,858" on
// one terminal line. Piped output gets nothing; there the line would just be
// noise.
func progressLine() instapaper.ProgressFunc {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil
	}
	return func(got, total int) {
		fmt.Printf("\r  %s / %s", comma(got), comma(total))
	}
}

func endProgress() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Print("\r\033[K")
	}
}
