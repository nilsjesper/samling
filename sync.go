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

	if err := archivePending(ctx, client, lib, path, *concurrency, *dryRun); err != nil {
		return err
	}
	if err := drain(ctx, client, lib, *folder, *dryRun); err != nil {
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

	collected := make(map[string]library.Bookmark, len(res.Bookmarks))
	for _, b := range res.Bookmarks {
		id := b.BookmarkID.String()
		if id == "" {
			continue
		}
		collected[id] = library.Bookmark{
			ID:          id,
			URL:         b.URL,
			Title:       b.Title,
			Description: b.Description,
			Hash:        b.Hash,
			Time:        b.Time.Int64(),
			Progress:    b.Progress.Float64(),
			Starred:     b.Starred.Bool(),
			Folder:      folder,
		}
	}

	// delete_ids reports ids from "have" that the server did not find in the
	// folder. That is only safe to act on when the whole mirror fits inside the
	// window: past that, an id could be absent simply because it sits below the
	// 500-item cut, and evicting it would throw away a real article.
	held := len(lib.Bookmarks) + len(lib.Read)
	trustDeletes := held <= instapaper.MaxListLimit
	var deleteIDs []string
	if trustDeletes {
		for _, id := range res.DeleteIDs {
			if s := id.String(); s != "" {
				deleteIDs = append(deleteIDs, s)
			}
		}
	}

	if dryRun {
		fresh := 0
		for id := range collected {
			if _, known := lib.Bookmarks[id]; !known {
				fresh++
			}
		}
		fmt.Printf("Would add %s and refresh %s already mirrored.\n",
			plural(fresh, "new article", "new articles"),
			plural(len(collected)-fresh, "article", "articles"))
		if len(deleteIDs) > 0 {
			fmt.Printf("Would drop %s no longer in %s.\n", plural(len(deleteIDs), "article", "articles"), folder)
		}
		if !trustDeletes {
			fmt.Printf("Would not act on delete_ids: the mirror (%s) is larger than the %d-item window.\n",
				comma(held), instapaper.MaxListLimit)
		}
		fmt.Printf("Local mirror currently holds %s unread.\n", comma(len(lib.Bookmarks)))
		reportCeiling(len(res.Bookmarks), len(collected), folder)
		return nil
	}

	added := 0
	for _, b := range collected {
		if _, known := lib.Bookmarks[b.ID]; !known {
			added++
		}
		lib.Upsert(b)
	}
	dropped := 0
	for _, id := range deleteIDs {
		if _, ok := lib.Bookmarks[id]; ok {
			delete(lib.Bookmarks, id)
			dropped++
		}
	}

	fmt.Printf("  %s new", plural(added, "article", "articles"))
	if n := len(collected) - added; n > 0 {
		fmt.Printf(", %s refreshed", comma(n))
	}
	if dropped > 0 {
		fmt.Printf(", %s no longer in %s", plural(dropped, "article", "articles"), folder)
	}
	fmt.Println()

	reportCeiling(len(res.Bookmarks), added, folder)
	return nil
}

// reportCeiling explains the 500-item wall the first time a sync hits it, so a
// backlog that stops growing does not look like a bug.
func reportCeiling(returned, added int, folder string) {
	if returned < instapaper.MaxListLimit {
		return
	}
	fmt.Printf("\nInstapaper only exposes the newest %d items of %s, and offers no way\n"+
		"to page past that. As you archive what you have read, older articles move\n"+
		"into view and the next sync will pick them up.\n", instapaper.MaxListLimit, folder)
}
