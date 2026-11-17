package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"airmedy/internal/domain"

	"github.com/jmoiron/sqlx"
)

type lastfmRecommendationRepository struct {
	db *DB
}

// NewLastFmRecommendationRepository returns the persisted Last.fm lists.
func NewLastFmRecommendationRepository(db *DB) domain.LastFmRecommendationRepository {
	return &lastfmRecommendationRepository{db: db}
}

type storedRecommendationList struct {
	Kind      string    `db:"kind"`
	Title     string    `db:"title"`
	UpdatedAt time.Time `db:"updated_at"`
	WeekFrom  int64     `db:"week_from"`
	WeekTo    int64     `db:"week_to"`
}

type storedRecommendationItem struct {
	domain.RecommendationItem
	ArtistsJSON string `db:"artists"`
	SourcesJSON string `db:"sources"`
}

const recommendationItemColumns = `kind, position, match_key, title, artist, artists, album,
	duration, cover_url, cover_url_large, downloadable, blocked_reason, sources,
	matching, matched, no_source, provider, external_id, state, local_track_id, suspicious,
	play_count, week_from, week_to`

// ListRecommendationLists returns the stored cards. Counts and covers are
// derived from the rows, so a stale summary cannot drift from the detail page.
func (r *lastfmRecommendationRepository) ListRecommendationLists(
	ctx context.Context,
) ([]domain.RecommendationListSummary, error) {
	lists := make([]storedRecommendationList, 0, 2)
	if err := r.db.SelectContext(ctx, &lists,
		`SELECT kind, title, updated_at, week_from, week_to FROM lastfm_recommendation_lists
		 ORDER BY updated_at DESC, kind ASC`); err != nil {
		return nil, fmt.Errorf("lastfm recommendation lists: list: %w", err)
	}

	items := make([]domain.RecommendationItem, 0)
	if err := r.db.SelectContext(ctx, &items,
		`SELECT kind, position, duration, cover_url, matching
		 FROM lastfm_recommendation_items
		 ORDER BY kind ASC, position ASC`); err != nil {
		return nil, fmt.Errorf("lastfm recommendation lists: summaries: %w", err)
	}

	summaries := make([]domain.RecommendationListSummary, 0, len(lists))
	index := make(map[string]*domain.RecommendationListSummary, len(lists))
	for _, list := range lists {
		summary := domain.RecommendationListSummary{
			Kind:      list.Kind,
			Title:     list.Title,
			CoverURLs: []string{},
			WeekFrom:  list.WeekFrom,
			WeekTo:    list.WeekTo,
			UpdatedAt: list.UpdatedAt,
		}
		summaries = append(summaries, summary)
		index[list.Kind] = &summaries[len(summaries)-1]
	}

	for _, item := range items {
		summary, ok := index[item.Kind]
		if !ok {
			continue
		}
		summary.TrackCount++
		summary.TotalDuration += item.Duration
		if item.Matching {
			summary.MatchingCount++
		}
		if cover := item.CoverURL; cover != "" && len(summary.CoverURLs) < 4 {
			duplicate := false
			for _, existing := range summary.CoverURLs {
				if existing == cover {
					duplicate = true
					break
				}
			}
			if !duplicate {
				summary.CoverURLs = append(summary.CoverURLs, cover)
			}
		}
	}
	return summaries, nil
}

// GetRecommendationList returns one list, or (nil, nil) when it was never built.
func (r *lastfmRecommendationRepository) GetRecommendationList(
	ctx context.Context, kind string,
) (*domain.RecommendationList, error) {
	var stored storedRecommendationList
	err := r.db.GetContext(ctx, &stored,
		`SELECT kind, title, updated_at, week_from, week_to
		 FROM lastfm_recommendation_lists WHERE kind = ?`, kind)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lastfm recommendation list %s: %w", kind, err)
	}

	rows := make([]storedRecommendationItem, 0)
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+recommendationItemColumns+` FROM lastfm_recommendation_items
		 WHERE kind = ? ORDER BY position ASC`, kind); err != nil {
		return nil, fmt.Errorf("lastfm recommendation list %s: items: %w", kind, err)
	}

	list := &domain.RecommendationList{
		Kind:      stored.Kind,
		Title:     stored.Title,
		Items:     make([]domain.RecommendationItem, 0, len(rows)),
		UpdatedAt: stored.UpdatedAt,
		WeekFrom:  stored.WeekFrom,
		WeekTo:    stored.WeekTo,
	}
	for _, row := range rows {
		item := row.RecommendationItem
		item.Artists = decodeStrings(row.ArtistsJSON)
		item.Sources = decodeStrings(row.SourcesJSON)
		list.Items = append(list.Items, item)
	}
	return list, nil
}

