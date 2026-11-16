package library

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"airmedy/internal/domain"
)

// stubSearcher returns canned rows per query and records the queries it saw.
// The embedded port keeps the test from implementing the write side.
type stubSearcher struct {
	domain.SearchService
	results map[string][]domain.SearchResult
	errs    map[string]error
	calls   []string
}

func (s *stubSearcher) Search(_ context.Context, query string) ([]domain.SearchResult, error) {
	s.calls = append(s.calls, query)
	if err := s.errs[query]; err != nil {
		return nil, err
	}
	return s.results[query], nil
}

func hit(kind, id string) domain.SearchResult {
	return domain.SearchResult{ID: id, Type: kind}
}

func TestSearch_MergesAliasVariantsWithDeduplication(t *testing.T) {
	searcher := &stubSearcher{results: map[string][]domain.SearchResult{
		"camellia": {hit("track", "1"), hit("album", "9")},
		"かめりあ":     {hit("track", "1"), hit("track", "2")},
	}}
	aliases := NewArtistAliasService(&stubAliasRepo{
		aliases: []domain.ArtistAlias{enabledPair("camellia", "かめりあ")},
	})
	svc := NewSearchService(searcher, aliases)

	got, err := svc.Search(context.Background(), "camellia")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(searcher.calls) != 2 || searcher.calls[0] != "camellia" || searcher.calls[1] != "かめりあ" {
		t.Fatalf("queries = %v, want the original then the alias", searcher.calls)
	}
	want := []string{"track:1", "album:9", "track:2"}
	if len(got) != len(want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
	for index, key := range want {
		if got[index].Type+":"+got[index].ID != key {
			t.Fatalf("result %d = %s:%s, want %s", index, got[index].Type, got[index].ID, key)
		}
	}
}

func TestSearch_WithoutAnAliasRunsOneQuery(t *testing.T) {
	searcher := &stubSearcher{results: map[string][]domain.SearchResult{
		"unrelated": {hit("track", "1")},
	}}
	svc := NewSearchService(searcher, NewArtistAliasService(&stubAliasRepo{}))

	got, err := svc.Search(context.Background(), "unrelated")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(searcher.calls) != 1 {
		t.Fatalf("queries = %v, want exactly one", searcher.calls)
	}
	if len(got) != 1 {
		t.Fatalf("results = %v, want the original hit", got)
	}
}

func TestSearch_DisabledAliasRunsOneQuery(t *testing.T) {
	disabled := enabledPair("camellia", "かめりあ")
	disabled.Enabled = false
	searcher := &stubSearcher{results: map[string][]domain.SearchResult{
		"camellia": {hit("track", "1")},
	}}
	svc := NewSearchService(searcher, NewArtistAliasService(&stubAliasRepo{
		aliases: []domain.ArtistAlias{disabled},
	}))

	if _, err := svc.Search(context.Background(), "camellia"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(searcher.calls) != 1 {
		t.Fatalf("queries = %v, want exactly one", searcher.calls)
	}
}

func TestSearch_CapsTheMergedResults(t *testing.T) {
	original := make([]domain.SearchResult, 0, searchResultLimit)
	other := make([]domain.SearchResult, 0, searchResultLimit)
	for index := 0; index < searchResultLimit; index++ {
		original = append(original, hit("track", fmt.Sprintf("a-%d", index)))
		other = append(other, hit("track", fmt.Sprintf("b-%d", index)))
	}
	searcher := &stubSearcher{results: map[string][]domain.SearchResult{
		"camellia": original,
		"かめりあ":     other,
	}}
	svc := NewSearchService(searcher, NewArtistAliasService(&stubAliasRepo{
		aliases: []domain.ArtistAlias{enabledPair("camellia", "かめりあ")},
	}))

	got, err := svc.Search(context.Background(), "camellia")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != searchResultLimit {
		t.Fatalf("merged %d results, want the cap of %d", len(got), searchResultLimit)
	}
}

func TestSearch_VariantFailureKeepsTheOriginalResults(t *testing.T) {
	searcher := &stubSearcher{
		results: map[string][]domain.SearchResult{"camellia": {hit("track", "1")}},
		errs:    map[string]error{"かめりあ": errors.New("variant exploded")},
	}
	svc := NewSearchService(searcher, NewArtistAliasService(&stubAliasRepo{
		aliases: []domain.ArtistAlias{enabledPair("camellia", "かめりあ")},
	}))

	got, err := svc.Search(context.Background(), "camellia")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("results = %v, want the original hit", got)
	}
}

func TestSearch_OriginalFailureIsReturned(t *testing.T) {
	searcher := &stubSearcher{errs: map[string]error{"camellia": errors.New("index unavailable")}}
	svc := NewSearchService(searcher, NewArtistAliasService(&stubAliasRepo{
		aliases: []domain.ArtistAlias{enabledPair("camellia", "かめりあ")},
	}))

	if _, err := svc.Search(context.Background(), "camellia"); err == nil {
		t.Fatal("expected the original query failure to surface")
	}
}

func TestSearch_BlankQueryRunsNothing(t *testing.T) {
	searcher := &stubSearcher{}
	svc := NewSearchService(searcher, NewArtistAliasService(&stubAliasRepo{}))

	got, err := svc.Search(context.Background(), "   ")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 || len(searcher.calls) != 0 {
		t.Fatalf("blank query ran %v and returned %v", searcher.calls, got)
	}
}
