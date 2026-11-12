-- Persisted Last.fm recommendation lists.
--
-- The Last.fm page renders two lists as cards. Their content is stored so
-- reopening the page reads the database instead of spending Last.fm requests;
-- only an explicit refresh replaces a list.
CREATE TABLE lastfm_recommendation_lists (
    kind       TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '[]',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- One row per track, in Last.fm order. Matching updates a row in place, so a
-- long run does not rewrite the whole list. `matching = 1` is the worker queue
-- after a restart.
CREATE TABLE lastfm_recommendation_items (
    kind            TEXT NOT NULL,
    position        INTEGER NOT NULL,
    match_key       TEXT NOT NULL,
    title           TEXT NOT NULL,
    artist          TEXT NOT NULL,
    artists         TEXT NOT NULL DEFAULT '[]',
    album           TEXT NOT NULL DEFAULT '',
    duration        INTEGER NOT NULL DEFAULT 0,
    cover_url       TEXT NOT NULL DEFAULT '',
    cover_url_large TEXT NOT NULL DEFAULT '',
    downloadable    INTEGER NOT NULL DEFAULT 0,
    blocked_reason  TEXT NOT NULL DEFAULT '',
    sources         TEXT NOT NULL DEFAULT '[]',
    matching        INTEGER NOT NULL DEFAULT 1,
    matched         INTEGER NOT NULL DEFAULT 0,
    no_source       INTEGER NOT NULL DEFAULT 0,
    provider        TEXT NOT NULL DEFAULT '',
    external_id     TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT '',
    local_track_id  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (kind, match_key)
);

CREATE INDEX idx_lastfm_recommendation_matching
    ON lastfm_recommendation_items(matching);