// SaveRecommendationList replaces every row of one kind in a single
// transaction. A refresh either lands completely or keeps the old list.
func (r *lastfmRecommendationRepository) SaveRecommendationList(
	ctx context.Context, list *domain.RecommendationList,
) error {
	if list == nil || list.Kind == "" {
		return fmt.Errorf("lastfm recommendation list: empty kind")
	}
	updatedAt := list.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}

	return r.db.RunTx(ctx, func(ctx context.Context) error {
		exec := r.db.Ext(ctx)
		// payload belongs to the candidate pool (see SaveRecommendationCandidates);
		// rewriting the rows must not clear it, so the conflict path leaves it alone.
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO lastfm_recommendation_lists (kind, title, updated_at, week_from, week_to)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(kind) DO UPDATE SET
			   title = excluded.title,
			   updated_at = excluded.updated_at,
			   week_from = excluded.week_from,
			   week_to = excluded.week_to`,
			list.Kind, list.Title, updatedAt.UTC(), list.WeekFrom, list.WeekTo); err != nil {
			return fmt.Errorf("lastfm recommendation list %s: save header: %w", list.Kind, err)
		}
		if _, err := exec.ExecContext(ctx,
			`DELETE FROM lastfm_recommendation_items WHERE kind = ?`, list.Kind); err != nil {
			return fmt.Errorf("lastfm recommendation list %s: clear rows: %w", list.Kind, err)
		}
		for position, item := range list.Items {
			artists, err := json.Marshal(nonNilStrings(item.Artists))
			if err != nil {
				return fmt.Errorf("lastfm recommendation list %s: encode artists: %w", list.Kind, err)
			}
			sources, err := json.Marshal(nonNilStrings(item.Sources))
			if err != nil {
				return fmt.Errorf("lastfm recommendation list %s: encode sources: %w", list.Kind, err)
			}
			if _, err := exec.ExecContext(ctx,
				`INSERT INTO lastfm_recommendation_items
				 (kind, position, match_key, title, artist, artists, album, duration, cover_url,
				  cover_url_large, downloadable, blocked_reason, sources, matching,
				  matched, no_source, provider, external_id, state, local_track_id, suspicious,
				  play_count, week_from, week_to)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				list.Kind, position, item.MatchKey, item.Title, item.Artist, string(artists),
				item.Album, item.Duration, item.CoverURL, item.CoverURLLarge, item.Downloadable,
				item.BlockedReason, string(sources), item.Matching, item.Matched,
				item.NoSource, item.Provider, item.ExternalID, item.State, item.LocalTrackID,
				item.Suspicious, item.PlayCount, item.WeekFrom, item.WeekTo,
			); err != nil {
				return fmt.Errorf("lastfm recommendation list %s: insert %s: %w",
					list.Kind, item.MatchKey, err)
			}
		}
		return nil
	})
}

// UpdateRecommendationItems writes match progress back to existing rows.
func (r *lastfmRecommendationRepository) UpdateRecommendationItems(
	ctx context.Context, items []domain.RecommendationItem,
) error {
	if len(items) == 0 {
		return nil
	}
	return r.db.RunTx(ctx, func(ctx context.Context) error {
		exec := r.db.Ext(ctx)
		for _, item := range items {
			if item.Kind == "" || item.MatchKey == "" {
				continue
			}
			artists, err := json.Marshal(nonNilStrings(item.Artists))
			if err != nil {
				return fmt.Errorf("lastfm recommendation item %s: encode artists: %w", item.MatchKey, err)
			}
			sources, err := json.Marshal(nonNilStrings(item.Sources))
			if err != nil {
				return fmt.Errorf("lastfm recommendation item %s: encode sources: %w", item.MatchKey, err)
			}
			if _, err := exec.ExecContext(ctx,
				`UPDATE lastfm_recommendation_items
				 SET title = ?, artist = ?, artists = ?, album = ?, duration = ?, cover_url = ?,
				     cover_url_large = ?, downloadable = ?, blocked_reason = ?, sources = ?,
				     matching = ?, matched = ?, no_source = ?, provider = ?,
				     external_id = ?, state = ?, local_track_id = ?, suspicious = ?,
				     play_count = ?, week_from = ?, week_to = ?
				 WHERE kind = ? AND match_key = ?`,
				item.Title, item.Artist, string(artists), item.Album, item.Duration, item.CoverURL,
				item.CoverURLLarge, item.Downloadable, item.BlockedReason, string(sources),
				item.Matching, item.Matched, item.NoSource, item.Provider,
				item.ExternalID, item.State, item.LocalTrackID, item.Suspicious,
				item.PlayCount, item.WeekFrom, item.WeekTo,
				item.Kind, item.MatchKey,
			); err != nil {
				return fmt.Errorf("lastfm recommendation item %s/%s: update: %w",
					item.Kind, item.MatchKey, err)
			}
		}
		return nil
	})
}

