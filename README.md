# samling

Shuffle your way through an Instapaper backlog.

`samling` keeps a local mirror of your unread list, picks articles out of it at
random, opens them in your browser, and archives what you've read on the next
sync. It exists because a reading list is ordered and the top of it goes stale,
so a large backlog just sits there getting larger.

## Inspiration

This is a port of the idea behind [pickpocket][] — a Pocket CLI that died with
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

The counts work offline too. `samling survivors` lists every kept article,
most-kept first and oldest first within a count; `--min 3` keeps it to the
ones you've saved three times or more, and a bare number caps the list.
`pick` and `list` take `--kept K` to choose only among articles kept at least
K times (a bare `--kept` means once):

```sh
samling survivors 10          # the ten most stubborn articles
samling pick 3 --kept 2       # three of the ones you've kept twice or more
```

To burn down a chunk you're never going to read, skip the browser entirely:

```sh
samling pick 100 --older-than 1y --no-open
samling sync
```

`list` is the non-committing preview; `pick` always commits.

## Dead links

A backlog that reaches back a decade is full of articles whose pages are gone.
Before opening tabs, `pick` checks the links it chose, all at once, with a
5-second timeout. A pick is dead only on a clear signal: 404 or 410, a domain
that no longer resolves, or a refused connection. Anything murkier (401, 403,
429, 5xx, timeouts, TLS errors) counts as alive, since that's how bot-blocking
sites like the NYT answer for pages that are fine. A link that redirects to its
site's homepage is shown as "possibly dead" but still opened.

Dead picks are replaced with fresh random picks that match the same filters,
until there are enough live ones or the pool runs out:

```
skipped 2 dead links
  How the Kindle Changed Reading (HTTP 404)
    https://web.archive.org/web/http://example.com/2011/kindle
  …
They're marked read too; the next sync archives them and tags them samling-dead.
```

Nothing vanishes silently: a skipped pick is marked read like the rest of the
batch (so `undo` puts it back too), and the next `sync` tags it `samling-dead`
in Instapaper before archiving it, through the same retry queue as the
`samlingN` counters. Instapaper often kept its own copy of the text, so a dead
article may still be readable there.

`list` checks too, but only reports. `pick --no-check` skips the check, and
`pick --no-open` never checks, since nothing gets opened. Every result is
appended to `~/.samling/linkcheck.log`, to see how often each case comes up.

## How it works

Picking never talks to Instapaper. `samling pick` reads a JSON file, chooses at
random, checks the chosen links, and shells out to your browser. Everything
that touches Instapaper happens in one explicit `samling sync`; `pick` only
records what sync should do, such as the tags for dead links.

Picked articles are **archived, never deleted**. They stay in your account and
stay searchable. A tombstone list makes sure an article is never served up
twice, even if a later sync still reports it as unread.

## Install

```sh
# Homebrew (macOS or Linux)
brew install nilsjesper/tap/samling

# Or download a binary for macOS, Linux, Windows from Releases
# https://github.com/nilsjesper/samling/releases

# Or with Go
go install github.com/nilsjesper/samling@latest

# Or build from source
go build -o samling .
samling version
```

The release binaries aren't signed. The Homebrew install clears macOS's
quarantine flag for you; for a downloaded binary, run
`xattr -d com.apple.quarantine samling` once if macOS won't open it.

## Setup

**You create your own Instapaper application** (takes ~30 seconds). This keeps
your credentials yours — no shared keys, no rate-limit contention, no third
party in the loop. A new application starts in "Owner Only" mode and works
right away for your own account; Instapaper's human review is only needed
before *other people* can sign in with it.

1. **Create the application** at
   <https://www.instapaper.com/developers/applications/create>

