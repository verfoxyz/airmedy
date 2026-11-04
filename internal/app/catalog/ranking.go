package catalog

import (
	"sort"
	"strings"

	"airmedy/internal/domain"
)

// rankFusionConstant is the +3 in Σ 1/(rank + 3).
//
// It is the standard reciprocal-rank-fusion offset: it stops the top hit from
// dominating every other source and gives ranks 5+ a long, comparable tail. The
// exact value is not important; that it is *fixed* is. Platform "relevance
// scores" are not comparable (some are 0..1, some 0..100, some absent), so the
// host ranks by position, never by the source's own number.
const rankFusionConstant = 3.0

// titleHitBonus is added to a row whose title or artist contains the query.
//
// It is applied after fusion and is larger than any single rank contribution
// (1/3 at rank 0), so an exact name match beats a merely well-ranked row from a
// source that matched something else. It stays below 1 so it cannot outrank a
// row that fused in from several sources at the top.
const titleHitBonus = 0.9

// sourceRow is one row from one source, with the rank the source assigned it.
type sourceRow struct {
	source string
	rank   int
	row    domain.PluginSearchResultRow
}

// fuseRanks merges per-source results into one ordered, de-duplicated list.
//
// Rules (catalog/plugins/README.md, "catalog.search"):
//   - identity is (source, external id): the same track from two different
//     sources is NOT merged. Each row keeps its own source name, because the
//     download that follows depends on which source produced it.
//   - a source returning the same id twice is a bug, not a merge case; the best
//     (lowest) rank wins and the duplicate is dropped.
//   - ties are broken by source order, then by the row's position in that
//     source. That makes the ordering deterministic: the same answers always
//     produce the same page.
func fuseRanks(query string, sourceOrder []string, results map[string]domain.PluginSearchResult) []domain.CatalogSearchResult {
	orderIndex := make(map[string]int, len(sourceOrder))
	for index, source := range sourceOrder {
		orderIndex[source] = index
	}

	rows := make([]sourceRow, 0)
	seen := make(map[string]struct{})
	for _, source := range sourceOrder {
		result, ok := results[source]
		if !ok {
			continue
		}
		for rank, row := range result.Rows {
			if strings.TrimSpace(row.ExternalID) == "" {
				// A row without an id cannot be downloaded or identified, so it
				// is not a result: dropping it is better than rendering an
				// action that can never work.
				continue
			}
			key := source + "\x00" + row.ExternalID
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			rows = append(rows, sourceRow{source: source, rank: rank, row: row})
		}
	}

	fused := make([]domain.CatalogSearchResult, 0, len(rows))
	for _, entry := range rows {
		rank := 1.0 / (float64(entry.rank) + rankFusionConstant)
		if matchesQuery(query, entry.row) {
			rank += titleHitBonus
		}
		artists := trimAll(entry.row.Artists)
		fused = append(fused, domain.CatalogSearchResult{
			Provider:      entry.source,
			ExternalID:    entry.row.ExternalID,
			Title:         strings.TrimSpace(entry.row.Title),
			Artists:       artists,
			AlbumName:     strings.TrimSpace(entry.row.AlbumName),
			Duration:      entry.row.Duration,
			CoverURL:      strings.TrimSpace(entry.row.CoverURL),
			CoverURLLarge: strings.TrimSpace(entry.row.CoverURLLarge),
			Rank:          rank,
		})
	}

	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].Rank != fused[j].Rank {
			return fused[i].Rank > fused[j].Rank
		}
		// Same score: keep the declared source order, then the source's own
		// rank. Without this, a map iteration would make the page order drift
		// between two identical searches.
		left, right := orderIndex[fused[i].Provider], orderIndex[fused[j].Provider]
		if left != right {
			return left < right
		}
		return fused[i].Title < fused[j].Title
	})
	return fused
}

// matchesQuery reports whether the query hits the row's title or any artist.
//
// The comparison is case-insensitive substring matching after trimming: a
// search for "晴天" must match a row titled "晴天 (Live)", and a search for
// "jay" must match the artist "Jay Chou". It deliberately does not tokenise or
// stem, because the platform already decided relevance and this is only the
// small tie-break on top of it.
func matchesQuery(query string, row domain.PluginSearchResultRow) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return false
	}
	if strings.Contains(strings.ToLower(row.Title), needle) {
		return true
	}
	for _, artist := range row.Artists {
		if strings.Contains(strings.ToLower(artist), needle) {
			return true
		}
	}
	return false
}

// trimAll trims each artist and drops the blanks, keeping the source's order.
func trimAll(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

// pageOf slices the fused list for one request.
//
// HasMore is derived from the slice, not from "the page was full": a source
// that returned exactly Limit rows and has nothing behind them must not make
// the UI offer a "load more" that produces an empty page.
func pageOf(fused []domain.CatalogSearchResult, offset, limit int) ([]domain.CatalogSearchResult, bool) {
	if offset >= len(fused) {
		return nil, false
	}
	end := offset + limit
	if end > len(fused) {
		end = len(fused)
	}
	return fused[offset:end], end < len(fused)
}
