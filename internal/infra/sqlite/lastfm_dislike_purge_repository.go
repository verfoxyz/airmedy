package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"airmedy/internal/domain"

	"github.com/jmoiron/sqlx"
)

type lastfmDislikePurgeRepository struct {
	db *DB
}

// NewLastFmDislikePurgeRepository returns the transactional purge of a disliked
// identity's recommendation traces.
func NewLastFmDislikePurgeRepository(db *DB) domain.LastFmDislikeTracePurger {
	return &lastfmDislikePurgeRepository{db: db}
}

// PurgeDislikeTraces removes the identities from every stored list, every
// candidate pool and the shared match cache in one transaction.
func (r *lastfmDislikePurgeRepository) PurgeDislikeTraces(
	ctx context.Context, matchKeys []string,
) error {
	keys := uniqueMatchKeys(matchKeys)
	if len(keys) == 0 {
		return nil
	}
	query, args, err := sqlx.In(`DELETE FROM lastfm_recommendation_items WHERE match_key IN (?)`, keys)
	if err != nil {
		return fmt.Errorf("lastfm dislike purge: items: %w", err)
	}
	cacheQuery, cacheArgs, err := sqlx.In(`DELETE FROM lastfm_match_cache WHERE match_key IN (?)`, keys)
	if err != nil {
		return fmt.Errorf("lastfm dislike purge: match cache: %w", err)
	}

	return r.db.RunTx(ctx, func(ctx context.Context) error {
		exec := r.db.Ext(ctx)
		if _, err := exec.ExecContext(ctx, r.db.Rebind(query), args...); err != nil {
			return fmt.Errorf("lastfm dislike purge: items: %w", err)
		}
		if _, err := exec.ExecContext(ctx, r.db.Rebind(cacheQuery), cacheArgs...); err != nil {
			return fmt.Errorf("lastfm dislike purge: match cache: %w", err)
		}

		var lists []struct {
			Kind    string `db:"kind"`
			Payload string `db:"payload"`
		}
		if err := exec.SelectContext(ctx, &lists,
			`SELECT kind, payload FROM lastfm_recommendation_lists`); err != nil {
			return fmt.Errorf("lastfm dislike purge: candidate pools: %w", err)
		}
		blocked := make(map[string]struct{}, len(keys))
		for _, key := range keys {
			blocked[key] = struct{}{}
		}
		for _, list := range lists {
			var pool []domain.RecommendationItem
			if err := json.Unmarshal([]byte(list.Payload), &pool); err != nil {
				// An older or empty payload has no candidate pool to clean.
				continue
			}
			kept := make([]domain.RecommendationItem, 0, len(pool))
			for _, item := range pool {
				if _, skip := blocked[item.MatchKey]; skip {
					continue
				}
				kept = append(kept, item)
			}
			if len(kept) == len(pool) {
				continue
			}
			encoded, err := json.Marshal(kept)
			if err != nil {
				return fmt.Errorf("lastfm dislike purge: encode candidates: %w", err)
			}
			if _, err := exec.ExecContext(ctx,
				`UPDATE lastfm_recommendation_lists SET payload = ? WHERE kind = ?`,
				string(encoded), list.Kind,
			); err != nil {
				return fmt.Errorf("lastfm dislike purge: save candidates: %w", err)
			}
		}
		return nil
	})
}
