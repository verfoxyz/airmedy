# Database

<!-- toc -->
- [Summary](#summary)
- [Files](#files)
- [Connection Setup](#connection-setup)
- [Migration History](#migration-history)
- [Full Schema](#full-schema)
  - [Core Entity Tables](#core-entity-tables)
  - [Junction Tables (Many-to-Many)](#junction-tables-many-to-many)
  - [State Tables (Single-Row)](#state-tables-single-row)
  - [Listening Insights Tables](#listening-insights-tables)
  - [EQ Tables](#eq-tables)
  - [Indexes](#indexes)
- [Repository Patterns](#repository-patterns)
  - [TrackRepository — Key Queries](#trackrepository-key-queries)
  - [AlbumRepository — Key Queries](#albumrepository-key-queries)
  - [PlaylistRepository — Track Ordering (LexoRank)](#playlistrepository-track-ordering-lexorank)
- [ID Generation](#id-generation)
<!-- /toc -->

## Summary

SQLite database managed via `golang-migrate` for schema versioning and `sqlx` for query execution. The schema uses a normalized relational model with many-to-many junction tables for artist/genre/composer relationships.

## Files

| File                                    | Purpose                      |
| --------------------------------------- | ---------------------------- |
| `internal/infra/sqlite/sqlite.go`       | Connection, migration runner |
| `internal/infra/sqlite/migrations/`     | SQL up/down migration files  |
| `internal/infra/sqlite/columns.go`      | Shared SQL column selections |
| `internal/infra/sqlite/*_repository.go` | Repository implementations   |
| `internal/infra/sqlite/listening_repository.go` | Listening session persistence and analytics aggregates |

## Connection Setup

- Driver: `github.com/mattn/go-sqlite3` (cgo)
- Query builder: `github.com/jmoiron/sqlx`
- Migrations: `github.com/golang-migrate/migrate/v4`
- Write serialization: single write connection with WAL mode enabled.
- Migrations run automatically on startup before any service initializes.

## Migration History

| #      | File                           | Change                                                                                                                             |
| ------ | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| 000001 | `init_schema.up.sql`           | Full initial schema (tracks, albums, artists, genres, composers, playlists, lyrics, watched_folders, all junction tables, indexes) |
| 000002 | `eq_profiles.up.sql`           | Add `eq_profiles` and `eq_bands` tables                                                                                            |
| 000003 | `favorites.up.sql`             | `ALTER TABLE tracks ADD COLUMN is_favorite INTEGER DEFAULT 0`, add index                                                           |
| 000004 | `player_state.up.sql`          | Add `player_state` table (single-row, CHECK id = 1)                                                                                |
| 000005 | `app_settings.up.sql`          | Add `app_settings` table (single-row)                                                                                              |
| 000006 | `app_settings_theme.up.sql`    | `ALTER TABLE app_settings ADD COLUMN theme TEXT DEFAULT 'system'`                                                                  |
| 000007 | `playlist_artwork.up.sql`      | `ALTER TABLE playlists ADD COLUMN artwork_key TEXT`                                                                                |
| 000008 | `extra_track_metadata.up.sql`  | Add `bpm`, `label`, `isrc`, `play_count` to `tracks`                                                                               |
| 000009 | `eq_profile_is_default.up.sql` | Add `is_default` to `eq_profiles`, set all existing = 1                                                                            |
| 000010 | `app_settings_lastfm.up.sql`   | `ALTER TABLE app_settings ADD COLUMN lastfm_username TEXT`                                                                         |
| 000011 | `app_settings_updates.up.sql`  | Add `auto_check_update`, `start_at_login` to `app_settings`                                                                       |
| 000012 | `playlist_lexorank.up.sql`     | Convert `playlist_tracks.position` from INTEGER to TEXT (LexoRank string), migrate existing data with computed ranks              |
| 000013 | `app_settings_eq.up.sql`       | `ALTER TABLE app_settings ADD COLUMN eq_enabled BOOLEAN DEFAULT 0`                                                                |
| 000014 | `meta_lyrics.up.sql`                 | Add `meta_content` and `meta_source` to `lyrics` table; add `lrclib_mode` to `app_settings`; backfill lyrics from `other_metadata` |
| 000015 | `artist_artwork.up.sql`              | `ALTER TABLE artists ADD COLUMN artwork_key TEXT`                                                                                    |
| 000016 | `app_settings_artist_artwork.up.sql` | `ALTER TABLE app_settings ADD COLUMN use_online_artist_artwork BOOLEAN NOT NULL DEFAULT 1`                                          |
| 000017 | `lyrics_provider_settings.up.sql`    | Add `enable_lrclib`, `enable_kugou`, `prefer_metadata_lyrics` (all `BOOLEAN NOT NULL DEFAULT 1`) to `app_settings`                 |
| 000018 | `app_settings_tray.up.sql`           | `ALTER TABLE app_settings ADD COLUMN show_tray_icon BOOLEAN NOT NULL DEFAULT 1`                                                    |
| 000019 | `app_settings_prevent_sleep.up.sql`  | `ALTER TABLE app_settings ADD COLUMN prevent_sleep_while_playing BOOLEAN NOT NULL DEFAULT 0`                                       |
| 000020 | `remote_server.up.sql`               | Add `remote_server_enabled` (0), `remote_server_port` (0), `remote_server_password` (`''`) to `app_settings`                       |
| 000021 | `show_player_indicator.up.sql`       | `ALTER TABLE app_settings ADD COLUMN show_player_indicator BOOLEAN NOT NULL DEFAULT FALSE` (down is a no-op — column kept)         |
| 000022 | `prefer_local_lyrics.up.sql`         | `ALTER TABLE app_settings RENAME COLUMN prefer_metadata_lyrics TO prefer_local_lyrics` (value preserved)                           |
| 000023 | `artist_artwork_source.up.sql`       | `ALTER TABLE artists ADD COLUMN artwork_source TEXT NOT NULL DEFAULT ''` — tracks artwork origin (`online`/`local_file`/`manual`)   |
| 000024 | `app_settings_artist_artwork.up.sql` | Add `prefer_local_artist_artwork BOOLEAN NOT NULL DEFAULT 1` and `last_scan_version TEXT NOT NULL DEFAULT ''` to `app_settings`     |
| 000026 | `drop_prefer_local_artist_artwork.up.sql` | Drop `prefer_local_artist_artwork` — replaced by deriving from `use_online_artist_artwork` |
| 000025 | `artist_artwork_multi_source.up.sql` | Replace `artwork_key`/`artwork_source` with per-source `artwork_key_manual`/`_local`/`_online` (backfilled), then drop the old two |
| 000027 | `lyrics_folder.up.sql`               | Add `lyrics_folder_enabled`, `lyrics_folder_path`, `lyrics_subfolder_enabled`, `lyrics_subfolder_name` to `app_settings` (dedicated folder + per-track subfolder lyrics lookup) |
| 000028 | `prefer_local_artist_artwork.up.sql` | Re-add `prefer_local_artist_artwork BOOLEAN NOT NULL DEFAULT 1` — nested sub-toggle: when online artwork is on, a local/manual image suppresses the Deezer one |
| 000029 | `mini_player_state.up.sql`           | Add `mini_player_state` table (single-row, CHECK id = 1) — persists mini player window geometry (`x`, `y`, `width`, `height`), `always_on_top`, and `has_position` |
| 000030 | `tag_delimiters.up.sql`              | Add `artist_delimiters`, `album_artist_delimiters`, `genre_delimiters`, `composer_delimiters` to `app_settings` (TEXT, JSON arrays, default `'[";","\\",","]'`) — user-configurable multi-value tag splitting |
| 000031 | `library_sync_state.up.sql`          | Add `library_sync_state` table (single-row, CHECK id = 1) with `delimiters_signature TEXT` — records the delimiter config the library data currently reflects, so a sync knows whether to re-split |
| 000032 | `add_comma_default_delimiter.up.sql` | Add `,` to the default delimiter set: `UPDATE app_settings SET <col> = '[";","\\",","]' WHERE <col> = '[";","\\"]'` (only rows still on the previous default; user-customized lists untouched) |
| 000033 | `update_default_delimiters.up.sql`   | Update default delimiters: change single backslash `\` to double backslash `\\` (JSON `'[";","\\\\",","]'`) for rows still on the previous default |
| 000034 | `track_features.up.sql`              | Add `track_features` table (one-time DSP analysis: loudness/dynamics/spectral + reserved-null mood cols); add `tracks.analyzed_version INTEGER NOT NULL DEFAULT 0` (0 = pending) + `idx_tracks_analyzed_version`; add `normalization_enabled`, `normalization_mode` ('off'), `normalization_target_lufs` (-14), `normalization_prevent_clip` (1) to `app_settings` |
| 000035 | `junction_reverse_indexes.up.sql`    | Add reverse-lookup indexes on junction tables (`idx_track_artists_artist_id`, `idx_track_album_artists_artist_id`, `idx_track_genres_genre_id`, `idx_track_composers_composer_id`, `idx_album_artists_artist_id`) — speeds orphan-cleanup anti-joins and `GetBy{Artist,Genre,Composer}ID` |
| 000036 | `onset_variance.up.sql`              | `ALTER TABLE track_features ADD COLUMN onset_variance REAL` (danceability input; down keeps column — SQLite `DROP COLUMN` unsafe across versions) |
| 000037 | `corpus_feature_stats.up.sql`        | Add `feature_percentiles` table (per-feature `p1/p5/p50/p95/p99` + `sample_count`/`computed_at`, corpus normalization stats for mood derivation); add `app_settings.mood_derivation_version INTEGER NOT NULL DEFAULT 0` |
| 000038 | `track_mood_version.up.sql`          | `ALTER TABLE tracks ADD COLUMN mood_derived_version INTEGER NOT NULL DEFAULT 0` + `idx_tracks_mood_derived_version` — marks a track's mood stale vs `app_settings.mood_derivation_version` for re-derivation |
| 000054 | `track_brightness.up.sql`            | Add corpus-normalized `track_features.brightness`, derived from spectral centroid for Mood Radio similarity; invalidate cached mood values for backfill |
| 000055 | `track_brightness_index.up.sql`      | Add `idx_track_features_brightness` for live Smart Playlist brightness ranges |
| 000039 | `track_bitdepth_codec.up.sql`        | `ALTER TABLE tracks ADD COLUMN bit_depth INTEGER NOT NULL DEFAULT 0`, `ADD COLUMN codec TEXT NOT NULL DEFAULT ''` — bits-per-sample and inner codec (e.g. m4a `aac`/`alac`) from the `go-taglib` fork, used to classify Lossy/Lossless/Hi-Res/DSD |
| 000040 | `metadata_schema_version.up.sql`     | `ALTER TABLE library_sync_state ADD COLUMN metadata_schema_version INTEGER NOT NULL DEFAULT 0` — tracks which extractor field-set a library's data reflects, so a sync can force one full re-parse when it's behind (see `catalog/library`) |
| 000047 | `library_analysis_worker_count.up.sql` | `ALTER TABLE app_settings ADD COLUMN library_analysis_worker_count INTEGER NOT NULL DEFAULT 2` — persists the desired concurrent worker count for the library-analysis pool; down intentionally keeps the column |
| 000048 | `crossfade_seconds.up.sql`              | `ALTER TABLE app_settings ADD COLUMN crossfade_seconds INTEGER NOT NULL DEFAULT 0` — track-transition overlap in seconds, 0 = off/gapless (see `catalog/player`) |
| 000049 | `blend_artwork_during_crossfade.up.sql` | `ALTER TABLE app_settings ADD COLUMN blend_artwork_during_crossfade BOOLEAN NOT NULL DEFAULT 1` — fullscreen artwork blend during automatic crossfade |
| 000050 | `high_contrast_lyrics.up.sql` | `ALTER TABLE app_settings ADD COLUMN high_contrast_lyrics BOOLEAN NOT NULL DEFAULT 1` — fullscreen lyrics glass panel; false uses the immersive panel |
| 000051 | `analysis_component_versions.up.sql` | Add `track_analysis_components` for independently versioned `ffmpeg`/`aubio` raw analysis; backfill only legacy tracks with `analyzed_version >= 4` (missing feature row becomes `failed`) |
| 000052 | `track_analysis_pending_mask.up.sql` | Add indexed `tracks.analysis_pending_mask` (FFmpeg=1, aubio=2), backfilled from component versions; makes progress/backfill proportional to unresolved tracks |
| 000053 | `pending_analysis_backfill_order.up.sql` | Add partial `(created_at, id)` index for stable pending-analysis backfill without a temp sort |
| 000056 | `eq_profile_preamp_gain.up.sql` | Add legacy per-profile `preamp_gain` to `eq_profiles` |
| 000057 | `app_settings_stereo_width.up.sql` | Add global `stereo_width` (default 100) to `app_settings` |
| 000058 | `global_eq_preamp.up.sql` | Move the active profile's legacy preamp to global `app_settings.eq_preamp`, then remove `eq_profiles.preamp_gain` |
| 000059 | `eq_preset_key.up.sql` | Add unique non-empty `eq_profiles.preset_key` for stable built-in preset identity |
| 000060 | `app_settings_auto_advance_notifications.up.sql` | Add `app_settings.auto_advance_notifications_enabled BOOLEAN NOT NULL DEFAULT TRUE` for the macOS automatic-track notification preference |
| 000062 | `listening_insights.up.sql` | Add append-only `listening_sessions` plus per-track/day aggregate `daily_track_listening_stats`, with date indexes, for listening insights |
| 000063 | `playback_attempts.up.sql` | Add raw playback attempts plus daily completion/skip/stop aggregates for completion and skip rates |
| 000064 | `playback_attempt_listening_time.up.sql` | Add aggregate listened seconds to calculate average playback session length |
| 000065 | `mobile_pairing.up.sql` | Add desktop pairing identity and trusted mobile public-key tables |
| 000067 | `mobile_library_sync.up.sql` | Add persisted per-device library-sync plans and idempotent asset receipts |
| 000068 | `mobile_sync_lyric_cache.up.sql` | Add desktop cache of effective lyric snapshots, output versions, and input fingerprints used while creating mobile-sync plans |
| 000069 | `mobile_playlist_sync.up.sql` | Add durable per-device playlist-mutation ledger used to deduplicate mobile reconciliation retries |
| 000070 | `mobile_playlist_reconciliation.up.sql` | Add global playlist LWW/tombstone watermarks and device/reconciliation-owned artwork staging with expiry index |
| 000071 | `mobile_favorite_sync.up.sql` | Add trusted-device favorite mutation ledger and per-track last-write-wins favorite watermark for mobile reconciliation |
| 000072 | `listening_sources.up.sql` | Add source-device identity to raw listening rows and daily aggregate keys for cross-device sync |
| 000073 | `living_artwork_background.up.sql` | Add `app_settings.living_artwork_background BOOLEAN NOT NULL DEFAULT 1` |
| 000074 | `romanization_enabled.up.sql` | Add `app_settings.romanization_enabled BOOLEAN NOT NULL DEFAULT 1` |
| 000075 | `lyrics_glow.up.sql` | Add `app_settings.lyrics_glow BOOLEAN NOT NULL DEFAULT 0` |
| 000076 | `show_player_favorite_button.up.sql` | Add `app_settings.show_player_favorite_button BOOLEAN NOT NULL DEFAULT 0`; down removes the column |
| 000077 | `remote_tracks.up.sql` | Add `tracks.availability/provider/external_id` plus a partial unique index on `(provider, external_id)` for rows with an external id. `path` stays NOT NULL UNIQUE: an online track that has no file yet carries the virtual `remote://<provider>/<external_id>` path, so `domain.RemotePath` is the only writer and the unique index deduplicates per source without rebuilding the table |
| 000078 | `download_jobs.up.sql` | Add `download_jobs`, the queue for remote tracks. A partial unique index on `track_id` covers only `queued`/`active`, so a second enqueue is rejected at the DB level; retries UPDATE the same row (`attempts + 1`, back to `queued`). `playlist_id` is nullable with `ON DELETE SET NULL` so removing a playlist never aborts a download |
| 000079 | `download_settings.up.sql` | Add `app_settings.download_dir/download_quality/auto_download_on_playlist_add`; an empty `download_dir` means "resolve the default at read time" |
| 000080 | `plugin_settings.up.sql` | Add `plugin_settings(plugin_id, enabled, updated_at)`, the per-plugin enable flag. A missing row means *disabled*: enabling a plugin launches a third-party process, so the default has to be off. One row per plugin — a plugin is one functional unit with no parent row to defer to, so the schema needs no hierarchy. Down drops the table |
| 000081 | `lx_sources.up.sql` | Add `lx_sources(id, name, file_path, enabled, position, created_at, updated_at)` plus an index on `position`, the imported lx custom-source list. The lx plugin runs the scripts; the core owns the list because the plugin is a child process with no persistence and the user manages it from settings. `position` is the try order (`0..n-1`, rewritten transactionally by reorder) so a concurrent read never sees two rows at the same priority. Down drops the table |
| 000085 | `download_naming_order.up.sql` | Add `app_settings.download_naming_order TEXT NOT NULL DEFAULT 'title-artist'`. Values are `title-artist` / `artist-title`; the repository falls back to the default on read, and the settings service rejects any other value on save. Only affects files finalized after the change |
| 000084 | `repair_remote_ready_rows.up.sql` | Repairs rows a finished download left unplayable: a leftover `remote://` row marked `ready` while another row held the file. Moves the user's favorite/play count/playlist entries onto the row that owns the file, deletes the leftover, then gives the survivor the identity (steps in that order — `idx_tracks_provider_external` is UNIQUE). Rows whose target file is no longer in the library are left alone. Down is a deliberate no-op: the split cannot be reconstructed. |
| 000083 | `download_content.up.sql` | Add `app_settings.download_with_lyrics/download_with_lyric_translation/download_with_cover`, the three download-content switches. Each is an independent network request, so the defaults matter: lyrics and translation off, cover on. The translation/lyrics contradiction is collapsed in `domain.ResolveDownloadContent`, not in SQL. Down drops the three columns |
| 000082 | `plugin_kv.up.sql` | Add `plugin_kv(plugin_id, key, value, updated_at)` with a composite primary key on `(plugin_id, key)`, the only persistence a plugin has (`storage.*`). `value` is opaque TEXT: the plugin's own JSON, never parsed by the host. Limits (64 KiB per value, 128-byte keys, 256 keys per plugin) are enforced in `app/pluginkv` rather than by CHECK constraints, so raising one needs no migration. Down drops the table |
| 000086 | `download_jobs_keep_history.up.sql` | Rebuild `download_jobs` to drop the `track_id → tracks(id) ON DELETE CASCADE` FK. Deleting a track (or folding a remote row into the imported one) must not erase its download history; `DeleteByTrack` removes jobs explicitly when the user deletes a file. `playlist_id` keeps `ON DELETE SET NULL`, and the two indexes are recreated. Down restores the FK and copies only rows whose track still exists |
| 000087 | `track_raw_album_name.up.sql` | Add `tracks.raw_album_name TEXT NOT NULL DEFAULT ''`. A remote row has no album entity, so this is the only place the search result's album name can live until the downloaded file is tagged with it. Down drops the column |

`listeningRepository.GetListeningInsights` returns up to 50 entries for both Top Artists
(ordered by listened seconds) and Top Tracks (ordered by play count, then listened
seconds, then title for a stable tie-break).

## Full Schema

### Core Entity Tables

```sql
artists (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    sort_name TEXT NOT NULL,
    normalization_key TEXT,
    artwork_key_manual TEXT,  -- user-set image (highest priority)
    artwork_key_local TEXT,   -- scanned artist.jpg/png
    artwork_key_online TEXT,  -- Deezer fetch
    created_at DATETIME,
    updated_at DATETIME
)

albums (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    sort_title TEXT NOT NULL,
    normalization_key TEXT,
    year INTEGER,
    copyright TEXT,
    artwork_key TEXT,
    created_at DATETIME,
    updated_at DATETIME
)

genres (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    normalization_key TEXT
)

composers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    normalization_key TEXT
)

tracks (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL,
    sort_title TEXT NOT NULL,
    album_id TEXT REFERENCES albums(id) ON DELETE SET NULL,
    year INTEGER,
    track_number INTEGER,
    total_tracks INTEGER,
    disc_number INTEGER,
    total_discs INTEGER,
    duration INTEGER,           -- seconds
    bitrate INTEGER,
    sample_rate INTEGER,
    format TEXT,
    bit_depth INTEGER NOT NULL DEFAULT 0,  -- 0 = unknown/legacy row, not yet re-synced (000039)
    codec TEXT NOT NULL DEFAULT '',        -- inner codec for container formats, e.g. m4a aac/alac (000039)
    artwork_key TEXT,
    raw_artist_names TEXT,
    raw_album_artist_names TEXT,
    raw_album_name TEXT NOT NULL DEFAULT '',  -- album name from the online search result (000087)
    raw_genre_names TEXT,
    raw_composer_names TEXT,
    copyright TEXT,
    other_metadata TEXT,        -- JSON blob of all raw tags
    file_size INTEGER DEFAULT 0,
    bpm INTEGER,
    label TEXT,
    isrc TEXT,
    play_count INTEGER DEFAULT 0,
    is_favorite INTEGER DEFAULT 0,
    analyzed_version INTEGER NOT NULL DEFAULT 0,  -- 0 = pending DSP analysis (000034)
    analysis_pending_mask INTEGER NOT NULL DEFAULT 3, -- FFmpeg=1, aubio=2; indexed unresolved work (000052)
    mood_derived_version INTEGER NOT NULL DEFAULT 0,  -- stale vs app_settings.mood_derivation_version → re-derive (000038)
    availability TEXT NOT NULL DEFAULT 'ready',       -- ready | remote | downloading | failed (000077); non-ready rows carry a remote:// path
    provider TEXT NOT NULL DEFAULT '',                -- '' = local file; otherwise the source id, e.g. wy (000077)
    external_id TEXT NOT NULL DEFAULT '',             -- provider-side track id; unique per (provider, external_id) (000077)
    mtime DATETIME,
    created_at DATETIME,
    updated_at DATETIME
)

track_features (   -- one-time DSP analysis (000034); 0 rows until analyzer runs
    track_id TEXT PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    analyzer_version INTEGER NOT NULL DEFAULT 0,
    analyzed_at DATETIME,
    loudness_lufs REAL, loudness_range REAL, true_peak REAL, rms REAL, crest REAL,        -- ebur128 + astats
    spectral_centroid REAL, spectral_rolloff REAL, spectral_flatness REAL,
    spectral_flux REAL, zcr REAL,                                                          -- aspectralstats
    onset_variance REAL,                                                                   -- aubio onset spread; danceability input (000036)
    tempo REAL,                                                                             -- aubio tempo
    energy REAL, danceability REAL, brightness REAL                                         -- derived from raw features vs feature_percentiles; brightness is normalized spectral centroid
)

track_analysis_components (   -- independent raw-source freshness (000051)
    track_id TEXT REFERENCES tracks(id) ON DELETE CASCADE,
    component TEXT,           -- ffmpeg | aubio
    version INTEGER,
    status TEXT,              -- complete | failed
    analyzed_at DATETIME,
    PRIMARY KEY (track_id, component)
)

feature_percentiles (   -- corpus-wide normalization stats, one row per raw feature (000037)
    feature_name TEXT PRIMARY KEY,                        -- rms | spectral_centroid | spectral_flux | tempo | crest | onset_variance | loudness_range
    p1 REAL NOT NULL, p5 REAL NOT NULL, p50 REAL NOT NULL, p95 REAL NOT NULL, p99 REAL NOT NULL,
    sample_count INTEGER NOT NULL,
    computed_at DATETIME NOT NULL
)

playlists (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    artwork_key TEXT,
    created_at DATETIME,
    updated_at DATETIME
)

download_jobs (   -- queue for remote tracks (000078, track FK dropped in 000086)
    id TEXT PRIMARY KEY,
    track_id TEXT NOT NULL,                 -- no FK: deleting a track keeps its download history
    playlist_id TEXT REFERENCES playlists(id) ON DELETE SET NULL,  -- "add here when done"; not a reason to abort
    quality TEXT NOT NULL DEFAULT '320k',
    state TEXT NOT NULL DEFAULT 'queued',      -- queued | active | done | failed | canceled
    attempts INTEGER NOT NULL DEFAULT 0,       -- incremented by MarkFailed; retry UPDATEs this row
    max_attempts INTEGER NOT NULL DEFAULT 3,
    next_retry_at DATETIME,                    -- NULL for a never-retried job; set on a retryable failure
    last_error TEXT NOT NULL DEFAULT '',
    bytes_done INTEGER NOT NULL DEFAULT 0,
    bytes_total INTEGER NOT NULL DEFAULT 0,
    target_path TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at DATETIME,
    finished_at DATETIME
)
-- idx_download_jobs_active: UNIQUE(track_id) WHERE state IN ('queued','active')

lyrics (
    track_id TEXT PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    source TEXT,
    meta_content TEXT NOT NULL DEFAULT '',
    meta_source TEXT NOT NULL DEFAULT '',
    created_at DATETIME,
    updated_at DATETIME
)

watched_folders (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    created_at DATETIME
)
```

### Junction Tables (Many-to-Many)

```sql
track_artists        (track_id, artist_id, position) PK(track_id, artist_id)
track_album_artists  (track_id, artist_id, position) PK(track_id, artist_id)
track_genres         (track_id, genre_id,  position) PK(track_id, genre_id)
track_composers      (track_id, composer_id, position) PK(track_id, composer_id)
album_artists        (album_id, artist_id, position) PK(album_id, artist_id)
playlist_tracks      (playlist_id, track_id, position TEXT) PK(playlist_id, track_id)
```

All junction tables cascade delete when parent entity is deleted.

### State Tables (Single-Row)

```sql
player_state (
    id INTEGER PRIMARY KEY CHECK(id = 1),
    queue_track_ids TEXT DEFAULT '[]',   -- JSON array of track IDs
    current_track_id TEXT,
    position REAL DEFAULT 0,
    volume REAL DEFAULT 1.0,
    muted INTEGER DEFAULT 0,
    shuffle INTEGER DEFAULT 0,
    repeat_mode TEXT DEFAULT 'off',
    updated_at DATETIME
)

app_settings (
    id INTEGER PRIMARY KEY CHECK(id = 1),
    language TEXT DEFAULT 'en',
    theme TEXT DEFAULT 'system',
    lastfm_username TEXT,
    auto_check_update BOOLEAN DEFAULT 1,
    start_at_login BOOLEAN DEFAULT 0,
    eq_enabled BOOLEAN DEFAULT 0,
    lrclib_mode TEXT DEFAULT 'prefer_metadata',
    use_online_artist_artwork BOOLEAN NOT NULL DEFAULT 1,
    prefer_local_artist_artwork BOOLEAN NOT NULL DEFAULT 1, -- re-added (000028)
    last_scan_version TEXT NOT NULL DEFAULT '',
    enable_lrclib BOOLEAN NOT NULL DEFAULT 1,
    enable_kugou BOOLEAN NOT NULL DEFAULT 1,
    prefer_local_lyrics BOOLEAN NOT NULL DEFAULT 1, -- renamed from prefer_metadata_lyrics (000022)
    lyrics_folder_enabled BOOLEAN NOT NULL DEFAULT 0,
    lyrics_folder_path TEXT NOT NULL DEFAULT '',
    lyrics_subfolder_enabled BOOLEAN NOT NULL DEFAULT 0,
    lyrics_subfolder_name TEXT NOT NULL DEFAULT '',
    artist_delimiters TEXT NOT NULL DEFAULT '[";","\\\\",","]',        -- JSON array; empty [] = no splitting
    album_artist_delimiters TEXT NOT NULL DEFAULT '[";","\\\\",","]',  -- JSON array
    genre_delimiters TEXT NOT NULL DEFAULT '[";","\\\\",","]',         -- JSON array
    composer_delimiters TEXT NOT NULL DEFAULT '[";","\\\\",","]',      -- JSON array
    library_analysis_worker_count INTEGER NOT NULL DEFAULT 2,          -- desired analysis-pool worker count; runtime clamps to [1, numCPU/2] (000047)
    normalization_enabled INTEGER NOT NULL DEFAULT 0,                  -- volume normalization (000034)
    normalization_mode TEXT NOT NULL DEFAULT 'off',                    -- off | track | album
    normalization_target_lufs REAL NOT NULL DEFAULT -14,
    normalization_prevent_clip INTEGER NOT NULL DEFAULT 1,
    mood_derivation_version INTEGER NOT NULL DEFAULT 0,               -- bumped on corpus percentile recompute → stales every track's mood (000037)
    crossfade_seconds INTEGER NOT NULL DEFAULT 0,                     -- track-transition overlap in seconds; 0 = off/gapless (000048)
    blend_artwork_during_crossfade BOOLEAN NOT NULL DEFAULT 1,        -- fullscreen cover blend during automatic crossfade (000049)
    high_contrast_lyrics BOOLEAN NOT NULL DEFAULT 1,                  -- fullscreen lyrics glass panel; false is immersive (000050)
    show_player_favorite_button BOOLEAN NOT NULL DEFAULT 0,           -- favorite control visibility in desktop footer and mini player (000076)
    updated_at DATETIME
    -- (also: show_tray_icon, prevent_sleep_while_playing, remote_server_*, show_player_indicator, max_queue_size)
)

library_sync_state (
    id INTEGER PRIMARY KEY CHECK(id = 1),
    delimiters_signature TEXT NOT NULL DEFAULT '',  -- JSON of the 4 applied delimiter lists; compared on sync to decide re-split
    metadata_schema_version INTEGER NOT NULL DEFAULT 0,  -- current extractor field-set version; behind → next SyncFolder force-reparses all files once (000040)
    updated_at DATETIME
)

mini_player_state (
    id INTEGER PRIMARY KEY CHECK(id = 1),
    x INTEGER DEFAULT 0,
    y INTEGER DEFAULT 0,
    width INTEGER DEFAULT 300,
    height INTEGER DEFAULT 300,
    always_on_top INTEGER DEFAULT 0,
    has_position INTEGER DEFAULT 0,  -- 0 until first move/resize; geometry ignored while 0
    updated_at DATETIME
)
```

### Listening Insights Tables

```sql
listening_sessions (
    id TEXT PRIMARY KEY,
    source_device_id TEXT,
    track_id TEXT REFERENCES tracks(id) ON DELETE CASCADE,
    started_at DATETIME,
    ended_at DATETIME,
    listened_seconds INTEGER,
    qualified_play INTEGER DEFAULT 0
)

daily_track_listening_stats (
    source_device_id TEXT,
    local_date TEXT,
    track_id TEXT REFERENCES tracks(id) ON DELETE CASCADE,
    listened_seconds INTEGER DEFAULT 0,
    play_count INTEGER DEFAULT 0,
    PRIMARY KEY (source_device_id, local_date, track_id)
)

playback_attempts (
    id TEXT PRIMARY KEY,
    source_device_id TEXT,
    track_id TEXT REFERENCES tracks(id) ON DELETE CASCADE,
    started_at DATETIME,
    ended_at DATETIME,
    start_position_seconds REAL,
    listened_seconds INTEGER,
    end_reason TEXT -- completed, skipped, stopped; NULL while active
)

daily_playback_attempt_stats (
    source_device_id TEXT,
    local_date TEXT,
    attempts INTEGER,
    completed INTEGER,
    skipped INTEGER,
    stopped INTEGER,
    listened_seconds INTEGER,
    PRIMARY KEY (source_device_id, local_date)
)
```

Analytics queries sum across sources; `source_device_id` remains available for
a future device filter. Snapshot import uses immutable raw IDs and monotonic
per-origin daily counters, so retrying mobile reconciliation cannot duplicate
totals.

`ListeningRepository.RecordSession` writes both tables in one transaction. It
splits elapsed listening time across local calendar days and increments the
daily play count only on the session's ending day when the playback threshold
was reached. `listening_sessions` is retained for 180 days; aggregates remain
available for all-time insights. Playback attempts are separate from listening
sessions: one attempt spans pause/resume and seek, starts asynchronously after
playback starts, and is finalized by the same bounded background writer.
Completion Rate is `completed / (completed + skipped + stopped)`; Skip Rate
uses `skipped` as its numerator. Raw attempts are retained for 180 days while
daily aggregates remain for all-time rates; startup converts unclosed attempts
left by a crash into `stopped` attempts.

`ListeningRepository.GetListeningInsights` also returns `streak_days`: the current
consecutive local-calendar-day listening streak, beginning today when active or
yesterday otherwise; a second missed day resets it to zero. It is calculated
from all listening aggregates, independently of the selected insight range.
`ListeningRepository.GetLibraryInsights` returns `library_growth`, calculated
from the indexed `tracks.created_at` column. Bounded periods first count tracks
before the requested range and aggregate additions only within that range, then
build the cumulative daily series in memory. All-time insights aggregate once
per local calendar year. This avoids a full-library count for every chart point.

### EQ Tables

```sql
eq_profiles (
    id TEXT PRIMARY KEY,
    preset_key TEXT NOT NULL DEFAULT '', -- stable built-in key; empty for user profiles
    name TEXT NOT NULL,
    is_active INTEGER DEFAULT 0,
    is_default INTEGER DEFAULT 0,
    created_at DATETIME
)

eq_bands (
    profile_id TEXT REFERENCES eq_profiles(id) ON DELETE CASCADE,
    band_index INTEGER,
    frequency REAL NOT NULL,
    gain REAL DEFAULT 0.0,
    bandwidth REAL DEFAULT 1.0,
    PRIMARY KEY (profile_id, band_index)
)
```

### Indexes

```sql
idx_tracks_album_id             ON tracks(album_id)
idx_tracks_sort_title           ON tracks(sort_title)
idx_tracks_is_favorite          ON tracks(is_favorite)
idx_tracks_analyzed_version     ON tracks(analyzed_version)        -- pending-DSP scan (000034)
idx_tracks_mood_derived_version ON tracks(mood_derived_version)    -- stale-mood scan (000038)
idx_artists_normalization_key   ON artists(normalization_key)
idx_albums_normalization_key    ON albums(normalization_key)
idx_genres_normalization_key    ON genres(normalization_key)
idx_composers_normalization_key ON composers(normalization_key)

-- Junction reverse-lookup indexes (000035): second-column scans / orphan-cleanup anti-joins
idx_track_artists_artist_id        ON track_artists(artist_id)
idx_track_album_artists_artist_id  ON track_album_artists(artist_id)
idx_track_genres_genre_id          ON track_genres(genre_id)
idx_track_composers_composer_id    ON track_composers(composer_id)
idx_album_artists_artist_id        ON album_artists(artist_id)
idx_listening_sessions_started_at  ON listening_sessions(started_at)
idx_listening_sessions_ended_at    ON listening_sessions(ended_at)  -- retention cleanup
idx_daily_track_listening_stats_date ON daily_track_listening_stats(local_date)
```

## Repository Patterns

### TrackRepository — Key Queries

**`GetAll`** and **`GetPaginated`**: Complex SELECT with `LEFT JOIN album_artists`, `LEFT JOIN track_artists`, `GROUP_CONCAT` for aggregating related artists into a comma-separated string, then parsed back into structs.

**`GetByIDs`**: Builds dynamic `IN (?, ?, ...)` placeholder, then re-orders results to match input ID order (SQLite doesn't guarantee order for IN queries).

**`GetByArtistID`**: Joins through `track_artists` junction table.

**`AlbumArtistIDsByPathPrefix`**: `SELECT DISTINCT taa.artist_id FROM tracks t JOIN track_album_artists taa … WHERE t.path LIKE ? || '%'` — distinct album-artist IDs of tracks under a folder. Used by local artist-image mapping (`artist.jpg`); avoids loading full track DTOs (whose `AlbumArtists` `GetByPathPrefix` doesn't populate).

**`GetMostListened`**: `ORDER BY play_count DESC LIMIT ?`

**`GetRecentlyPlayed`**: `ORDER BY updated_at DESC LIMIT ?` (updated when play count increments)

**`GetRecentlyAdded`**: `ORDER BY created_at DESC LIMIT ?` (track import time)

**`Upsert`**: `INSERT OR REPLACE INTO tracks ...` using sqlx named parameters.

**Junction `Set*` methods**: Wrapped in a transaction — DELETE existing junction rows, then INSERT new ones with positional ordering.

### AlbumRepository — Key Queries

**`GetByArtistID`**: UNION of three conditions:

1. Albums where artist is in `album_artists`
2. Albums where artist is in `track_artists` of any track in the album
3. Albums where artist is in `track_album_artists` of any track in the album

**`DeleteOrphaned`**: `DELETE FROM albums WHERE id NOT IN (SELECT DISTINCT album_id FROM tracks WHERE album_id IS NOT NULL)`

### PlaylistRepository — Track Ordering (LexoRank)

**`AddTrack`**: INSERT with LexoRank position string.

**`RemoveTrack`**: DELETE the row. No position reindexing needed — LexoRank strings are independent.

**`UpdateTrackPosition`**: UPDATE single track's LexoRank position.

**`UpdateTracksPositions`**: Batch UPDATE positions in a transaction (used for rebalancing).

**`GetTracks`**: `JOIN tracks ON ... ORDER BY pt.position, pt.track_id`

## ID Generation

All entity IDs are deterministic UUID v4-style strings derived from MD5 hash of a seed string:

- Track: MD5(file path)
- Artist: MD5(normalization_key)
- Album: MD5(normalization_key + primary_artist_normalization_key)
- Genre/Composer: MD5(normalization_key)
- Playlist: random UUID v4

This ensures the same file always gets the same track ID, and the same artist name always maps to the same artist entity, enabling safe upserts without collision.
