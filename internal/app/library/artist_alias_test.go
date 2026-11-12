package library

import (
	"context"
	"testing"

	"airmedy/internal/domain"
)

type stubAliasRepo struct {
	added [][3]string
	err   error
}

func (s *stubAliasRepo) ListArtistAliases(context.Context) ([]domain.ArtistAlias, error) {
	return nil, nil
}

func (s *stubAliasRepo) AddArtistAlias(_ context.Context, aliasKey, canonicalKey, evidence string) error {
	if s.err != nil {
		return s.err
	}
	s.added = append(s.added, [3]string{aliasKey, canonicalKey, evidence})
	return nil
}

func (s *stubAliasRepo) RemoveArtistAlias(context.Context, string, string) error { return nil }
func (s *stubAliasRepo) SetArtistAliasEnabled(context.Context, string, string, bool) error {
	return nil
}

func (s *stubAliasRepo) ExpandArtists(_ context.Context, keys []string) ([]string, error) {
	return keys, nil
}

func TestArtistAliasService_RejectsEmptyAndSelfPairs(t *testing.T) {
	repo := &stubAliasRepo{}
	svc := NewArtistAliasService(repo)
	ctx := context.Background()

	if err := svc.Add(ctx, " ", "かめりあ"); err != domain.ErrArtistAliasInvalid {
		t.Fatalf("empty alias error = %v, want ErrArtistAliasInvalid", err)
	}
	if err := svc.Add(ctx, "Camellia", " camellia "); err != domain.ErrArtistAliasSelf {
		t.Fatalf("self alias error = %v, want ErrArtistAliasSelf", err)
	}
	if len(repo.added) != 0 {
		t.Fatalf("invalid pairs reached the repository: %v", repo.added)
	}
}

func TestArtistAliasService_NormalizesAndMarksManual(t *testing.T) {
	repo := &stubAliasRepo{}
	svc := NewArtistAliasService(repo)

	if err := svc.Add(context.Background(), " Camellia ", "かめりあ"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(repo.added) != 1 {
		t.Fatalf("added = %v, want one pair", repo.added)
	}
	pair := repo.added[0]
	if pair[0] != "camellia" || pair[1] != "かめりあ" || pair[2] != domain.ArtistAliasEvidenceManual {
		t.Fatalf("pair = %v, want normalized keys and manual evidence", pair)
	}
}
