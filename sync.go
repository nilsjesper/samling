package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/nilsjesper/samling/internal/instapaper"
	"github.com/nilsjesper/samling/internal/library"
)

// haveWarnThreshold is where the "have" parameter gets big enough to be worth
// mentioning, since it all has to fit in one POST body.
const haveWarnThreshold = 100_000

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	folder := fs.String("folder", "unread", "Instapaper folder to mirror: unread, starred, archive, or a numeric id")
	dryRun := fs.Bool("dry-run", false, "report what would happen without changing anything")
	concurrency := fs.Int("concurrency", 4, "parallel archive requests")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *concurrency < 1 {
		return errors.New("--concurrency must be at least 1")
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
	if err := drain(ctx, client, lib, *folder, *dryRun); err != nil {
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
				err := client.Archive(ctx, e.ID)

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

// drain refreshes the local mirror of a folder.
//
// The API has no pagination. bookmarks/list applies its 500-item limit first
// and only then subtracts the ids passed in "have", so a folder can never be
// read past its newest 500 entries -- see instapaper.MaxListLimit.
//
// So the mirror is cumulative rather than a snapshot. Each sync asks for the
// current window minus everything already known, and adds whatever is new.
// Because archiving an article removes it from Unread, the window slides as
// you read, and each subsequent sync uncovers a little more of the backlog.
func drain(ctx context.Context, client *instapaper.Client, lib *library.Library, folder string, dryRun bool) error {
	have := lib.HaveParam()
	if len(have) > haveWarnThreshold {
		fmt.Fprintf(os.Stderr,
			"note: the delta-sync parameter is %s characters; if Instapaper starts rejecting\n"+
				"      requests, this is why -- see the README.\n", comma(len(have)))
	}

	fmt.Printf("Fetching %s...\n", folder)

	res, err := client.List(ctx, folder, instapaper.MaxListLimit, have)
	if err != nil {
		return err
	}

	// Classify everything the server considers unread.
	var (
		fresh    []library.Bookmark // not seen before
		refresh  []library.Bookmark // already mirrored
		rescued  []library.Bookmark // re-saved by hand; must not be archived
		stillDue int                // picked, not re-saved, still to archive
	)
	for _, ib := range res.Bookmarks {
		id := ib.BookmarkID.String()
		if id == "" {
			continue
		}
		b := library.Bookmark{
			ID:          id,
			URL:         ib.URL,
			Title:       ib.Title,
			Description: ib.Description,
			Hash:        ib.Hash,
			Time:        ib.Time.Int64(),
			Progress:    ib.Progress.Float64(),
			Starred:     ib.Starred.Bool(),
			Folder:      folder,
		}

		switch e, pending := lib.Pending(id); {
		case pending && b.Time > e.PickedAt:
			// Saved again after we handed it to the browser: the triage
			// gesture for "actually, I want to read this". Keep it.
			rescued = append(rescued, b)
		case pending:
			// Still sitting where we left it, awaiting archive.
			stillDue++
		case lib.IsArchived(id):
			// Tombstoned but back in Unread, so it was re-saved or unarchived
			// deliberately. The user's action outranks our bookkeeping.
			rescued = append(rescued, b)
		default:
			if _, known := lib.Bookmarks[id]; known {
				refresh = append(refresh, b)
			} else {
				fresh = append(fresh, b)
			}
		}
	}

	// delete_ids reports ids from "have" that the server did not find in the
	// folder -- but "not found" and "below the 500-item cut" are indistinguishable
	// from here, so it is only safe to act on when everything we know about fits
	// inside the window with room to spare.
	//
	// The margin matters. An earlier version trusted delete_ids at held == 500,
	// and a single re-saved article rejoining the top of Unread pushed the last
	// article out of the window, which came back as a delete_id and evicted a
	// perfectly live article from the mirror.
	held := len(lib.Bookmarks) + len(lib.Read)
	trustDeletes := held+len(res.Bookmarks) < instapaper.MaxListLimit
	var deleteIDs []string
	if trustDeletes {
		for _, id := range res.DeleteIDs {
			if s := id.String(); s != "" {
				deleteIDs = append(deleteIDs, s)
			}
		}
	}

	if dryRun {
		fmt.Printf("Would add %s and refresh %s.\n",
			plural(len(fresh), "new article", "new articles"),
			plural(len(refresh), "article", "articles"))
		if len(rescued) > 0 {
			fmt.Printf("Would rescue %s re-saved since being picked:\n", plural(len(rescued), "article", "articles"))
			for _, b := range rescued {
				fmt.Printf("  %s\n", titleOf(b))
			}
		}
		if len(deleteIDs) > 0 {
			fmt.Printf("Would drop %s no longer in %s.\n", plural(len(deleteIDs), "article", "articles"), folder)
		}
		if !trustDeletes {
			fmt.Printf("Would not act on delete_ids: the mirror (%s) is larger than the %d-item window.\n",
				comma(held), instapaper.MaxListLimit)
		}
		fmt.Printf("Local mirror currently holds %s unread, %s awaiting archive.\n",
			comma(len(lib.Bookmarks)), comma(stillDue))
		reportCeiling(len(res.Bookmarks), folder)
		return nil
	}

	for _, b := range fresh {
		lib.Upsert(b)
	}
	for _, b := range refresh {
		lib.Upsert(b)
	}
	for _, b := range rescued {
		lib.Rescue(b)
	}
	dropped := 0
	for _, id := range deleteIDs {
		if _, ok := lib.Bookmarks[id]; ok {
			delete(lib.Bookmarks, id)
			dropped++
		}
	}

	fmt.Printf("  %s new", plural(len(fresh), "article", "articles"))
	if len(refresh) > 0 {
		fmt.Printf(", %s refreshed", comma(len(refresh)))
	}
	if dropped > 0 {
		fmt.Printf(", %s no longer in %s", plural(dropped, "article", "articles"), folder)
	}
	fmt.Println()

	if len(rescued) > 0 {
		fmt.Printf("  kept %s you re-saved:\n", plural(len(rescued), "article", "articles"))
		for _, b := range rescued {
			fmt.Printf("    %s\n", titleOf(b))
		}
	}

	reportCeiling(len(res.Bookmarks), folder)
	return nil
}

// reportCeiling explains the 500-item wall the first time a sync hits it, so a
// backlog that stops growing does not look like a bug.
func reportCeiling(returned int, folder string) {
	if returned < instapaper.MaxListLimit {
		return
	}
	fmt.Printf("\nInstapaper only exposes the newest %d items of %s, and offers no way\n"+
		"to page past that. As you archive what you have read, older articles move\n"+
		"into view and the next sync will pick them up.\n", instapaper.MaxListLimit, folder)
}
