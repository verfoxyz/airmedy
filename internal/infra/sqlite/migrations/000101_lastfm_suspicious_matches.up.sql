-- A local match is accepted even when the duration disagrees, but the row is
-- flagged so the UI asks the user instead of auto-saving/downloading.
ALTER TABLE lastfm_recommendation_items
    ADD COLUMN suspicious INTEGER NOT NULL DEFAULT 0;
