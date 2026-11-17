# Download Pipeline

## Summary

Turns an online track into a local library file. A remote track (a `tracks` row with no
file yet, see [database](../database/README.md#full-schema)) is queued in `download_jobs`,
resolved to a stream URL, downloaded to a partial file, validated, tagged, imported into
the library and finally attached to the playlist that asked for it.

Only the media request worker (`mediarequest.Worker` → `download.Service.Execute`) starts
a job: saves upsert a request, and the executor either promotes a cached copy in place or
enqueues the download described here.

The pipeline owns no UI and no network policy: adapters are injected as domain ports, and
the only external interaction is "give me a URL for this track" plus "move these bytes".

## Boundaries

| Layer | Owns |
|---|---|
| `internal/app/download` | queue worker, retry policy, finalize, merge, media-request executor (promote or enqueue) |
| `internal/domain/download.go` | job/state model, ports, retry classification |
| `internal/infra/downloader` | HTTP transfer, ranges, signature validation |
| `internal/infra/metadata` | tag writing (taglib) |
| `internal/infra/sqlite` | job and merge persistence |
| `internal/infra/wails` | binding + `download:*` events (translation only) |

`app` never imports `infra`: the concrete adapters are bound onto the ports in `main.go`
(`download.BindPorts` for the transfer/tag/artwork side, and a plugin-backed
`domain.StreamResolver` from `app/stream` for URL resolution).

**No built-in resolver.** URL resolution is plugin-only: a provider is playable only
when an enabled plugin declares it under `providers` and implements `stream.resolve`.
The catalog refuses a save for an undeclared provider before `EnsureRemoteTrack`, and
`Enqueue` independently refuses the same provider, so no job is created by the search
action, the playlist auto-download hook, a direct call or a retry. The error is the
readable `ErrStreamUnavailable`, naming the provider and the manifest field to fix.

## Job lifecycle

```
tracks.availability:  remote ──enqueue──► downloading ──► ready
                                              └─ fail (permanent) ─► failed
download_jobs.state:  queued ──claim──► active ──► done
                              ▲            │
                              └── retry ───┘ (same row: attempts+1, state back to queued)
```

- **One active job per track.** A partial unique index on `download_jobs(track_id)` covers
  `queued`/`active` only; `Enqueue` translates the conflict into
  `domain.ErrDownloadAlreadyQueued`, and callers treat "download this" as an idempotent
  user intent (`DownloadRemote` swallows it).
- **Automatic retries reuse the row.** After a retryable failure,
  `MarkFailed(..., nextRetryAt)` sets `state=queued`, `attempts+1` and a due time; nothing
  inserts a second job. `nextRetryAt == nil` is a permanent failure and sets `finished_at`.
- **Manual retry enqueues a new row.** `Retry(jobID)` is valid only after `failed` or
  `canceled`, and goes through `Enqueue`: the old row stays as history with its original
  `attempts` and error, and the new job starts at `attempts=0`. Callers that list the queue
  must therefore expect more than one row for a track over time and pick the live one —
  the UI hides a stale `failed`/`canceled` row while a `queued`/`active` row exists for the
  same track.
- **One worker, one job at a time.** Simpler than a pool and keeps the SQLite writer and
  the network budget to one consumer. An idle worker wakes on `Enqueue` or every
  `defaultPollInterval` (30s).
- **Crash recovery.** `Start` calls `ResetActiveToQueued`, so a job interrupted by a
  shutdown/restart is retried on the next launch.
- **Completion precedes merge.** `execute()` calls `MarkDone` right after finalize, then
  merges. Merge only rewrites library rows; a failure there is logged and cannot flip a
  finished job to `failed`. `MarkDone` is idempotent: a row already gone (for example after
  track deletion) is treated as closed instead of returning `download job not active`.

## Retry policy

`domain.ClassifyDownloadError(err, attempts, maxAttempts)` is the single source of truth:

| Failure | Retryable | Delay |
|---|---|---|
| `ErrStreamRateLimited` (429) | yes | 60s, doubling per attempt |
| `ErrStreamUnavailable` (VIP/copyright/`-110`) | no | — |
| `ErrStreamAuth` (403) | no | — |
| `ErrCorruptDownload` / `ErrDownloadPermanent` | no | — |
| anything else (transport, truncation) | yes | 5s → 30s → 5min, capped by `max_attempts` |

The URL is **re-resolved before every attempt**: stream URLs expire, and replaying a
cached one is exactly why a naive retry "does nothing".

### Quality fallback

`download_quality` is a preferred ceiling, not a hard requirement.
`domain.ResolveWithQualityFallback` asks the resolver for the configured tier and then each
lower tier (`flac → 320k → 128k`, `320k → 128k`, `128k`), stopping at the first URL. It
degrades only on `ErrStreamQualityUnavailable` — the plugin's structured `unavailable`/`vip`
kind or an empty URL. Network, auth and rate-limit failures return immediately, because a
lower bitrate would not fix them. The tier that resolved is written to `download_jobs.quality`
(`UpdateQuality`), drives the target/finalize step, and is what a later promotion compares
against; when every tier fails, the error names the tried tiers.

## Finalize

1. Target path is `<download_dir>/<name>.<ext>` — flat, no artist/album subfolders. `<name>`
   is `title - artist` by default, or `artist - title` when `download_naming_order` is
   `artist-title`. An existing path with **identical bytes** is reused; a different file
   already using the name gets `(2)`, `(3)`, … and is never overwritten. A reused file is
   re-tagged in place — identical bytes say nothing about its tags — and the staged duplicate
   is removed.
2. Validate the partial file: byte count, signature, decoded duration within ±3s of the
   expected duration.
3. Write tags with taglib, including the cover when the enqueue carried one. The cover URL
   is the source's large rendition (500×500 or original), not the 100×100 search
   thumbnail; a source that does not distinguish sizes falls back to its thumbnail. It is
   stored **before** the job is enqueued: a repeated save may already hold a job, and an
   `ErrDownloadAlreadyQueued` answer must not drop the cover.
   Cover URLs live in memory only: a restart loses the cover, never the download. Tags are
   read back afterwards; an empty title or artist is logged as a warning. A caller that
   already holds the bytes (promoting a streamed file) passes them as `CoverData`
   instead of a URL, so the finished file keeps the image the app has been showing even
   when the image host is unreachable.
4. Atomically rename the validated part file into place.
5. `LibraryService.ImportFileWithID(path)` → the track id this file became.
6. Attach to `playlist_id` when the job carries one (it may be NULL: deleting the playlist
   does not abort an in-flight download, the FK is `ON DELETE SET NULL`).

## Stream promotion

A saved remote track can be turned into a library file without downloading it again:
the stream cache's audio is finalised into the download directory. Playback owns that
file while a session is open, so a promotion that cannot run is deferred, never forced.

- **Request.** Every save goes through `mediarequest.Saver` (favorites, playlist adds,
  catalog/search/Last.fm saves), which upserts one `media_requests` row by
  `(provider, external_id)` and wakes the request worker. No caller creates a download
  job or a promotion row.
- **Execution.** `download.Service.Execute` decides: an active job owns the work; a
  complete cache at or above the wanted tier is promoted in place by
  `PromoteCachedFile` (`finalize → import → merge`), then the cache entry is discarded;
  anything else is enqueued as a normal download.
- **Blocked by playback.** When a cache session still owns the identity, or the player
  has the row loaded (`PlayingTrack`), `Execute` returns
  `domain.ErrMediaRequestDeferred`. The request stays `wanted`;
  `stream.Cache.OnSessionClosed` wakes the worker and the next sweep retries. A
  deferral records no failure and consumes no retry.
- **Quality.** Cache tier ≥ download setting → promote. Lower (or the cache tier
  unknown, treated as lowest) → download at the configured quality; a finished download
  discards the cache.
- **Deferred delete.** `DiscardEntry` returns `domain.ErrStreamDiscardDeferred` while a
  session owns the entry. The cache remembers that entry in memory and deletes the row
  and file itself when the session closes, so callers never write a cleanup flag. A
  promoted entry whose file is already gone is a success.
- **Replay.** Opening a cached entry reuses the audio; a discarded entry no longer
  exists, so the next playback simply downloads it again.
- **Startup / upgrade.** There is no promotion table and no cache flag. Migration 000104
  mirrors queued/running `promotion_tasks` and active `download_jobs` into
  `media_requests` before dropping the table; 000105 mirrors `pending_promote` entries
  before dropping the three flags. At startup `stream.Cache` deletes entries whose
  identity already maps to a local row, keeping any entry a request still needs.

## Merge rules

Applied right after `MarkDone`, before the job row would be needed again. `M1`/`M2` fold a duplicate row
into the survivor inside one transaction (playlist membership, favorite flag and play
count move across, then the remote row is deleted).

| Rule | Trigger | Behaviour |
|---|---|---|
| M1 | another row already holds this `(provider, external_id)` | fold into that row |
| M2 | the target path already belongs to a different row | fold into that row |
| M3 | a local track with the same title/artist/duration (±3s) exists | **log only, never auto-merge** — a wrong merge of two different songs is worse than a duplicate |
| none | no rule matches | the **imported** row survives (it owns the file and the resolved artists/album); the remote row is folded into it and its identity moves onto the survivor |

The "none" case is the common one, and getting it wrong is what made downloaded
tracks unplayable: keeping the remote row and only flipping `availability` to
`ready` left it pointing at `remote://<provider>/<id>` while the imported row held
the file. A streamable build can play that row, but it streams it again instead
of using the downloaded file, and `IsRemote()` still reports true, bypassing
local-file behaviour (tag writes, file watching, delete). Two invariants follow:

- **A row with `availability = 'ready'` has a real path.** It never keeps the
  virtual `remote://` form; the identity is not the path.
- **The identity follows the file.** `CarryIdentity` moves `(provider, external_id)`
  onto the survivor, because dropping it would make the next download of the same
  song register a fresh remote row — permanently unplayable again.
- **Lyrics follow the row too.** The merge moves any `lyrics` row from the remote
  id to the survivor inside the same transaction (`MoveLyrics`). New lyrics are
  requested only *after* the merge, using `MergeResult.SurvivorTrackID`: fetching
  before it attached the lyric to the row that was about to be deleted, and
  `lyrics.track_id ON DELETE CASCADE` removed it.
- **Artwork follows the row.** The imported file's embedded art wins; when it has
  none, `CarryArtworkKey` copies the streamed row's `artwork_key` onto the
  survivor (and its album) if those columns are still empty. Promotion then falls
  back to the bytes the stream cover cache served for the identity
  (`coverAssets.BytesForIdentity` → `artworkCache.Save` → same carry), so a
  promoted coverless file keeps the image the UI was showing instead of turning
  into a placeholder. When none of the three sources exist the key stays empty.
- **Deleting the file re-opens the request.** `media_requests` is keyed by
  identity, not by the row it produced: after a local file is deleted,
  `LibraryService.DeleteTrackFile` re-opens the identity's `done` row as `wanted`
  (same intent/quality, history kept) and wakes the worker, so the next save or
  playlist add promotes/downloads again. `MediaRequestService.Upsert` makes the
  same decision itself by asking `HasLocalTrack` when it sees `done` with an
  unchanged intent, and the worker re-runs that check for finished rows at
  startup (files deleted outside the app). A healthy local row keeps the old
  idempotent no-op behaviour.

**Decoded duration.** After the merge, `applyDecodedDuration` stores the duration and
bitrate on the row that owns the file. The duration is read by `domain.MediaDurationProbe`
(the ffmpeg container header, no decode); the taglib value is used only when the probe
fails. It also refuses implausible values: a decode of 0 is ignored, and a decode under two
seconds is accepted only when the row's stored duration is also short. A malformed
container — an old build wrote ID3 frames into a FLAC's audio stream — reports about one
second, and a raw FLAC has reported eight times the real length; either value would leave
the library showing a duration that has nothing to do with the file.

**Re-download.** `Service.Redownload(trackID)` is the repair entry for a file an
older build damaged while tagging: it deletes the row, file, playlist memberships
and index entry, re-registers the same identity with an unknown duration, and
enqueues a fresh job. It never runs implicitly; the track context menu exposes it
only for an online track that owns a local file.

## Availability invariants

- `provider = ''` ⟹ `availability = 'ready'` (local file, maintained by the scanner).
- `availability <> 'ready'` ⟹ `path` is the virtual `remote://<provider>/<external_id>`
  built by `domain.RemotePath` — never hand-assembled.
- `availability = 'ready'` ⟹ `path` points at a real file we validated at write time. This
  is one-directional: deletion of the file is not an invariant violation, the row stays
  and the UI can offer a re-download.
- The scanner and `DeleteByPathPrefix` are path-prefix scoped, so a virtual path can never
  be mistaken for a missing local file. Path-prefix queries reject an empty prefix.

## Saving a search result

The search page's two buttons (`收藏`, `加入歌单`) are the same action with a
different side effect: register the `(provider, external_id)` identity
(`EnsureRemoteTrack`), queue the download, and optionally set the favorite flag or
add playlist membership. `app/catalog` orchestrates it through three narrow ports
(`RemoteTracks`, `FavoritesWriter`, `PlaylistMembership`) so it imports neither this
package nor the playlist or library services.

- **Both buttons download.** The "download when adding to a playlist" setting only
  gates the automatic hook on a playlist add; acting on a search row is itself the
  download intent. `ErrDownloadAlreadyQueued` is success here, because the hook may
  have won the race.
- **A finished row is not queued again.** `EnsureRemoteTrack` returns the existing
  row; when `IsRemote()` is false the action reports `in_library` and stops, instead
  of failing with "already available locally". The remote row's album name from the
  search result is stored in `tracks.raw_album_name` (a backfill updates an older
  row that lacks it), because a remote row has no album entity for the finalize tag
  step to read.
- **Local-first registration.** Before creating a remote row, `EnsureRemoteTrack`
  looks for an on-disk row for the same title and a compatible artist set. Duration
  is only a hint (≤10s is normal; a larger difference still matches and is flagged
  suspicious), and the candidate with the smallest difference wins. A hit is
  returned instead of registering a duplicate; the provider identity is written
  onto it only when it has none, and a row already bound to another provider keeps
  that identity. `RemoteTrackSpec.Force` (download anyway, re-download) skips the
  lookup. The lookup is best-effort: a failure logs and falls through to
  registration.
- **Tags are verified by the import, not by the tagger.** After importing the
  finished file, `VerifyImportedTrack` re-reads the row through the library's own
  parser. A row without a title or artist is deleted (row, file and index) and the
  download fails with `domain.ErrDownloadPermanent`, so a tag write that only the
  tagger can see never becomes a search-less ghost row.
- **Removing never deletes.** Un-favoriting or removing from a playlist only drops
  the relationship; the file stays in the library. There is no implicit deletion.

`CatalogRowStates` answers what the search row should show, for a whole page in one
call, from the identity's **media request**: `in_library` (audio on disk, or a `done`
request whose merge failed), `downloading` (wanted/queued/running, or a failed request
with a retry scheduled), `failed` (a permanent failure, where the UI offers a retry),
`saved` (registered with no request in flight, or canceled), or no state (never
registered). It looks each identity up by its indexed `(provider, external_id)`, so a
50-row page costs 50 index hits and no table scan. `CatalogRowStates` is also exposed to
the UI as `CatalogService.GetCatalogRowStates`: after `download:done` the remote row is
already merged away, so the page re-reads the badge by identity instead of by the
track id the event carries.

`CatalogLocalMatches` covers the other half: a file the user imported has
`provider=''`, so the identity lookup cannot see it. Rows with no registered
identity are matched against ready on-disk tracks by normalised title, a
**compatible** artist set (`domain.ArtistSetsCompatible`: one set contains the
other and they share an artist, so a source that reports only the primary artist
matches a file crediting the featured artist too) and a duration hint. A
difference of ≤10s is normal; a larger one still matches but reports
`suspicious`, and the closest candidate wins, because a second row is worse than
a flagged match. A hit reports `in_library` plus `local_track_id` (and
`suspicious` when flagged). Both sides are expanded one hop through
`artist_aliases` first, so `Camellia` matches a file tagged `かめりあ`.
The action reuses that row (favorite/playlist only) unless the user explicitly
chooses "download anyway", which registers the remote identity and enqueues.
`MatchLocalTrack` exposes the same rule as a single-track port, which is what the
Last.fm worker and `EnsureStreamableTrack` call before touching the network.

## Settings

| Column | Meaning |
|---|---|
| `download_dir` | absolute target directory; empty means "resolve the default (`<Music>/Airmedy`) at read time" |
| `download_quality` | `128k` / `320k` / `flac`; copied onto the job so a later preference change does not alter a queued job |
| `download_naming_order` | `title-artist` (default) or `artist-title`; only affects newly finalized files, existing files keep their names |
| `auto_download_on_playlist_add` | when on, adding a remote track to a playlist upserts a media request (default on). When off, the row is saved as `saved` without work; a cached stream copy is still promoted, and a favorite is always an explicit request |
| `download_with_lyrics` | fetch and store lyrics for the downloaded track (off by default: an extra request per track) |
| `download_with_lyric_translation` | keep the translated lyric line; meaningless without `download_with_lyrics`, which `ResolveDownloadContent` enforces |
| `download_with_cover` | embed the cover image into the file's tags (on by default: it is what makes a downloaded file look right, and a missing cover is already tolerated) |

## Events

`download:progress`, `download:done`, `download:failed` — payload carries `job_id`,
`track_id`, `playlist_id`, `state`, `bytes_done`, `bytes_total`, `percent`, `error`.
The frontend keys its per-track state by `track_id` so a playlist row can ask "is this
still downloading?" while rendering. A download reaching `done` or a permanent
`failed` also emits `task:updated` so the tasks page reloads.

## Tasks page

`DownloadService.ListTasks()` returns one `domain.TaskRun` per **media request**, plus
one per `stream_cache_tasks` session: `kind` (`request`/`cache`), `state`
(`active`/`waiting`/`done`/`failed`/`canceled`), `phase` (resolving/downloading/
promoting/verifying while a request runs), track, title/artist, quality, error and
timestamps, newest first. A request within its retry backoff reads as `waiting`
("waiting to retry"), not `active`/"downloading" and not `failed`; once the
20-attempt limit is reached there is no next attempt and it reads `failed`.
A cache session stopped on purpose (track switch, shutdown, user stop) is
`canceled` with an empty `error`; only a real transport/parse/disk failure is
`failed`. Migration 000108 rewrote historical `failed`/`error='interrupted'`
rows to `canceled`. The page puts canceled cache rows under "completed" (grey
"canceled"), while a canceled request stays under "errors" because it still
needs attention.
The row carries the stable `request_id` (equal to the request row's `id`) plus
`provider`/`external_id`; the page keys and merges rows by the request id, so a
merge that replaces `track_id` cannot leave a second row behind. Titles prefer the
title/artist snapshot written when the request was created, then one batched track
lookup; a row a merge already deleted falls back to its external id, never to the
invalidated track id. Cache rows carry the title and artist the entry was created
with. The worker emits `task:updated` after every running/done/failed transition,
so the page reloads instead of keeping a merge-time snapshot; the page also
re-reads on mount, KeepAlive activation and window focus.

The page groups rows into in-progress / completed / errors, labels the kind and shows
the phase as the row's status text. A request offers cancel while it runs and retry
after it failed; `CancelTask`/`RetryTask` operate on the request, and retry reopens it
as `wanted` with the same intent. A cache row has no actions.

`ClearTaskHistory()` deletes terminal requests, the retired job history and finished
cache sessions; queued/active work is never touched. Requests keep at most 500 finished
rows (trimmed by the worker after a terminal transition), and clearing does not rewrite
`tracks`.

## Media requests

`media_requests` (000103) is the authoritative record of "the user wants this
online track in the library". Every save entry point — favorites, single and batch
playlist adds, and catalog/search/Last.fm saves — goes through
`mediarequest.Saver.OnRemoteTrackSaved`, which upserts one row by
`(provider, external_id)` and wakes the worker. Re-saving the same intent returns the
existing row, while a finished/canceled/failed row is reopened as `wanted` when the
intent changes. A non-explicit playlist add still honours
`auto_download_on_playlist_add`. `media_request_events` is append-only history.

`mediarequest.Worker` sweeps serially (no two passes over one identity), assigns the
shared retry backoff (1m/5m/30m/2h/6h, 20 attempts) and delegates execution through
`domain.MediaRequestExecutor`, implemented by the download service. `Execute` applies
the cache/quality rule: an active job wins; a complete cache meeting the wanted tier is
promoted in place; anything else enqueues a download. It returns
`domain.ErrMediaRequestDeferred` while playback owns the cached file, and the request
stays `wanted` for the session-close wake. `Status` reports the newest job failure
created after the run started, so a permanent failure fails the request and a newer
success clears an older failure. A local row closes the request as `done`. An explicit
re-download (`DownloadService.RedownloadTrack`) deletes the damaged file when needed and
upserts `desired=download`, which reopens a finished request; the worker performs the
download.

A request whose track row disappeared (a merge or delete invalidated the id) no longer
retries: the worker first re-resolves the identity by `(provider, external_id)` — the id
may have changed — then matches the stored title/artist snapshot against the local
library. A hit closes the request as `done` with event detail `resolved: local row`;
otherwise it is `canceled` with `canceled: track row removed`, which is terminal until
the next explicit save. A merge (`track:merged`) and a file deletion additionally
converge the other requests of the same normalised artist+title (`MediaRequestMatchKey`,
the recommendation match-cache rule): each becomes `done` when its own identity has a
playable row, else `canceled` (`canceled: duplicate identity`). This is why one song
promoted through one provider no longer leaves another provider's request "downloading"
forever; see `docs/specs/library/fix-orphan-request.md`.

At startup the worker also runs `ListOrphans` (`state = failed` and a non-empty
`track_id` with no `tracks` row) and settles those rows immediately, ignoring
`next_attempt_at`: a deleted row is unrecoverable, so waiting out a six-hour
backoff would only show a stuck "waiting to retry" row. The same local-match
rule decides done vs canceled. Requests with an empty `track_id` are not orphans —
they resolve their identity on the next run. The tasks page and the search/Last.fm chips read only requests, and the retired
table/flags are gone — see `docs/specs/library/media-request-state-machine.md`.

## Duplicate-cache cleanup

`ScanDuplicateCache` / `CleanDuplicateCache` (exposed on `DownloadService`) share one
plan, so the report and the deletion cannot disagree. It covers three kinds: duplicate
groups (normalized title + artist equal, duration within 5 s when both report one),
entries whose identity already maps to a local track, and `.audio` files with no row. A
group keeps the best copy by quality (`flac > 320k > 128k`), then `complete`, then
`last_used`; if the top two tie on all three, the group is skipped rather than guessed.
An entry whose identity has an **active media request** (wanted/queued/running, or
failed with a retry scheduled) is skipped, because the request still needs it; so is an
entry a playback session owns.

`CleanDuplicateCache` pauses playback only when the loaded track owns one of the
candidates (`CacheCleanupPlayer`), releases the stream session, deletes entry by entry
and orphan by orphan (one failure is reported and the batch continues), then resumes
the same track and position. The queue, repeat/shuffle and current track are never
touched. Both calls are read-only until confirm; a clean emits `task:updated`.

## Failure handling worth remembering

- A queue that fails to start must not take the player down: the fx hook logs and
  continues.
- A job whose track row vanished (or was already downloaded by a crashed attempt) is
  closed out instead of retried.
- Progress writes are best-effort: a failed `UpdateProgress` is logged at debug level and
  the transfer continues.

## Repository rules that bite

- `MarkDone` accepts an `active` job only; calling it on a `queued` one is an error.
- `ClaimNext` runs in its own transaction — do not wrap it in an outer `RunTx`.
- The partial unique index covers `queued`/`active` **only** on purpose: re-enqueuing a
  track after it reached `done` is legal. A full-table unique constraint breaks that.
- `ExportM3U8` writes a remote track as a `# remote: <provider>/<id>` comment rather
  than skipping it, so playlist order survives.
- Queue restore keeps remote tracks; it must not fall back to a bare `os.Stat` skip,
  which deletes every not-yet-downloaded row.
- Path-prefix queries are guarded: `GetByPathPrefix("")` returns empty and
  `DeleteByPathPrefix("")` errors. Do not bypass either.
- Migration tests pin the version (`m.Migrate(77)`) because later migrations exist.
- SQLite tests open `NewDB(":memory:")`: a file-backed DB on Windows fails to release
  its lock during temp-dir cleanup.

## Tests

- `internal/app/download/*_test.go` — worker loop, retry classification, finalize,
  merge (M1/M2), auto-download hook, remote-track registration, the content switches
  (cover gated on the setting, lyrics only when asked, a lyric failure not failing the
  job), and the provider-toggle policy in `lyrics_test.go`.
- `internal/domain/download_test.go` — `ResolveDownloadContent` including the
  translation-without-lyrics collapse.
- `internal/infra/downloader/downloader_test.go` — 200/206/resume/truncation/403/timeout.
- `internal/infra/sqlite/download_repository_test.go` — idempotent enqueue, claim ordering
  and retry time, retry reusing the row, restart recovery, playlist-delete SET NULL.
- `internal/app/stream/plugin_resolver_test.go` — provider lookup, structured error
  kinds, crashes, missing method, empty external id and empty provider.
