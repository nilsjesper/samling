# Developing samling

Notes on the Instapaper API for anyone hacking on this.

## Two API versions

- **Sync, archiving, `folders`** use API v2 (`/api/2/`): JSON, `GET` for reads,
  and the access token as a plain `Authorization: Bearer` header.
- **`login`** signs in with OAuth 2 by default; `login --password` keeps v1's
  xAuth exchange until it is turned off (2027-09-30). An xAuth token works
  unchanged as a v2 bearer token (verified live); its secret goes unused.

## v1 couldn't see past 500

v1's `bookmarks/list` read only the newest 500 articles of a folder and had no
offset. Its `have` parameter looked like pagination but filtered *after* the
limit was applied. On a 23,858-article backlog, samling's mirror held 494 of
them. v2 has real `offset` paging and reports the section's `total`.

## The `since` feed is account-wide

`GET /api/2/bookmarks?since=T` returns every bookmark changed after `T` in any
section, archived ones included (`archived: true`), plus `deleted_ids`. A
tag-only change shows up too, with `time` unchanged. Bookmarks and deleted ids
share each page's `limit`, so the offset advances by both. samling stores the
time just *before* each sync started, minus ten minutes for clock skew, as the
next `since`. Reading a change twice is harmless; missing one is not.

## Re-saving reuses the bookmark id, but only for an identical URL

It refreshes the article's `time`. That timestamp is the only way to tell "I
re-saved this during triage" from "this is still queued for archiving", which
is why `sync` fetches before it archives. A re-save from the browser extension
uses the address the page redirects to, so an old article often comes back as a
new bookmark under a new id (seen live: two of three re-saves in one triage).
`sync` matches those by site plus address or title. (v2 `id` is the same number
as v1 `bookmark_id`, so libraries from older versions of samling carry straight
over.)

## Offset paging isn't a snapshot

If an article leaves Unread while a full read is paging through it, everything
shifts up one place and one article is skipped, and dropped from the mirror
until the next `sync --full`. It takes about 48 requests to read 24,000
articles, so the window for this is small.

## Tags are swapped by id

`POST /api/2/bookmarks/{id}/tags` takes `add_tags` by name (creating a tag that
doesn't exist yet) and `remove_tags` by id only, in one request, and answers
with the article's full tag list. The mirror stores tag names, so when an old
`samlingN` has to come off, sync reads `GET /api/2/tags` once to find its id.
Tagging leaves `time` alone, and tags survive a re-save from the browser
extension (both verified live). There's no endpoint for deleting a tag, so an
old `samlingN` nobody carries any more lingers in the tag list until you remove
it in the web app.

## No bulk actions

Unlike Pocket, archiving is one request per article
(`POST /api/2/bookmarks/{id}/move`), so `sync` uses a small worker pool
(`--concurrency`, default 4) and saves progress as it goes.

## Errors are real HTTP statuses

With `{"error": {"code", "message"}}`. samling retries 5xx, 429 (honouring
`Retry-After`, capped at a minute), and bodies that aren't JSON, which is how a
maintenance page shows up.

## No word count

In the bookmark object. Reading-time filters would need a
`GET /api/2/bookmarks/{id}/parse` per article; not implemented.