-- Imported lx custom sources (see domain.LxSource).
--
-- The lx plugin runs these scripts; the core owns the list because the plugin
-- is a child process with no persistence of its own and the user manages the
-- list from the settings UI. The files themselves live under
-- <DataDir>/plugins/lx/sources/<uuid>.js, so moving or deleting the file the
-- user picked does not break playback.
--
-- position is the try order: the plugin walks enabled rows from lowest to
-- highest and falls through to the next source when one cannot resolve a
-- track. It is assigned by the reorder operation, which rewrites it as 0..n-1
-- in one transaction, so a read never sees two rows at the same priority.
CREATE TABLE lx_sources (
    id          TEXT PRIMARY KEY,        -- uuid
    name        TEXT NOT NULL,           -- label in the list, defaults to the file name
    file_path   TEXT NOT NULL,           -- absolute path under the app data directory
    enabled     INTEGER NOT NULL DEFAULT 1,
    position    INTEGER NOT NULL DEFAULT 0,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_lx_sources_position ON lx_sources(position);
