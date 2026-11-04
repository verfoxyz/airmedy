-- Download content options (see domain.AppSettings).
--
-- These decide what the download pipeline additionally fetches and writes
-- alongside the audio file. They are separate from the audio settings because
-- each one is an independent network request that the user pays for.
--
-- download_with_lyrics: fetch lyrics from the enabled providers (LRCLIB /
--   KuGou) and store them for the new track. Off by default: a download should
--   not silently spend a second request per track unless asked.
-- download_with_lyric_translation: keep the translation track the source
--   returned alongside the original (lx custom sources expose `tlyric`).
--   Meaningful only together with download_with_lyrics: asking for a
--   translation of lyrics that were never fetched is a contradiction, so the
--   pipeline treats it as an implied "no translation" rather than a second
--   feature.
-- download_with_cover: embed the cover image into the audio file's tags.
--   On by default: it is what makes a downloaded file look right in the
--   library, and the pipeline already tolerates a missing cover.
ALTER TABLE app_settings ADD COLUMN download_with_lyrics BOOLEAN NOT NULL DEFAULT 0;
ALTER TABLE app_settings ADD COLUMN download_with_lyric_translation BOOLEAN NOT NULL DEFAULT 0;
ALTER TABLE app_settings ADD COLUMN download_with_cover BOOLEAN NOT NULL DEFAULT 1;