2. **Sign in.** Pick one:

   - **Access token (simplest).** On your application's page under
     [Applications](https://www.instapaper.com/developers/applications), select
     "Generate access token", then run `samling login --token` and paste it at
     the (hidden) prompt, or pipe it in: `pbpaste | samling login --token`.
     It's never accepted as an argument, so it stays out of your shell
     history. Nothing else is needed: no consumer key, no `config.json`. One
     token works on every machine, but regenerating it revokes the old one
     everywhere.

   - **Browser sign-in.** Give samling the application's consumer key and
     secret, in `~/.samling/config.json`:

     ```json
     { "consumer_key": "…", "consumer_secret": "…" }
     ```

     or via `INSTAPAPER_CONSUMER_KEY` / `INSTAPAPER_CONSUMER_SECRET`, which take
     precedence. On the application's page, add
     `http://127.0.0.1:8919/callback` to the Callback URIs (Instapaper only
     accepts a local callback while the application is "Owner Only"). Then
     `samling login` opens the consent page and catches the redirect on that
     port. If 8919 is taken, use `samling login --port N` and register that
     port's URI instead.

3. **Sync and pick.**

   ```sh
   samling sync     # the first sync reads your whole unread list
   samling pick 3
   ```

Either way, samling never sees your password. `login` checks the token with a
cheap API call before saving it, and only the token is stored.

The old password login is still there as `samling login --password`. It uses
xAuth, which Instapaper turns off for new sign-ins on **2027-09-30**; tokens it
already issued keep working after that, so existing installs don't need to log
in again.

## Commands

| Command | Description |
|---|---|
| `samling login` | Sign in through the browser and save an access token |
| `samling login --token` | Save a token from "Generate access token" instead |
| `samling sync` | Archive what you've read, then refresh the local mirror |
| `samling pick N` | Open N random unread articles, mark them read locally |
| `samling pick N --no-open` | Same, without tabs — bulk-skip a chunk |
| `samling pick N --no-check` | Open them without checking for dead links first |
| `samling list N` | Same selection as `pick`, printed instead of opened |
| `samling status` | Unread / pending / archived counts, top domains, oldest article, most re-saved |
| `samling survivors [N]` | List the articles you keep re-saving, most-kept first |
| `samling undo` | Put the most recent pick back (before the next sync) |
| `samling folders` | List your Instapaper folder ids |
| `samling version` | Print the version |

### Filters

`pick` and `list` take the same filters:

```sh
samling pick 5 --older-than 1y             # dig into the deep backlog
samling pick --domain nytimes.com          # matches subdomains too
samling pick --newer-than 7d               # only things saved this week
samling pick --starred                     # or --unstarred
samling list 10 --seed 42                  # reproducible shuffle
samling pick --folder 1234567              # a folder id from `samling folders`
samling pick 3 --kept 2                    # only articles re-saved twice or more
```

Spans accept `d`, `w` and `y` on top of Go's duration syntax: `90d`, `2w`,
`1y`, `36h`, `30m`.

A number right after `--kept` is its value, so `samling pick --kept 2` picks
one article kept at least twice. To combine a bare `--kept` with a count, put
the count first: `samling pick 3 --kept`.

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
| `config.json` | Your consumer key and secret (only for browser sign-in and `--password`) |
| `token.json` | Your access token (mode `0600`) |
| `library.json` | The local mirror: unread, pending-archive, tombstones, queued tags, and the sync cursor |
| `linkcheck.log` | One line per link `pick` or `list` checked: time, verdict, reason, address |

`library.json` is plain JSON and safe to read, grep, or back up. Writes are
atomic, so an interrupted run can't corrupt it.

## Development

```sh
go test ./...
go vet ./...
```

The tests cover the things most likely to break silently: the OAuth 1.0a
signature (checked against the worked example in RFC 5849 §3.4.1.1), the OAuth 2
sign-in (state checks, code exchange and its errors, against a fake server), and sync
against a fake v2 server. That includes paging, the change feed and its
deletions, re-saved keepers and their `samlingN` counters, picks archived
elsewhere, migrating a v1 library, and the cursor advancing only after a
successful sync. The link checker runs against a local test server and a fake
resolver, never real article addresses.

For deep API notes — two API versions, v1's 500-item limit, the `since` feed
semantics, re-save bookmark-id reuse, offset paging caveats, tag swap
mechanics, no bulk actions, error handling, missing word counts — see
[DEVELOPING.md](DEVELOPING.md).

## Name

Norwegian, Danish and Swedish for "collection".

## License

MIT