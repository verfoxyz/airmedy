-- The weekly and discovery lists do not exist without these columns, so the
-- rows they own are dropped first instead of silently losing their window.
DELETE FROM lastfm_recommendation_items
WHERE kind = 'discover' OR kind LIKE 'weekly_echo:%';
DELETE FROM lastfm_recommendation_lists
WHERE kind = 'discover' OR kind LIKE 'weekly_echo:%';

ALTER TABLE lastfm_recommendation_lists DROP COLUMN week_to;
ALTER TABLE lastfm_recommendation_lists DROP COLUMN week_from;
ALTER TABLE lastfm_recommendation_items DROP COLUMN week_to;
ALTER TABLE lastfm_recommendation_items DROP COLUMN week_from;
ALTER TABLE lastfm_recommendation_items DROP COLUMN play_count;
