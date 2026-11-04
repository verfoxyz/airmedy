-- Remote (not-yet-downloaded) tracks live in `tracks` with a virtual path
-- produced by domain.RemotePath ("remote://<provider>/<external_id>").
--
-- `path` stays NOT NULL UNIQUE: the virtual path is derived deterministically
-- from (provider, external_id), so the UNIQUE constraint gives per-source
-- deduplication for free without rebuilding the table (SQLite cannot ALTER
-- COLUMN, and `tracks` has been extended by six later migrations).
--
-- Invariants (see catalog/database/README.md):
--   provider = ''        -> local file, availability is always 'ready'
--   availability != 'ready' -> provider <> '' AND path = RemotePath(...)
ALTER TABLE tracks ADD COLUMN availability TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE tracks ADD COLUMN provider     TEXT NOT NULL DEFAULT '';
ALTER TABLE tracks ADD COLUMN external_id  TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_tracks_availability ON tracks(availability);

-- Uniqueness only applies to rows that actually carry an external id.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tracks_provider_external
    ON tracks (provider, external_id) WHERE external_id <> '';
