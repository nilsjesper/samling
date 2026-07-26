package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/molson/samling/internal/instapaper"
	"github.com/molson/samling/internal/library"
)

// maxDrainPages bounds the drain loop. At 500 bookmarks a page this covers
// 100,000 articles; it exists only so a server-side surprise can't spin forever.
const maxDrainPages = 200

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
// bookmarks/list has no offset parameter: the only way through a folder larger
// than the 500-item page limit is to keep re-asking while telling the server
// what you already hold via "have". Each pass therefore adds the ids it just
// received to "have" and asks again, until a page comes back short.
func drain(ctx context.Context, client *instapaper.Client, lib *library.Library, folder string, dryRun bool) error {
	baseHave := lib.HaveParam()
	have := baseHave
	if len(have) > haveWarnThreshold {
		fmt.Fprintf(os.Stderr,
			"note: the delta-sync parameter is %s characters; if Instapaper starts rejecting\n"+
				"      requests, this is why — see the README.\n", comma(len(have)))
	}

	collected := map[string]library.Bookmark{}
	var (
		deleteIDs    []string
		trustDeletes bool
		pages        int
	)

	fmt.Printf("Fetching %s...\n", folder)

	for pages = 1; pages <= maxDrainPages; pages++ {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted while fetching")
		}

		res, err := client.List(ctx, folder, instapaper.MaxListLimit, have)
		if err != nil {
			return err
		}

		added := 0
		for _, b := range res.Bookmarks {
			id := b.BookmarkID.String()
			if id == "" {
				continue
			}
			if _, seen := collected[id]; !seen {
				added++
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

		if len(res.Bookmarks) < instapaper.MaxListLimit {
			// The server had room to spare, so it covered the whole folder.
			//
			// This is the only point at which delete_ids can be trusted. The
			// API reports ids from "have" that "would not have appeared within
			// the given limit" — on a short page that means genuinely gone, but
			// on a full page it just means "further down the folder", and
			// honouring it mid-loop would evict live bookmarks.
			trustDeletes = true
			for _, id := range res.DeleteIDs {
				if s := id.String(); s != "" {
					deleteIDs = append(deleteIDs, s)
				}
			}
			break
		}
		if added == 0 {
			// A full page with nothing new: the server is not honouring "have"
			// the way we expect. Stop rather than loop forever, and don't act
			// on delete_ids we can't interpret.
			fmt.Fprintln(os.Stderr,
				"note: stopped early — a full page returned no new articles.")
			break
		}

		// Rebuild from the base each pass; appending to the previous value
		// would repeat everything collected in earlier passes.
		have = appendHave(baseHave, collected)
		fmt.Printf("  %s so far...\n", comma(len(collected)))
	}

	if pages > maxDrainPages {
		fmt.Fprintf(os.Stderr, "note: stopped after %d pages; run sync again to continue.\n", maxDrainPages)
	}

	if dryRun {
		fmt.Printf("Would add or update %s in the local mirror.\n", plural(len(collected), "article", "articles"))
		if trustDeletes && len(deleteIDs) > 0 {
			fmt.Printf("Would drop %s that are no longer in %s.\n", plural(len(deleteIDs), "article", "articles"), folder)
		}
		fmt.Printf("Local mirror currently holds %s unread.\n", comma(len(lib.Bookmarks)))
		return nil
	}

	for _, b := range collected {
		lib.Upsert(b)
	}
	dropped := 0
	if trustDeletes {
		for _, id := range deleteIDs {
			if _, ok := lib.Bookmarks[id]; ok {
				delete(lib.Bookmarks, id)
				dropped++
			}
		}
	}

	fmt.Printf("  %s new or updated", plural(len(collected), "article", "articles"))
	if dropped > 0 {
		fmt.Printf(", %s no longer in %s", plural(dropped, "article", "articles"), folder)
	}
	fmt.Println()
	return nil
}

// appendHave extends the delta-sync parameter with everything collected so far.
func appendHave(have string, collected map[string]library.Bookmark) string {
	parts := make([]string, 0, len(collected)+1)
	if have != "" {
		parts = append(parts, have)
	}
	for id, b := range collected {
		if b.Hash != "" {
			parts = append(parts, id+":"+b.Hash)
		} else {
			parts = append(parts, id)
		}
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	total := 0
	for _, p := range parts {
		total += len(p) + 1
	}
	buf := make([]byte, 0, total)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, p...)
	}
	return string(buf)
}
