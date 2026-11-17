-- Local recommendation exclusions. Last.fm has no public "dislike"
-- endpoint, so an exclusion is stored here only: it hides a song or an artist
-- from discovery and radio, and never touches the library or the account.
--
-- match_key uses the same normalisation as the match cache: the normalised
-- artist+title pair for a track, ArtistSetKey for an artist. The unique pair
-- makes a repeated dislike idempotent.
CREATE TABLE dislikes (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    match_key  TEXT NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    artist     TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    UNIQUE (kind, match_key)
);

CREATE INDEX idx_dislikes_kind ON dislikes(kind, created_at DESC);
