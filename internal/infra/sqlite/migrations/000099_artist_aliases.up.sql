CREATE TABLE artist_aliases (
    alias_key     TEXT NOT NULL,
    canonical_key TEXT NOT NULL,
    evidence      TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (alias_key, canonical_key)
);

CREATE INDEX idx_artist_aliases_canonical ON artist_aliases(canonical_key);
