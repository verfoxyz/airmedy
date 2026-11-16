package library

import (
	"context"
	"log/slog"
	"strings"

	"airmedy/internal/domain"
)

// searchResultLimit mirrors the per-query cap the Bleve adapter applies. Merged
// alias variants must not push the page past what one query would return.
const searchResultLimit = 200

// SearchService is the local search entry point. It expands the query through
// enabled artist aliases and merges the per-variant hits, so the Wails binding
// stays a translation layer and every local search path gets the behaviour.
type SearchService struct {
	search  domain.SearchService
	aliases *ArtistAliasService
}

// NewSearchService wires local search to the alias expander.
func NewSearchService(search domain.SearchService, aliases *ArtistAliasService) *SearchService {
	return &SearchService{search: search, aliases: aliases}
}

// Search runs the original query first and then any alias variant. Original
// hits keep their rank; a duplicate (type + id) from a variant is dropped, and
// the merged list is capped at searchResultLimit.
//
// A variant failure is logged and skipped: the original results are still a
// complete answer, so one extra index query failing must not fail the search.
func (s *SearchService) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	if s.search == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	queries := []string{query}
	if s.aliases != nil {
		expanded, err := s.aliases.ExpandSearchQueries(ctx, query)
		if err != nil {
			slog.Warn("search: alias expansion failed", "query", query, "error", err)
		} else if len(expanded) > 0 {
			queries = expanded
		}
	}
	if len(queries) == 1 {
		return s.search.Search(ctx, queries[0])
	}

	merged := make([]domain.SearchResult, 0, searchResultLimit)
	seen := make(map[string]struct{}, searchResultLimit)
	for index, candidate := range queries {
		rows, err := s.search.Search(ctx, candidate)
		if err != nil {
			if index == 0 {
				return nil, err
			}
			slog.Warn("search: alias variant failed", "query", candidate, "error", err)
			continue
		}
		for _, row := range rows {
			if len(merged) >= searchResultLimit {
				return merged, nil
			}
			key := strings.ToLower(row.Type) + ":" + row.ID
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, row)
		}
	}
	return merged, nil
}