// PendingRecommendationItems returns every row still marked matching, in list
// order, so a restart resumes exactly the rows that were left waiting.
func (r *lastfmRecommendationRepository) PendingRecommendationItems(
	ctx context.Context,
) ([]domain.RecommendationItem, error) {
	rows := make([]storedRecommendationItem, 0)
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+recommendationItemColumns+` FROM lastfm_recommendation_items
		 WHERE matching = 1 ORDER BY kind ASC, position ASC`); err != nil {
		return nil, fmt.Errorf("lastfm recommendation items: pending: %w", err)
	}
	items := make([]domain.RecommendationItem, 0, len(rows))
	for _, row := range rows {
		item := row.RecommendationItem
		item.Artists = decodeStrings(row.ArtistsJSON)
		item.Sources = decodeStrings(row.SourcesJSON)
		items = append(items, item)
	}
	return items, nil
}

// ResetRecommendationMatches clears the match state of the given identities so
// the rows re-enter the matching queue. A library import uses this to replace a
// cached remote match with the local copy it now owns.
func (r *lastfmRecommendationRepository) ResetRecommendationMatches(
	ctx context.Context, matchKeys []string,
) (int, error) {
	matchKeys = uniqueMatchKeys(matchKeys)
	if len(matchKeys) == 0 {
		return 0, nil
	}
	query, args, err := sqlx.In(
		`UPDATE lastfm_recommendation_items
		 SET matching = 1, matched = 0, no_source = 0, provider = '',
		     external_id = '', state = '', local_track_id = '', blocked_reason = '',
		     sources = '[]', downloadable = 0, suspicious = 0
		 WHERE match_key IN (?)`, matchKeys)
	if err != nil {
		return 0, fmt.Errorf("lastfm recommendation items: reset matches: %w", err)
	}
	result, err := r.db.Ext(ctx).ExecContext(ctx, r.db.Rebind(query), args...)
	if err != nil {
		return 0, fmt.Errorf("lastfm recommendation items: reset matches: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(affected), nil
}

// SaveRecommendationCandidates stores the unexpanded candidate pool in the
// list's payload column. Saving the list's rows deliberately leaves payload
// alone, so appending a page does not drop the rest of the pool.
func (r *lastfmRecommendationRepository) SaveRecommendationCandidates(
	ctx context.Context, kind string, items []domain.RecommendationItem,
) error {
	if kind == "" {
		return fmt.Errorf("lastfm recommendation candidates: empty kind")
	}
	payload, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("lastfm recommendation candidates %s: encode: %w", kind, err)
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE lastfm_recommendation_lists SET payload = ? WHERE kind = ?`,
		string(payload), kind); err != nil {
		return fmt.Errorf("lastfm recommendation candidates %s: save: %w", kind, err)
	}
	return nil
}

// GetRecommendationCandidates returns the stored candidate pool, newest first.
func (r *lastfmRecommendationRepository) GetRecommendationCandidates(
	ctx context.Context, kind string,
) ([]domain.RecommendationItem, error) {
	var payload string
	err := r.db.GetContext(ctx, &payload,
		`SELECT payload FROM lastfm_recommendation_lists WHERE kind = ?`, kind)
	if err == sql.ErrNoRows {
		return []domain.RecommendationItem{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lastfm recommendation candidates %s: %w", kind, err)
	}
	if payload == "" {
		return []domain.RecommendationItem{}, nil
	}
	var items []domain.RecommendationItem
	if err := json.Unmarshal([]byte(payload), &items); err != nil {
		return []domain.RecommendationItem{}, nil
	}
	if items == nil {
		return []domain.RecommendationItem{}, nil
	}
	return items, nil
}

func decodeStrings(raw string) []string {
	if raw == "" {
		return []string{}
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return []string{}
	}
	return nonNilStrings(values)
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
