-- Translations are stored separately from the original lyric. The host no
-- longer guesses which half of a concatenated line is the original, so a
-- provider's `tlyric` keeps its own column and its own display switch.
ALTER TABLE lyrics ADD COLUMN translation_content TEXT NOT NULL DEFAULT '';
ALTER TABLE lyrics ADD COLUMN translation_source TEXT NOT NULL DEFAULT '';

-- The mobile snapshot is cached by content, so the translation must be part of
-- the snapshot or a cache hit would silently drop it.
ALTER TABLE mobile_sync_lyric_cache ADD COLUMN translation_content TEXT NOT NULL DEFAULT '';
ALTER TABLE mobile_sync_lyric_cache ADD COLUMN translation_source TEXT NOT NULL DEFAULT '';

-- none | translation | romanization. Fresh installs start at `translation` so a
-- translated lyric is shown instead of arbitrarily enabling romanization.
ALTER TABLE app_settings ADD COLUMN lyrics_secondary_mode TEXT NOT NULL DEFAULT 'translation';
-- Existing users keep the behaviour they had: the boolean romanization switch
-- maps onto the matching enum member.
UPDATE app_settings
SET lyrics_secondary_mode = CASE WHEN romanization_enabled THEN 'romanization' ELSE 'none' END;
