package library

import (
	"context"
	"strings"

	"airmedy/internal/domain"
)

// ArtistAliasService owns the manual alias rules: normalize the two names, reject
// empty/self/duplicate pairs, and leave reading to the repository.
type ArtistAliasService struct {
	repo domain.ArtistAliasRepository
}

// NewArtistAliasService wires the settings-facing alias editor.
func NewArtistAliasService(repo domain.ArtistAliasRepository) *ArtistAliasService {
	return &ArtistAliasService{repo: repo}
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
	return s.repo.AddArtistAlias(ctx, aliasKey, canonicalKey, domain.ArtistAliasEvidenceManual)
}

// Remove deletes both directions of the pair.
func (s *ArtistAliasService) Remove(ctx context.Context, alias, canonical string) error {
	aliasKey, canonicalKey, err := normalizeAliasPair(alias, canonical)
	if err != nil {
		return err
	}
	return s.repo.RemoveArtistAlias(ctx, aliasKey, canonicalKey)
}

// SetEnabled keeps the row but takes it out of matching when false.
func (s *ArtistAliasService) SetEnabled(ctx context.Context, alias, canonical string, enabled bool) error {
	aliasKey, canonicalKey, err := normalizeAliasPair(alias, canonical)
	if err != nil {
		return err
	}
	return s.repo.SetArtistAliasEnabled(ctx, aliasKey, canonicalKey, enabled)
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
