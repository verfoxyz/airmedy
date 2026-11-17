-- Sources the user switched off, as a JSON array of normalized provider ids on
-- the single settings row.
--
-- A negative list: a newly installed source is enabled by default. A disabled
-- source keeps its position in catalog_source_order, so re-enabling it returns
-- to where the user had arranged it. Disabling only affects new searches; a
-- queued download or a playing stream is untouched.
ALTER TABLE app_settings ADD COLUMN catalog_disabled_sources TEXT NOT NULL DEFAULT '[]';
