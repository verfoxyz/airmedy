package catalog

import (
	"sort"
	"strings"

	"airmedy/internal/domain"
)

// artistMatchBonus is added when the query also appears in an artist name.
// Title similarity is the main signal; an artist hit lifts a row whose title is
// otherwise unrelated (a search by "周杰伦" should find "晴天").
const artistMatchBonus = 0.3

// candidate is one source row before multi-source grouping.
type candidate struct {
	source string
	order  int // position of source in the selection order
	rank   int // position of the row inside that source
	row    domain.PluginSearchResultRow
	score  float64
}

// rankMultiSource merges several sources into one ordered list.
//
// Rules (docs/aggregate-search-tx-kg-plan.md §4):
//   - each row is scored by normalized edit distance between the query and its
//     title, plus a bonus when the query hits an artist name;
//   - same title + same artists across sources is one row, labelled with every
//     source that returned it; the highest-scoring copy provides its identity;
//   - ties keep the selection order of the sources, then the source's own order;
//   - the merged list is not truncated: paging decides how much the UI shows.
func rankMultiSource(
	query string,
	sourceOrder []string,
	results map[string]domain.PluginSearchResult,
) []domain.CatalogSearchResult {
	orderIndex := make(map[string]int, len(sourceOrder))
	for index, source := range sourceOrder {
		orderIndex[source] = index
	}

	type group struct {
		best    candidate
		sources []string
		seen    map[string]struct{}
	}
	groups := make(map[string]*group)
	keys := make([]string, 0)

	for _, source := range sourceOrder {
		result, ok := results[source]
		if !ok {
			continue
		}
		seenIdentities := make(map[string]struct{})
		for rank, row := range result.Rows {
			if strings.TrimSpace(row.ExternalID) == "" {
				// A row without an id cannot be downloaded or identified.
				continue
			}
			identity := source + "\x00" + strings.TrimSpace(row.ExternalID)
			if _, duplicate := seenIdentities[identity]; duplicate {
				continue
			}
			seenIdentities[identity] = struct{}{}

			entry := candidate{
				source: source,
				order:  orderIndex[source],
				rank:   rank,
				row:    row,
				score:  rowScore(query, row),
			}
			key := dedupeKey(row)
			current, exists := groups[key]
			if !exists {
				groups[key] = &group{
					best:    entry,
					sources: []string{source},
					seen:    map[string]struct{}{source: {}},
				}
				keys = append(keys, key)
				continue
			}
			if _, known := current.seen[source]; !known {
				current.seen[source] = struct{}{}
				current.sources = append(current.sources, source)
			}
			// Strict comparison keeps the first source on a tie, which is the
			// documented "tie by source order" rule.
			if entry.score > current.best.score {
				current.best = entry
			}
		}
	}

	merged := make([]domain.CatalogSearchResult, 0, len(keys))
	for _, key := range keys {
		entry := groups[key]
		sources := append([]string(nil), entry.sources...)
		sort.SliceStable(sources, func(i, j int) bool {
			return orderIndex[sources[i]] < orderIndex[sources[j]]
		})
		merged = append(merged, domain.CatalogSearchResult{
			Provider:      entry.best.source,
			ExternalID:    entry.best.row.ExternalID,
			Title:         strings.TrimSpace(entry.best.row.Title),
			Artists:       trimAll(entry.best.row.Artists),
			AlbumName:     strings.TrimSpace(entry.best.row.AlbumName),
			Duration:      entry.best.row.Duration,
			Sources:       sources,
			CoverURL:      strings.TrimSpace(entry.best.row.CoverURL),
			CoverURLLarge: strings.TrimSpace(entry.best.row.CoverURLLarge),
			Rank:          entry.best.score,
		})
	}

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Rank != merged[j].Rank {
			return merged[i].Rank > merged[j].Rank
		}
		left, right := orderIndex[merged[i].Provider], orderIndex[merged[j].Provider]
		if left != right {
			return left < right
		}
		return merged[i].Title < merged[j].Title
	})

	return merged
}

// rankSingleSource keeps the source's own order. A user who selected exactly one
// source expects that platform's ranking, so the host only removes unusable and
// duplicate identities.
func rankSingleSource(source string, result domain.PluginSearchResult) []domain.CatalogSearchResult {
	rows := make([]domain.CatalogSearchResult, 0, len(result.Rows))
	seen := make(map[string]struct{}, len(result.Rows))
	for _, row := range result.Rows {
		externalID := strings.TrimSpace(row.ExternalID)
		if externalID == "" {
			continue
		}
		if _, duplicate := seen[externalID]; duplicate {
			continue
		}
		seen[externalID] = struct{}{}
		rows = append(rows, domain.CatalogSearchResult{
			Provider:      source,
			ExternalID:    externalID,
			Title:         strings.TrimSpace(row.Title),
			Artists:       trimAll(row.Artists),
			AlbumName:     strings.TrimSpace(row.AlbumName),
			Duration:      row.Duration,
			Sources:       []string{source},
			CoverURL:      strings.TrimSpace(row.CoverURL),
			CoverURLLarge: strings.TrimSpace(row.CoverURLLarge),
		})
	}
	return rows
}

// fuseRanks is the untruncated multi-source merge, kept for the ranking tests.
func fuseRanks(query string, sourceOrder []string, results map[string]domain.PluginSearchResult) []domain.CatalogSearchResult {
	return rankMultiSource(query, sourceOrder, results)
}

// rowScore combines title similarity with the artist bonus.
func rowScore(query string, row domain.PluginSearchResultRow) float64 {
	score := titleSimilarity(query, row.Title)
	for _, artist := range row.Artists {
		if containsFold(artist, query) {
			score += artistMatchBonus
			break
		}
	}
	return score
}

// dedupeKey identifies "the same song" across sources: normalized title plus the
// normalized artist set. Album and duration are deliberately not part of the key:
// the same track is listed under different albums on different platforms.
func dedupeKey(row domain.PluginSearchResultRow) string {
	artists := make([]string, 0, len(row.Artists))
	for _, artist := range row.Artists {
		if value := normalizeSpace(artist); value != "" {
			artists = append(artists, value)
		}
	}
	sort.Strings(artists)
	return normalizeSpace(row.Title) + "\x00" + strings.Join(artists, "\x01")
}

// titleSimilarity is 1 - editDistance / longer length, after case and whitespace
// normalization. Rune-based so CJK titles are measured by character, not byte.
func titleSimilarity(query, title string) float64 {
	left := []rune(normalizeSpace(query))
	right := []rune(normalizeSpace(title))
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	length := len(left)
	if len(right) > length {
		length = len(right)
	}
	return 1 - float64(editDistance(left, right))/float64(length)
}

// editDistance is the Levenshtein distance with unit insertion/deletion/substitution.
func editDistance(left, right []rune) int {
	previous := make([]int, len(right)+1)
	for j := 0; j <= len(right); j++ {
		previous[j] = j
	}
	for i := 1; i <= len(left); i++ {
		current := make([]int, len(right)+1)
		current[0] = i
		for j := 1; j <= len(right); j++ {
			cost := 1
			if left[i-1] == right[j-1] {
				cost = 0
			}
			current[j] = min3(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(right)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// normalizeSpace lowercases and collapses runs of whitespace.
func normalizeSpace(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

// containsFold reports whether haystack contains needle, case-insensitively.
func containsFold(haystack, needle string) bool {
	needle = normalizeSpace(needle)
	if needle == "" {
		return false
	}
	return strings.Contains(normalizeSpace(haystack), needle)
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

// pageOf slices the merged list for one request.
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
