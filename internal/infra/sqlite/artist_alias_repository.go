package sqlite

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"airmedy/internal/domain"
)

type artistAliasRepository struct {
	db *DB
}

// NewArtistAliasRepository returns the artist alias store used by every matcher.
func NewArtistAliasRepository(db *DB) domain.ArtistAliasRepository {
	return &artistAliasRepository{db: db}
}

const artistAliasColumns = `alias_key, canonical_key, evidence, enabled, created_at`

// ListArtistAliases returns every pair, newest first. Disabled rows are included:
// settings has to show them so the user can re-enable them.
func (r *artistAliasRepository) ListArtistAliases(ctx context.Context) ([]domain.ArtistAlias, error) {
	rows := make([]domain.ArtistAlias, 0)
	err := r.db.SelectContext(ctx, &rows,
		`SELECT `+artistAliasColumns+` FROM artist_aliases
		 ORDER BY created_at DESC, alias_key ASC`)
	if err != nil {
		return nil, fmt.Errorf("artist aliases: list: %w", err)
	}
	return rows, nil
}

// AddArtistAlias inserts one direction. A pair that already exists in either
// direction is rejected; the caller words the error.
func (r *artistAliasRepository) AddArtistAlias(
	ctx context.Context, aliasKey, canonicalKey, evidence string,
) error {
	if strings.TrimSpace(aliasKey) == "" || strings.TrimSpace(canonicalKey) == "" {
		return domain.ErrArtistAliasInvalid
	}
	var existing int
	err := r.db.GetContext(ctx, &existing,
		`SELECT COUNT(*) FROM artist_aliases
		 WHERE (alias_key = ? AND canonical_key = ?) OR (alias_key = ? AND canonical_key = ?)`,
		aliasKey, canonicalKey, canonicalKey, aliasKey)
	if err != nil {
		return fmt.Errorf("artist aliases: check %s/%s: %w", aliasKey, canonicalKey, err)
	}
	if existing > 0 {
		return domain.ErrArtistAliasExists
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO artist_aliases (alias_key, canonical_key, evidence, enabled)
		 VALUES (?, ?, ?, 1)
		 ON CONFLICT(alias_key, canonical_key) DO NOTHING`,
		aliasKey, canonicalKey, evidence)
	if err != nil {
		return fmt.Errorf("artist aliases: add %s/%s: %w", aliasKey, canonicalKey, err)
	}
	return nil
}

// RemoveArtistAlias deletes both directions: the pair is unordered.
func (r *artistAliasRepository) RemoveArtistAlias(ctx context.Context, aliasKey, canonicalKey string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM artist_aliases
		 WHERE (alias_key = ? AND canonical_key = ?) OR (alias_key = ? AND canonical_key = ?)`,
		aliasKey, canonicalKey, canonicalKey, aliasKey)
	if err != nil {
		return fmt.Errorf("artist aliases: remove %s/%s: %w", aliasKey, canonicalKey, err)
	}
	return nil
}

// SetArtistAliasEnabled flips both directions of the pair.
func (r *artistAliasRepository) SetArtistAliasEnabled(
	ctx context.Context, aliasKey, canonicalKey string, enabled bool,
) error {
	value := 0
	if enabled {
		value = 1
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE artist_aliases SET enabled = ?
		 WHERE (alias_key = ? AND canonical_key = ?) OR (alias_key = ? AND canonical_key = ?)`,
		value, aliasKey, canonicalKey, canonicalKey, aliasKey)
	if err != nil {
		return fmt.Errorf("artist aliases: set enabled %s/%s: %w", aliasKey, canonicalKey, err)
	}
	return nil
}

// ExpandArtists returns every input key plus its enabled one-hop aliases. A pair
// learned as A→B therefore also expands B to A at read time.
func (r *artistAliasRepository) ExpandArtists(
	ctx context.Context, keys []string,
) ([]string, error) {
	unique := make([]string, 0, len(keys))
	input := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, duplicate := input[key]; duplicate {
			continue
		}
		input[key] = struct{}{}
		unique = append(unique, key)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]any, 0, len(unique)*2)
	for _, key := range unique {
		args = append(args, key)
	}
	for _, key := range unique {
		args = append(args, key)
	}
	query := fmt.Sprintf(
		`SELECT alias_key, canonical_key FROM artist_aliases
		 WHERE enabled = 1 AND (alias_key IN (%s) OR canonical_key IN (%s))`,
		placeholders, placeholders)

	pairs := make([]domain.ArtistAlias, 0)
	if err := r.db.SelectContext(ctx, &pairs, query, args...); err != nil {
		return nil, fmt.Errorf("artist aliases: expand: %w", err)
	}

	// Only one hop: membership is tested against the original input, so a chain
	// A→B→C never expands A to C.
	out := make(map[string]struct{}, len(unique)+len(pairs))
	for key := range input {
		out[key] = struct{}{}
	}
	for _, pair := range pairs {
		if _, ok := input[pair.AliasKey]; ok {
			out[pair.CanonicalKey] = struct{}{}
		}
		if _, ok := input[pair.CanonicalKey]; ok {
			out[pair.AliasKey] = struct{}{}
		}
	}

	expanded := make([]string, 0, len(out))
	for key := range out {
		expanded = append(expanded, key)
	}
	sort.Strings(expanded)
	return expanded, nil
}
