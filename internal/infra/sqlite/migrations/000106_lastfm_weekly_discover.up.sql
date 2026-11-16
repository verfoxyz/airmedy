-- Weekly charts and discovery lists extend the stored recommendation rows.
--
-- play_count is Last.fm's playcount inside a weekly chart; week_from/week_to
-- name the chart window so a stored week can render its range without another
-- request. Existing rows keep the defaults, so the two older lists are
-- unaffected.
ALTER TABLE lastfm_recommendation_items ADD COLUMN play_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lastfm_recommendation_items ADD COLUMN week_from INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lastfm_recommendation_items ADD COLUMN week_to INTEGER NOT NULL DEFAULT 0;

-- The list header carries the window too, so an empty week still knows which
-- range it stands for.
ALTER TABLE lastfm_recommendation_lists ADD COLUMN week_from INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lastfm_recommendation_lists ADD COLUMN week_to INTEGER NOT NULL DEFAULT 0;
