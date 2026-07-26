# samling

Shuffle your way through an Instapaper backlog.

`samling` keeps a local mirror of your unread list, picks articles out of it at
random, opens them in your browser, and archives what you've read on the next
sync. It exists because a reading list is ordered and the top of it goes stale,
so a large backlog just sits there getting larger.

It's a port of the idea behind [pickpocket][] — a Pocket CLI that died with
Pocket in July 2025 — to Instapaper, with the destructive parts removed.

[pickpocket]: https://github.com/tiagoamaro/pickpocket-rust

```
$ samling status
1,847 unread
0 pending archive
312 archived

Last synced 2 hours ago
Oldest unread is 4 years old: The Cathedral and the Bazaar

Top domains:
    204  nytimes.com
    118  theatlantic.com
     97  longreads.com

$ samling pick -n 3
 1. A Brief History of the Interrobang
    https://www.theatlantic.com/…
    theatlantic.com · saved 2 years ago
 2. …
```

## The triage loop

The way this actually gets used:

```sh
samling pick -n 20    # opens 20 tabs
```

Work through the tabs. Close the ones you don't care about. For the ones you
*do* want to read, hit the Instapaper extension to re-save them — which also
floats them to the top of your Unread list.

```sh
samling sync          # archives the batch, keeps the ones you re-saved
```

samling detects the re-saves and leaves them alone. Re-saving reuses an
article's `bookmark_id`, so without that detection your keepers would be
archived by the very sync that follows, or stay tombstoned and never be served
again. A deliberate re-save always outranks samling's bookkeeping.

To burn down a chunk you're never going to read, skip the browser entirely:

```sh
samling pick -n 100 --older-than 1y --no-open
samling sync
```

`list` is the non-committing preview; `pick` always commits.

## How it works

Picking is **entirely offline**. `samling pick` reads a JSON file, chooses at
random, and shells out to your browser — no network, no latency, works on a
plane. Everything that touches Instapaper happens in one explicit `samling sync`.

Picked articles are **archived, never deleted**. They stay in your account and
stay searchable. A tombstone list makes sure an article is never served up
twice, even if a later sync still reports it as unread.

## Install

```sh
go install github.com/nilsjesper/samling@latest
```

Or build from a checkout: `go build -o samling .`

## Setup

1. **Get an API key.** Instapaper's Full API needs a consumer token, and every
   request is reviewed by a human. Apply at
   <https://www.instapaper.com/developers/applications/create>.

2. **Give it to samling**, either in `~/.samling/config.json`:

   ```json
   { "consumer_key": "…", "consumer_secret": "…" }
   ```

   or via `INSTAPAPER_CONSUMER_KEY` / `INSTAPAPER_CONSUMER_SECRET`, which take
   precedence.

3. **Log in and sync.**

   ```sh
   samling login    # prompts for your Instapaper username and password
   samling sync     # pulls your unread list down
   samling pick -n 3
   ```

Instapaper's API only supports xAuth, so `login` trades your username and
password for a long-lived token in a single call. The password is used once and
never written to disk; only the resulting token is stored.

## Commands

| | |
|---|---|
| `samling login` | Exchange username + password for an access token |
| `samling sync` | Archive what you've read, then refresh the local mirror |
| `samling pick` | Open N random unread articles, mark them read locally |
| `samling pick --no-open` | Same, without tabs — bulk-skip a chunk |
| `samling list` | Same selection as `pick`, printed instead of opened |
| `samling status` | Unread / pending / archived counts, top domains, oldest article |
| `samling undo` | Put the most recent pick back (before the next sync) |
| `samling folders` | List your Instapaper folder ids |

### Filters

`pick` and `list` take the same filters:

```sh
samling pick -n 5 --older-than 1y          # dig into the deep backlog
samling pick --domain nytimes.com          # matches subdomains too
samling pick --newer-than 7d               # only things saved this week
samling pick --starred                     # or --unstarred
samling list -n 10 --seed 42               # reproducible shuffle
samling pick --folder 1234567              # a folder id from `samling folders`
```

