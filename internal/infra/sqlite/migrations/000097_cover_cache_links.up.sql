-- Many-to-many cover identity links.
--
-- One image is shared by every track of an album, but a cover_cache row can
-- only remember a single provider/external_id. The extra columns therefore made
-- every other track of the same album miss the cached image (no cover for a
-- playable streamed row). Links keep cover_cache as the URL -> file index and
-- give each identity its own association.
--
-- Rows from before this migration are backfilled from the single-value columns;
-- later Get calls insert a link for every identity that asks for the image.
CREATE TABLE cover_cache_links (
    url_hash    TEXT NOT NULL,
    provider    TEXT NOT NULL,
    external_id TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (url_hash, provider, external_id)
);

CREATE INDEX idx_cover_cache_links_identity ON cover_cache_links(provider, external_id);
CREATE INDEX idx_cover_cache_links_hash ON cover_cache_links(url_hash);

INSERT OR IGNORE INTO cover_cache_links (url_hash, provider, external_id, created_at)
SELECT url_hash, provider, external_id, last_used
FROM cover_cache
WHERE provider <> '' AND external_id <> '';
