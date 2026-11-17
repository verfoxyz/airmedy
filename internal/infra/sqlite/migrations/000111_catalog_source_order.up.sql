-- User-preferred order of the searchable catalogue sources, stored as a JSON
-- array of normalized provider ids on the single settings row.
--
-- The order only decides tie-breaks between equally relevant rows, the source
-- fan-out order and the source chips; it never changes a source's own ranking.
-- An empty array means "alphabetical", which is the historical behaviour.
ALTER TABLE app_settings ADD COLUMN catalog_source_order TEXT NOT NULL DEFAULT '[]';
