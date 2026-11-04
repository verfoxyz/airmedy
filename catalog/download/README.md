# Download Pipeline

## Summary

Turns an online track into a local library file. A remote track (a `tracks` row with no
file yet, see [database](../database/README.md#full-schema)) is queued in `download_jobs`,
resolved to a stream URL, downloaded to a partial file, validated, tagged, imported into
the library and finally attached to the playlist that asked for it.

The pipeline owns no UI and no network policy: adapters are injected as domain ports, and
the only external interaction is "give me a URL for this track" plus "move these bytes".

## Boundaries

| Layer | Owns |
|---|---|
| `internal/app/download` | queue worker, retry policy, finalize, merge, auto-download decision |
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

## Finalize

1. Target path is `<download_dir>/<name>.<ext>` — flat, no artist/album subfolders. `<name>`
   is `title - artist` by default, or `artist - title` when `download_naming_order` is
   `artist-title`. An existing path with **identical bytes** is reused; a different file
   already using the name gets `(2)`, `(3)`, … and is never overwritten. A reused file is
   re-tagged in place — identical bytes say nothing about its tags — and the staged duplicate
   is removed.
2. Validate the partial file: byte count, signature, decoded duration within ±3s of the
   expected duration.
3. Write tags with taglib, including the cover when the enqueue carried one. Cover URLs
   live in memory only: a restart loses the cover, never the download. Tags are read back
   afterwards; an empty title or artist is logged as a warning.
4. Atomically rename the validated part file into place.
5. `LibraryService.ImportFileWithID(path)` → the track id this file became.
6. Attach to `playlist_id` when the job carries one (it may be NULL: deleting the playlist
   does not abort an in-flight download, the FK is `ON DELETE SET NULL`).

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
the file, so `IsRemote()` still reported true and the decoder was handed a path that
is not a file (`ma_player_load failed: -10`). Two invariants follow:

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
- **Tags are verified by the import, not by the tagger.** After importing the
  finished file, `VerifyImportedTrack` re-reads the row through the library's own
  parser. A row without a title or artist is deleted (row, file and index) and the
  download fails with `domain.ErrDownloadPermanent`, so a tag write that only the
  tagger can see never becomes a search-less ghost row.
- **Removing never deletes.** Un-favoriting or removing from a playlist only drops
  the relationship; the file stays in the library. There is no implicit deletion.

`CatalogRowStates` answers what the search row should show, for a whole page in one
call: `in_library` (audio on disk), `downloading` (queued/active job), `failed` (the
newest job failed permanently), `saved` (registered but no job), or no state
(never registered or canceled with no replacement). It reads the job list once and
looks each identity up by its indexed `(provider, external_id)`, so a 50-row page
costs one queue read plus 50 index hits. `CatalogRowStates` is also exposed to the
UI as `CatalogService.GetCatalogRowStates`: after `download:done` the remote row is
already merged away, so the page re-reads the badge by identity instead of by the
track id the event carries.

`CatalogLocalMatches` covers the other half: a file the user imported has
`provider=''`, so the identity lookup cannot see it. Rows with no registered
identity are matched against ready on-disk tracks by normalised title, primary
artist and duration within ±3s; a hit reports `in_library` plus `local_track_id`.
The action reuses that row (favorite/playlist only) unless the user explicitly
chooses "download anyway", which registers the remote identity and enqueues.

## Settings

| Column | Meaning |
|---|---|
| `download_dir` | absolute target directory; empty means "resolve the default (`<Music>/Airmedy`) at read time" |
| `download_quality` | `128k` / `320k` / `flac`; copied onto the job so a later preference change does not alter a queued job |
| `download_naming_order` | `title-artist` (default) or `artist-title`; only affects newly finalized files, existing files keep their names |
| `auto_download_on_playlist_add` | when on, adding a remote track to a playlist enqueues it (default on). When off, the row is saved into the library as `saved` without a job; adding to favorites always enqueues |
| `download_with_lyrics` | fetch and store lyrics for the downloaded track (off by default: an extra request per track) |
| `download_with_lyric_translation` | keep the translated lyric line; meaningless without `download_with_lyrics`, which `ResolveDownloadContent` enforces |
| `download_with_cover` | embed the cover image into the file's tags (on by default: it is what makes a downloaded file look right, and a missing cover is already tolerated) |

## Events

`download:progress`, `download:done`, `download:failed` — payload carries `job_id`,
`track_id`, `playlist_id`, `state`, `bytes_done`, `bytes_total`, `percent`, `error`.
The frontend keys its per-track state by `track_id` so a playlist row can ask "is this
still downloading?" while rendering.

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
