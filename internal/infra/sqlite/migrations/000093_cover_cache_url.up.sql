-- Keep the remote URL a cover was downloaded from.
--
-- A row keyed only by url_hash can serve the cached file, but it cannot bring
-- the image back after the file is gone, and promotion into the library needs
-- the URL to embed artwork. Existing rows keep an empty URL: the next hit for
-- the same hash fills it in, and rows that predate this migration fall back to
-- the artwork key materialised on the track.
ALTER TABLE cover_cache ADD COLUMN url TEXT NOT NULL DEFAULT '';
