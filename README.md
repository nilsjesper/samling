# samling

Open random articles from your Instapaper Unread list, then archive them on the
next sync. A port of [pickpocket](https://github.com/tiagoamaro/pickpocket-rust)
(a Pocket CLI that died with Pocket in July 2025) that archives instead of
deleting.

## Install

```sh
go install github.com/nilsjesper/samling@latest   # or, from a checkout: go build -o samling .
```

## Set up

1. Create an Instapaper application at
   <https://www.instapaper.com/developers/applications/create>. It works for
   your own account straight away.
2. On the application's page, select "Generate access token" and copy it.
3. Save the token and do the first sync, which reads your whole Unread list:

   ```sh
   pbpaste | samling login --token   # or run without the pipe and paste at the hidden prompt
   samling sync
   ```

One token works on every machine. Generating a new one revokes the old one.
To sign in through the browser instead, see [Browser sign-in](#browser-sign-in).

## Triage

```sh
samling pick 20   # opens 20 random unread articles in tabs
```

Close the tabs you don't want. Re-save the ones you do with the Instapaper
browser extension. Then:

```sh
samling sync      # archives the batch, keeps the re-saves
```

Each re-saved article stays in Unread and gets a counter tag in Instapaper:
`samling1` the first time you keep it, then `samling2`, and so on. Click
`samling4` in Instapaper to see everything you've kept four times.
`samling survivors` lists the same thing locally.

To clear old articles without reading them:

```sh
samling list 100 --older-than 1y             # preview, changes nothing
samling pick 100 --older-than 1y --no-open   # mark read without opening
samling sync
```

`samling undo` puts the last pick back, as long as you haven't synced since.
`pick` marks articles read before it opens anything, so use `undo` after an
interrupted pick too. After a sync, unarchive in Instapaper.

## Commands

| Command | |
|---|---|
| `samling login` | Sign in and save an access token |
| `samling sync` | Archive picked articles, then refresh the local mirror |
| `samling pick N` | Open N random unread articles and mark them read locally |
| `samling list N` | Same selection as `pick`, printed, nothing marked |
| `samling status` | Counts, top domains, oldest article, most re-saved |
| `samling survivors [N]` | Articles you've kept, most-kept first |
| `samling undo` | Put the last pick back (before the next sync) |
| `samling folders` | List your folder ids |
| `samling help` | Usage and flags |

`pick` and `list` filters:

```sh
--older-than 1y        # spans: 30m, 36h, 90d, 2w, 1y
--newer-than 7d
--domain nytimes.com   # includes subdomains
--starred              # or --unstarred
--folder 1234567       # id from `samling folders`; needs `samling sync --folder 1234567` first
--kept 2               # only articles kept at least twice; bare --kept means once
--seed 42              # reproducible shuffle
--no-open              # pick only: mark read without opening or checking links
--no-check             # skip the dead-link check
```

A number right after `--kept` is its value: `samling pick --kept 2` picks one
article. For three articles kept at least once, put the count first:
`samling pick 3 --kept`.

Other flags:

```sh
samling sync --full          # re-read the folder and drop anything missing; fixes a mirror that looks wrong
samling sync --dry-run       # show what sync would do
samling sync --concurrency 8 # parallel archive requests (default 4)
samling survivors --min 3    # only articles kept 3+ times
samling status --domains 10  # top domains to show (default 5, 0 for none)
```

## Dead links

Before opening tabs, `pick` checks its links (5-second timeout) and replaces
dead ones with new random picks that match the same filters. If the pool runs
out, you get fewer than you asked for. A link is dead
only on 404, 410, a domain that no longer resolves, or a refused connection.
Everything else (401, 403, 429, 5xx, timeouts, TLS errors) counts as alive,
because bot-blocking sites like the NYT answer that way for live pages. A link
that redirects to a front page (`/`), even on another site, is flagged
"possibly dead" and opened anyway.

Skipped picks are marked read with the rest of the batch, so `undo` restores
them. The next sync tags them `samling-dead` and archives them. Instapaper may
still have the article text.

`list` reports dead links without replacing them. Every check is logged to
`~/.samling/linkcheck.log`.

## Sync

`pick` never talks to Instapaper. It reads the local mirror and records what
sync should do.

The first sync reads all of Unread, 500 articles per request. Later syncs fetch
only what changed, usually in one request. Articles you archived, deleted or
moved elsewhere drop out of the mirror.

Picked articles are archived, never deleted. A local tombstone list stops an
article from being picked twice.

Re-saving an old article often saves a redirected address, which Instapaper
treats as a new bookmark. Sync matches a bookmark saved after the pick back to
it when both are on the same site and share an address (ignoring scheme, `www`,
query and trailing slash), or one title contains the other (ignoring case,
punctuation and a trailing ` | Site` or ` - Site`). It archives the old copy,
removes its `samlingN` tag, tags the new copy with the next count up, and
reports it as "re-saved at a new address".

Tagging doesn't change an article's place in Unread. A failed tag write is
reported and retried on the next sync.

`samling sync --folder 1234567` adds one of your folders to the mirror, and it
stays synced from then on. Starred is a filter, not a folder. The Archive isn't
mirrored.

## Browser sign-in

Put the application's consumer key and secret in `~/.samling/config.json`:

```json
{ "consumer_key": "...", "consumer_secret": "..." }
```

`INSTAPAPER_CONSUMER_KEY` and `INSTAPAPER_CONSUMER_SECRET` override the file.

Add `http://127.0.0.1:8919/callback` to the application's Callback URIs, then
run `samling login`. Instapaper only accepts a local callback while the
application is in "Owner Only" mode, which is the default. If port 8919 is
taken, use `--port N` and register that port instead. `--no-open` prints the
sign-in URL instead of opening it.

`samling login --password` uses the old username/password exchange (xAuth),
which Instapaper turns off for new sign-ins on 2027-09-30. Tokens it already
issued keep working.

samling never stores your password. `login` tests the token before saving it.

## Files

Everything lives in `~/.samling`, or `$SAMLING_HOME` if set.

| File | |
|---|---|
| `config.json` | Consumer key and secret (browser sign-in and `--password` only) |
| `token.json` | Access token (mode `0600`) |
| `library.json` | The mirror: unread, pending archive, tombstones, queued tags, sync cursor |
| `linkcheck.log` | One line per link checked: time, verdict, reason, address |

`library.json` is plain JSON and written atomically.

## Instapaper API notes

For anyone changing the sync code:

- Sync, archiving, tags and `folders` use API v2 (`/api/2/`) with
  `Authorization: Bearer`. `login` uses OAuth 2. `login --password` uses v1
  xAuth, and its token works unchanged as a v2 bearer token (verified live).
- v1 `bookmarks/list` returned only the newest 500 articles, with no offset.
  Its `have` parameter filtered after that limit. v2 has `offset` and reports
  `total`.
- `GET /api/2/bookmarks?since=T` is account-wide: every changed bookmark in any
  section (archived ones carry `archived: true`), tag-only changes (with `time`
  unchanged), and `deleted_ids`. Bookmarks and deleted ids share each page's
  `limit`, so advance the offset by both. samling stores the sync start time
  minus ten minutes as the next `since`, since reading a change twice is
  harmless and missing one isn't.
- Re-saving an identical URL keeps the id and updates `time`. That's the only
  way to tell a keeper from a pick waiting to be archived, so sync fetches
  before it archives. A redirected URL gets a new id (two of three re-saves in
  one live triage).
- v2 `id` equals v1 `bookmark_id`, so libraries from older versions carry over.
- Offset paging isn't a snapshot. An article leaving Unread during a full read
  shifts the pages, and one article is missed until the next `sync --full`.
- `POST /api/2/bookmarks/{id}/tags` adds tags by name and removes them by id
  only, so sync looks ids up once with `GET /api/2/tags`. Tagging leaves `time`
  alone, and tags survive a re-save (both verified live). There's no endpoint
  for deleting a tag, so unused `samlingN` tags stay until you remove them in
  the web app.
- Archiving is one `POST /api/2/bookmarks/{id}/move` per article, run in
  parallel (`--concurrency`), with progress saved as it goes.
- Errors are real HTTP statuses with `{"error": {"code", "message"}}`. samling
  retries 5xx, 429 (honouring `Retry-After`, capped at a minute) and non-JSON
  bodies, which is how a maintenance page shows up.
- Bookmarks have no word count. Reading-time filters would need
  `GET /api/2/bookmarks/{id}/parse` per article.

## Development

```sh
go test ./...
go vet ./...
```

Tests use fake servers and a fake DNS resolver. Nothing calls Instapaper or real
article addresses.

## Name

Norwegian, Danish and Swedish for "collection". MIT licensed.
