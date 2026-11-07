-- Remote catalogue cover cache (see internal/infra/covercache).
--
-- Files live beside this index; a row makes a file addressable. `path` is the
-- file name only, so the cache directory can move without rewriting rows.
--
-- `last_used` is the LRU key: a hit updates it (one UPDATE), and a write that
-- pushes the total over the limit deletes the smallest timestamps first.
-- `bytes` is recorded at write time instead of stat-ing every file, because
-- eviction needs a SUM it can compute in one query.
CREATE TABLE cover_cache (
    url_hash   TEXT PRIMARY KEY,
    path       TEXT NOT NULL,
    bytes      INTEGER NOT NULL,
    mime       TEXT NOT NULL,
    last_used  INTEGER NOT NULL
);

CREATE INDEX idx_cover_cache_last_used ON cover_cache(last_used);
