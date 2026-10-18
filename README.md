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

$ samling pick 3
 1. A Brief History of the Interrobang
    https://www.theatlantic.com/…
    theatlantic.com · saved 2 years ago
 2. …
```

## The triage loop

The way this actually gets used:

```sh
samling pick 20       # opens 20 tabs
```

Work through the tabs. Close the ones you don't care about. For the ones you
*do* want to read, hit the Instapaper extension to re-save them — which also
floats them to the top of your Unread list.

```sh
samling sync          # archives the batch, keeps the ones you re-saved
```

samling detects the re-saves and leaves them alone. Re-saving the same URL
reuses an article's `bookmark_id`, so without that detection your keepers
would be archived by the very sync that follows, or stay tombstoned and never
be served again. A deliberate re-save always outranks samling's bookkeeping.

Old articles often redirect (to `https`, to or from `www`, or to a new path),
and the extension saves the address it lands on. Instapaper then makes a new
bookmark instead. samling matches a new bookmark saved after a pick back to
that pick when they're on the same site and have the same address (ignoring
scheme, `www`, query and trailing slash) or the same title (ignoring case,
punctuation and a ` | Site` suffix). It archives the old copy so the article
isn't in the pool twice, and reports the new one as kept "(re-saved at a new
address)".

Each keeper also gets a counter tag in Instapaper: `samling1` the first time
you keep it, then `samling2`, `samling3` and so on, swapped in place so an
article only ever has one. Click `samling4` in Instapaper to see everything
that survived four triages. A keeper that came back at a new address carries
its count over to the new copy, and the archived old copy loses its
`samlingN` (only that tag), so each survivor shows up once. Kept articles stay in the pick pool, and `pick`,
`list` and `status` show the count (`kept ×3`). Tagging doesn't change an
article's save time or its place in Unread. If a tag write fails, `sync` says
so, carries on, and retries it next time.

To burn down a chunk you're never going to read, skip the browser entirely:

```sh
samling pick 100 --older-than 1y --no-open
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

