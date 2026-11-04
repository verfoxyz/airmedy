DROP INDEX IF EXISTS idx_tracks_provider_external;
DROP INDEX IF EXISTS idx_tracks_availability;

ALTER TABLE tracks DROP COLUMN external_id;
ALTER TABLE tracks DROP COLUMN provider;
ALTER TABLE tracks DROP COLUMN availability;
