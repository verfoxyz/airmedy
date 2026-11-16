package library

import (
	"context"
	"strings"
	"sync"
	"time"

	"airmedy/internal/domain"
)

// Alias expansion bounds. The table is tiny, but a search must not fan out into
// an unbounded number of Bleve queries; every extra query costs a full index
// scan. The alias list is cached for one search burst so Add/Remove/SetEnabled
// invalidate it explicitly.
const (
	searchAliasCacheTTL    = 30 * time.Second
	maxSearchAliasVariants = 4
	// artistKeySeparator is what ArtistSetKey joins multi-artist keys with. Such
	// keys cannot be substituted into a search box as a single word, so pairs
	// containing it are skipped for expansion.
	artistKeySeparator = "\x1f"
)

// ArtistAliasService owns the manual alias rules: normalize the two names, reject
// empty/self/duplicate pairs, and leave reading to the repository.
type ArtistAliasService struct {
	repo domain.ArtistAliasRepository

	mu        sync.Mutex
	index     map[string][]string
	fetchedAt time.Time
	now       func() time.Time
}

// NewArtistAliasService wires the settings-facing alias editor.
func NewArtistAliasService(repo domain.ArtistAliasRepository) *ArtistAliasService {
	return &ArtistAliasService{repo: repo, now: time.Now}
}

// List returns every pair, enabled or not, newest first.
func (s *ArtistAliasService) List(ctx context.Context) ([]domain.ArtistAlias, error) {
	return s.repo.ListArtistAliases(ctx)
}

// Add stores a manual pair. Both names are normalized with ArtistSetKey, so
// "Camellia" and "camellia" are the same key.
func (s *ArtistAliasService) Add(ctx context.Context, alias, canonical string) error {
	aliasKey, canonicalKey, err := normalizeAliasPair(alias, canonical)
	if err != nil {
		return err
	}
	if err := s.repo.AddArtistAlias(ctx, aliasKey, canonicalKey, domain.ArtistAliasEvidenceManual); err != nil {
		return err
	}
	s.invalidateSearchIndex()
	return nil
}

// Remove deletes both directions of the pair.
func (s *ArtistAliasService) Remove(ctx context.Context, alias, canonical string) error {
	aliasKey, canonicalKey, err := normalizeAliasPair(alias, canonical)
	if err != nil {
		return err
	}
	if err := s.repo.RemoveArtistAlias(ctx, aliasKey, canonicalKey); err != nil {
		return err
	}
	s.invalidateSearchIndex()
	return nil
}

// SetEnabled keeps the row but takes it out of matching when false.
func (s *ArtistAliasService) SetEnabled(ctx context.Context, alias, canonical string, enabled bool) error {
	aliasKey, canonicalKey, err := normalizeAliasPair(alias, canonical)
	if err != nil {
		return err
	}
	if err := s.repo.SetArtistAliasEnabled(ctx, aliasKey, canonicalKey, enabled); err != nil {
		return err
	}
	s.invalidateSearchIndex()
	return nil
}

// ExpandSearchQueries returns the original query followed by variants that
// replace a known artist alias with its counterpart.
//
// Only whole words are replaced, so "camellia" inside another word is never
// touched. A query without a matching alias returns just itself, and the caller
// therefore runs exactly one Bleve query.
func (s *ArtistAliasService) ExpandSearchQueries(ctx context.Context, query string) ([]string, error) {
	original := []string{query}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" || s.repo == nil {
		return original, nil
	}
	index := s.aliasIndex(ctx)
	if len(index) == 0 {
		return original, nil
	}

	variants := make([]string, 0, maxSearchAliasVariants)
	seen := map[string]struct{}{query: {}}
	add := func(variant string) {
		variant = strings.TrimSpace(variant)
		if variant == "" || len(variants) >= maxSearchAliasVariants {
			return
		}
		if _, exists := seen[variant]; exists {
			return
		}
		seen[variant] = struct{}{}
		variants = append(variants, variant)
	}

	// The whole query may be the artist name ("sad keyboard guy").
	for _, counterpart := range index[domain.NormalizationKey(trimmed)] {
		add(counterpart)
	}

	// Or one word of it ("camellia remix" → "かめりあ remix").
	words := strings.Fields(trimmed)
	if len(words) > 1 {
		for position, word := range words {
			for _, counterpart := range index[domain.NormalizationKey(word)] {
				replaced := append([]string{}, words...)
				replaced[position] = counterpart
				add(strings.Join(replaced, " "))
				if len(variants) >= maxSearchAliasVariants {
					break
				}
			}
			if len(variants) >= maxSearchAliasVariants {
				break
			}
		}
	}

	return append(original, variants...), nil
}

// aliasIndex returns normalized name → enabled counterparts, cached for one
// search burst. A read failure yields an empty index: search keeps working
// without expansion instead of failing.
func (s *ArtistAliasService) aliasIndex(ctx context.Context) map[string][]string {
	now := s.clock()
	s.mu.Lock()
	if s.index != nil && now.Sub(s.fetchedAt) < searchAliasCacheTTL {
		index := s.index
		s.mu.Unlock()
		return index
	}
	s.mu.Unlock()

	aliases, err := s.repo.ListArtistAliases(ctx)
	if err != nil {
		return nil
	}
	index := make(map[string][]string, len(aliases))
	for _, pair := range aliases {
		if !pair.Enabled {
			continue
		}
		aliasKey := strings.TrimSpace(pair.AliasKey)
		canonicalKey := strings.TrimSpace(pair.CanonicalKey)
		if aliasKey == "" || canonicalKey == "" || aliasKey == canonicalKey {
			continue
		}
		if strings.Contains(aliasKey, artistKeySeparator) || strings.Contains(canonicalKey, artistKeySeparator) {
			continue
		}
		index[aliasKey] = append(index[aliasKey], canonicalKey)
		index[canonicalKey] = append(index[canonicalKey], aliasKey)
	}

	s.mu.Lock()
	s.index = index
	s.fetchedAt = now
	s.mu.Unlock()
	return index
}

func (s *ArtistAliasService) invalidateSearchIndex() {
	s.mu.Lock()
	s.index = nil
	s.fetchedAt = time.Time{}
	s.mu.Unlock()
}

func (s *ArtistAliasService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func normalizeAliasPair(alias, canonical string) (string, string, error) {
	aliasKey := domain.ArtistSetKey([]string{strings.TrimSpace(alias)})
	canonicalKey := domain.ArtistSetKey([]string{strings.TrimSpace(canonical)})
	if aliasKey == "" || canonicalKey == "" {
		return "", "", domain.ErrArtistAliasInvalid
	}
	if aliasKey == canonicalKey {
		return "", "", domain.ErrArtistAliasSelf
	}
	return aliasKey, canonicalKey, nil
}