Spans accept `d`, `w` and `y` on top of Go's duration syntax: `90d`, `2w`,
`1y`, `36h`, `30m`.

### Undo

`pick` commits its state change before opening anything, so an interrupted run
can't serve the same article twice. If you didn't mean it:

```sh
samling undo
```

That only works before the next `sync`. Once an article has actually been
archived in Instapaper, unarchive it there.

## Reaching a deep backlog

The API will not show you more than the newest 500 unread articles, and there is
no parameter that changes that. If your backlog is larger, samling can't see all
of it at once — nothing can, through this API.

What it does instead is accumulate. `library.json` is never wiped; each sync
adds whatever is newly visible. Since archiving removes an article from Unread,
every article you read slides one more into the window:

```sh
samling pick -n 5     # read five
samling sync          # archives them; five older ones become visible
```

So the mirror grows as you work through it, and `status` will show the total
climbing past 500 over time.

## Files

Everything lives in `~/.samling`, or `$SAMLING_HOME` if set.

| file | |
|---|---|
| `config.json` | Your consumer key and secret |
| `token.json` | The OAuth access token (mode `0600`) |
| `library.json` | The local mirror: unread, pending-archive, and tombstones |

`library.json` is plain JSON and safe to read, grep, or back up. Writes are
atomic, so an interrupted run can't corrupt it.

## Notes on the Instapaper API

Things worth knowing if you plan to hack on this:

- **xAuth only.** There is no request-token/authorize browser flow. OAuth 1.0a
  with HMAC-SHA1, parameters in the `Authorization` header, everything POSTed.
- **500 per folder, and no way past it.** `bookmarks/list` has no offset and no
  cursor. The `have` parameter looks like pagination but isn't: the server takes
  the first `limit` items of the folder and *then* subtracts `have`, so asking
  again with everything you hold returns nothing rather than the next page.
  Verified against the live API. A folder with 5,000 articles will only ever
  expose its newest 500.

  This is why samling's mirror is **cumulative** rather than a snapshot.
  Archiving an article removes it from Unread, which slides the window, so each
  sync uncovers a little more of the backlog. See "Reaching a deep backlog".
- **`bookmarks/list` is called at v1.1.** The v1 endpoint returns an array of
  typed objects and buries `delete_ids` as a comma-separated string on a `meta`
  element; v1.1 returns the documented object with `delete_ids` as an array.
- **`delete_ids` reports what fell outside the *window*, not what was deleted.**
  An article still in your account but sitting below the 500-item cut comes back
  as a `delete_id`, indistinguishable from a genuine deletion. samling therefore
  acts on it only when everything it knows about fits inside the window with room
  to spare. An earlier version trusted it at exactly 500 and a single re-saved
  article — rejoining the top of Unread and displacing the last one — evicted a
  live article from the mirror. Worst case now, a real deletion is noticed later.
- **Re-saving reuses the `bookmark_id`** and refreshes the article's `time`.
  That timestamp is the only way to tell "I re-saved this during triage" from
  "this is still queued for archiving", which is why `sync` fetches before it
  archives.
- **`have` gets big.** Roughly 9 bytes per article, so a 20,000-item backlog
  means a ~180 KB POST body. `sync` warns past 100 KB. If Instapaper ever starts
  rejecting those, that's the reason.
- **No bulk actions.** Unlike Pocket, archiving is one request per article, so
  `sync` uses a small worker pool (`--concurrency`, default 4) and saves
  progress as it goes.
- **No word count.** The bookmark object is only `bookmark_id, url, title,
  description, hash, time, progress, progress_timestamp, starred,
  private_source`. Reading-time filters would need a `bookmarks/get_text` fetch
  per article; not implemented.

## Development

```sh
go test ./...
go vet ./...
```

The tests cover the two things most likely to break silently: the OAuth 1.0a
signature (checked against the worked example in RFC 5849 §3.4.1.1) and the
drain loop's paging and `delete_ids` handling (checked against a fake server,
including a deliberately hostile one that reports spurious deletions).

## Name

Norwegian, Danish and Swedish for "collection".

## License

MIT