3. **Let samling sign in.** Pick one:

   - **Browser sign-in (the default).** On your application's page under
     [Applications](https://www.instapaper.com/developers/applications), add
     `http://127.0.0.1:8919/callback` to the Callback URIs. Instapaper only
     accepts a local callback on an application in "Owner Only" mode, which is
     what a personal key is anyway. Then `samling login` opens the consent page
     and catches the redirect on that port. If 8919 is taken, use
     `samling login --port N` and register that port's URI instead.
   - **Access token.** On the same page, select "Generate access token", then
     run `samling login --token` and paste it at the (hidden) prompt, or pipe it
     in: `pbpaste | samling login --token`. It's never accepted as an argument,
     so it stays out of your shell history. One token works on every machine,
     but regenerating it revokes the old one everywhere.

4. **Sync and pick.**

   ```sh
   samling login
   samling sync     # pulls your unread list down
   samling pick 3
   ```

Either way, samling never sees your password. `login` checks the token with a
cheap API call before saving it, and only the token is stored.

The old password login is still there as `samling login --password`. It uses
xAuth, which Instapaper turns off for new sign-ins on **2027-09-30**; tokens it
already issued keep working after that, so existing installs don't need to log
in again.

## Commands

| | |
|---|---|
| `samling login` | Sign in through the browser and save an access token |
| `samling login --token` | Save a token from "Generate access token" instead |
| `samling sync` | Archive what you've read, then refresh the local mirror |
| `samling pick N` | Open N random unread articles, mark them read locally |
| `samling pick N --no-open` | Same, without tabs — bulk-skip a chunk |
| `samling list N` | Same selection as `pick`, printed instead of opened |
| `samling status` | Unread / pending / archived counts, top domains, oldest article, most re-saved |
| `samling undo` | Put the most recent pick back (before the next sync) |
| `samling folders` | List your Instapaper folder ids |

### Filters

`pick` and `list` take the same filters:

```sh
samling pick 5 --older-than 1y             # dig into the deep backlog
samling pick --domain nytimes.com          # matches subdomains too
samling pick --newer-than 7d               # only things saved this week
samling pick --starred                     # or --unstarred
samling list 10 --seed 42                  # reproducible shuffle
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

## How sync works

The first sync reads your whole Unread list, however long it is, a page of 500
at a time:

```
$ samling sync
Reading all of unread...
  add 23,371 new articles, refresh 487, drop 7 articles no longer in unread
```

After that, a sync asks Instapaper only for what changed since the last one,
which usually takes a single request. New saves come in; articles you archived,
deleted or moved to another folder elsewhere drop out; a pick you already
archived from your phone is simply forgotten, with no request sent.

`samling sync --full` re-reads the whole folder. You shouldn't need it, but it
is the cure if the mirror ever seems out of step: a full read is a snapshot, so
anything it doesn't return is removed from the mirror.

`samling sync --folder 1234567` mirrors one of your own folders as well (ids
from `samling folders`), and `pick --folder 1234567` picks from it. Every
folder synced this way stays current from then on, whichever one you name.
Starred isn't a folder (use `pick --starred`), and the Archive isn't mirrored.

## Files

Everything lives in `~/.samling`, or `$SAMLING_HOME` if set.

| file | |
|---|---|
| `config.json` | Your consumer key and secret |
| `token.json` | Your access token (mode `0600`) |
| `library.json` | The local mirror: unread, pending-archive, tombstones, and the sync cursor |

`library.json` is plain JSON and safe to read, grep, or back up. Writes are
atomic, so an interrupted run can't corrupt it.

## Notes on the Instapaper API

Things worth knowing if you plan to hack on this:

- **Two API versions.** Sync, archiving and `folders` use API v2 (`/api/2/`):
  JSON, `GET` for reads, and the access token as a plain
  `Authorization: Bearer` header. `login` signs in with OAuth 2 by default;
  `login --password` keeps v1's xAuth exchange until it is turned off. An xAuth
  token works unchanged as a v2 bearer token (verified live); its secret goes
  unused.
- **v1 couldn't see past 500.** v1's `bookmarks/list` read only the newest 500
  articles of a folder and had no offset. Its `have` parameter looked like
  pagination but filtered *after* the limit was applied. On a 23,858-article
  backlog, samling's mirror held 494 of them. v2 has real `offset` paging and
  reports the section's `total`.
- **The `since` feed is account-wide.** `GET /api/2/bookmarks?since=T` returns
  every bookmark changed after `T` in any section, archived ones included
  (`archived: true`), plus `deleted_ids`. A tag-only change shows up too, with
  `time` unchanged. Bookmarks and deleted ids share each page's `limit`, so the
  offset advances by both. samling stores the time just *before* each sync
  started, minus ten minutes for clock skew, as the next `since`. Reading a
  change twice is harmless; missing one is not.
- **Re-saving reuses the bookmark id, but only for an identical URL**, and
  refreshes the article's `time`. That timestamp is the only way to tell "I
  re-saved this during triage" from "this is still queued for archiving",
  which is why `sync` fetches before it archives. A re-save from the browser
  extension uses the address the page redirects to, so an old article often
  comes back as a new bookmark under a new id (seen live: two of three
  re-saves in one triage). `sync` matches those by site plus address or title. (v2 `id` is the same number as v1 `bookmark_id`, so libraries from
  older versions of samling carry straight over.)
- **Offset paging isn't a snapshot.** If an article leaves Unread while a full
  read is paging through it, everything shifts up one place and one article is
  skipped, and dropped from the mirror until the next `sync --full`. It takes
  about 48 requests to read 24,000 articles, so the window for this is small.
- **Tags are swapped by id.** `POST /api/2/bookmarks/{id}/tags` takes
  `add_tags` by name (creating a tag that doesn't exist yet) and `remove_tags`
  by id only, in one request, and answers with the article's full tag list.
  The mirror stores tag names, so when an old `samlingN` has to come off, sync
  reads `GET /api/2/tags` once to find its id. Tagging leaves `time` alone, and
  tags survive a re-save from the browser extension (both verified live).
  There's no endpoint for deleting a tag, so an old `samlingN` nobody carries
  any more lingers in the tag list until you remove it in the web app.
- **No bulk actions.** Unlike Pocket, archiving is one request per article
  (`POST /api/2/bookmarks/{id}/move`), so `sync` uses a small worker pool
  (`--concurrency`, default 4) and saves progress as it goes.
- **Errors are real HTTP statuses** with `{"error": {"code", "message"}}`.
  samling retries 5xx, 429 (honouring `Retry-After`, capped at a minute), and
  bodies that aren't JSON, which is how a maintenance page shows up.
- **No word count** in the bookmark object. Reading-time filters would need a
  `GET /api/2/bookmarks/{id}/parse` per article; not implemented.

## Development

```sh
go test ./...
go vet ./...
```

The tests cover the things most likely to break silently: the OAuth 1.0a
signature (checked against the worked example in RFC 5849 §3.4.1.1), and sync
against a fake v2 server. That includes paging, the change feed and its
deletions, re-saved keepers and their `samlingN` counters, picks archived
elsewhere, migrating a v1 library, and the cursor advancing only after a
successful sync.

## Name

Norwegian, Danish and Swedish for "collection".

## License

MIT
