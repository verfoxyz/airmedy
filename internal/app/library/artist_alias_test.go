package library

import (
	"context"
	"testing"

	"airmedy/internal/domain"
)

type stubAliasRepo struct {
	aliases   []domain.ArtistAlias
	added     [][3]string
	err       error
	listCalls int
}

func (s *stubAliasRepo) ListArtistAliases(context.Context) ([]domain.ArtistAlias, error) {
	s.listCalls++
	return s.aliases, nil
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

func enabledPair(alias, canonical string) domain.ArtistAlias {
	return domain.ArtistAlias{AliasKey: alias, CanonicalKey: canonical, Enabled: true}
}

func TestExpandSearchQueries_BidirectionalAndWholeWord(t *testing.T) {
	repo := &stubAliasRepo{aliases: []domain.ArtistAlias{enabledPair("camellia", "かめりあ")}}
	svc := NewArtistAliasService(repo)
	ctx := context.Background()

	got, err := svc.ExpandSearchQueries(ctx, "camellia")
	if err != nil {
		t.Fatalf("ExpandSearchQueries: %v", err)
	}
	if len(got) != 2 || got[0] != "camellia" || got[1] != "かめりあ" {
		t.Fatalf("camellia expanded to %v", got)
	}

	// The stored direction is one-way, but expansion is bidirectional.
	got, err = svc.ExpandSearchQueries(ctx, "かめりあ")
	if err != nil {
		t.Fatalf("ExpandSearchQueries: %v", err)
	}
	if len(got) != 2 || got[0] != "かめりあ" || got[1] != "camellia" {
		t.Fatalf("かめりあ expanded to %v", got)
	}

	// A word that merely contains the alias is not replaced.
	got, err = svc.ExpandSearchQueries(ctx, "camellias")
	if err != nil {
		t.Fatalf("ExpandSearchQueries: %v", err)
	}
	if len(got) != 1 || got[0] != "camellias" {
		t.Fatalf("camellias expanded to %v, want no variant", got)
	}
}

func TestExpandSearchQueries_ReplacesOneWordOfAMultiWordQuery(t *testing.T) {
	repo := &stubAliasRepo{aliases: []domain.ArtistAlias{enabledPair("camellia", "かめりあ")}}
	svc := NewArtistAliasService(repo)

	got, err := svc.ExpandSearchQueries(context.Background(), "camellia remix")
	if err != nil {
		t.Fatalf("ExpandSearchQueries: %v", err)
	}
	if len(got) != 2 || got[0] != "camellia remix" || got[1] != "かめりあ remix" {
		t.Fatalf("expanded to %v", got)
	}
}

func TestExpandSearchQueries_CapsVariants(t *testing.T) {
	aliases := make([]domain.ArtistAlias, 0, 6)
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		aliases = append(aliases, enabledPair(name, name+"-alias"))
	}
	svc := NewArtistAliasService(&stubAliasRepo{aliases: aliases})

	got, err := svc.ExpandSearchQueries(context.Background(), "a b c d e f")
	if err != nil {
		t.Fatalf("ExpandSearchQueries: %v", err)
	}
	// The original plus at most four variants.
	if len(got) != 1+maxSearchAliasVariants {
		t.Fatalf("expanded to %d queries, want %d", len(got), 1+maxSearchAliasVariants)
	}
	if got[0] != "a b c d e f" {
		t.Fatalf("the original query must stay first, got %v", got)
	}
}

func TestExpandSearchQueries_IgnoresDisabledAliases(t *testing.T) {
	disabled := enabledPair("camellia", "かめりあ")
	disabled.Enabled = false
	svc := NewArtistAliasService(&stubAliasRepo{aliases: []domain.ArtistAlias{disabled}})

	got, err := svc.ExpandSearchQueries(context.Background(), "camellia")
	if err != nil {
		t.Fatalf("ExpandSearchQueries: %v", err)
	}
	if len(got) != 1 || got[0] != "camellia" {
		t.Fatalf("disabled alias still expanded to %v", got)
	}
}

func TestExpandSearchQueries_CacheAndInvalidation(t *testing.T) {
	repo := &stubAliasRepo{aliases: []domain.ArtistAlias{enabledPair("camellia", "かめりあ")}}
	svc := NewArtistAliasService(repo)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := svc.ExpandSearchQueries(ctx, "camellia"); err != nil {
			t.Fatalf("ExpandSearchQueries: %v", err)
		}
	}
	if repo.listCalls != 1 {
		t.Fatalf("alias list read %d times, want one cached read", repo.listCalls)
	}

	// Adding a pair must take effect immediately, not after the TTL.
	if err := svc.Add(ctx, "gumi", "megpoid"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := svc.ExpandSearchQueries(ctx, "camellia"); err != nil {
		t.Fatalf("ExpandSearchQueries: %v", err)
	}
	if repo.listCalls != 2 {
		t.Fatalf("cache was not invalidated by Add (reads = %d)", repo.listCalls)
	}
}
