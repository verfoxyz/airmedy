package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var (
	articlesRegexp = regexp.MustCompile(`^(?i)(the|a|an)\s+`)
	numberRegexp   = regexp.MustCompile(`\d+`)

	// artistCreditDelimiter splits a credit string on the separators sources and
	// tags use for multi-artist values.
	artistCreditDelimiter = regexp.MustCompile(`[;,\x{3001}/&]`)
	// artistCreditWordDelimiter splits the featured-credit wording ("feat.",
	// "ft.", "with") that only acts as a separator between artists.
	artistCreditWordDelimiter = regexp.MustCompile(`(?i)\s+(?:feat\.?|ft\.?|with)\s+`)
)

// NormalizeSort creates a string suitable for alphabetical sorting.
func NormalizeSort(s string) string {
	if s == "" {
		return ""
	}

	// 1. Article Stripping
	res := articlesRegexp.ReplaceAllString(s, "")

	// 2. Unicode Folding
	res = FoldUnicode(res)

	// 3. Sanitization: Remove leading punctuation and symbols only
	res = strings.TrimLeftFunc(res, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r)
	})

	// 4. Numeric Padding
	res = numberRegexp.ReplaceAllStringFunc(res, func(n string) string {
		return fmt.Sprintf("%04s", n)
	})

	return strings.TrimSpace(res)
}

// NormalizationKey creates a key for deduplication.
// It follows these rules:
// 1. Convert to lowercase.
// 2. Trim extra spaces.
// 3. Remove Vietnamese diacritics and other accents.
func NormalizationKey(s string) string {
	if s == "" {
		return ""
	}

	res := strings.ToLower(s)
	res = strings.Join(strings.Fields(res), " ") // Trim and collapse spaces
	res = FoldUnicode(res)

	return res
}

// FoldUnicode removes accents and diacritics from a string.
func FoldUnicode(s string) string {
	// NFKD normalization breaks characters into base + combining marks
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	res, _, _ := transform.String(t, s)

	// Special handling for Vietnamese characters that don't fold well with Mn removal
	// e.g., 'đ' -> 'd'
	res = strings.ReplaceAll(res, "đ", "d")
	res = strings.ReplaceAll(res, "Đ", "D")

	return res
}

// ArtistSetKey turns a credit list into a comparable key: every artist string is
// split on the common delimiters, each part is normalised, duplicates are
// removed, the set is sorted and joined. Order and separators no longer matter,
// so "Sad Keyboard Guy;Gardens" and "Gardens, Sad Keyboard Guy" produce the same
// key.
func ArtistSetKey(names []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		for _, chunk := range artistCreditDelimiter.Split(name, -1) {
			for _, piece := range artistCreditWordDelimiter.Split(chunk, -1) {
				key := NormalizationKey(piece)
				if key == "" {
					continue
				}
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				parts = append(parts, key)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1f")
}

// ArtistSetsCompatible reports whether two credit lists can describe the same
// song.
//
// A source often reports only the primary artist while the local file credits
// the featured one too ("Gardens" vs "Gardens" + "Sad Keyboard Guy"), so equality
// is too strict. The sets are compatible when they share at least one artist and
// one contains the other; a title and duration still have to match separately.
func ArtistSetsCompatible(left, right []string) bool {
	leftKey := ArtistSetKey(left)
	rightKey := ArtistSetKey(right)
	if leftKey == "" || rightKey == "" {
		return false
	}
	if leftKey == rightKey {
		return true
	}
	leftParts := strings.Split(leftKey, "\x1f")
	rightParts := strings.Split(rightKey, "\x1f")
	if !artistSetsIntersect(leftParts, rightParts) {
		return false
	}
	return artistSetContains(leftParts, rightParts) || artistSetContains(rightParts, leftParts)
}

func artistSetsIntersect(left, right []string) bool {
	for _, want := range left {
		for _, have := range right {
			if want == have {
				return true
			}
		}
	}
	return false
}

// artistSetContains reports whether every part of subset appears in superset.
func artistSetContains(subset, superset []string) bool {
	for _, want := range subset {
		found := false
		for _, have := range superset {
			if want == have {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// DefaultDelimiters returns the built-in delimiter set used when the user has
// not customized splitting for a field.
func DefaultDelimiters() []string {
	return []string{";", "\\\\", ","}
}

// RawTagSeparator joins multiple same-named tag frames (e.g. two ARTIST tags)
// into a single Raw*Names string. It is always treated as a value boundary when
// re-splitting, independent of the user's delimiters, so genuine multi-value
// tags always yield separate entities.
const RawTagSeparator = "; "

const maxDelimiterLen = 5

// ValidateDelimiters checks a user-provided delimiter list. An empty list is
// allowed and means "do not split" (the whole tag value is one entity). Rules:
//   - each entry non-empty / not whitespace-only after trimming
//   - each entry no longer than maxDelimiterLen
//   - no duplicates (post-trim, case-sensitive)
func ValidateDelimiters(list []string) error {
	seen := make(map[string]bool, len(list))
	for _, d := range list {
		t := strings.TrimSpace(d)
		if t == "" {
			return fmt.Errorf("delimiter cannot be empty")
		}
		if len([]rune(t)) > maxDelimiterLen {
			return fmt.Errorf("delimiter %q is too long (max %d characters)", t, maxDelimiterLen)
		}
		if seen[t] {
			return fmt.Errorf("duplicate delimiter %q", t)
		}
		seen[t] = true
	}
	return nil
}

// SplitNames breaks a concatenated value into individual names using the given
// delimiters. Each delimiter is treated as a literal separator substring.
// Results are trimmed, empties dropped, and deduplicated case-insensitively.
// With no usable delimiters the whole (trimmed) string is returned as one name.
func SplitNames(s string, delimiters []string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}

	// Collect non-empty literal delimiters.
	parts := []string{s}
	for _, d := range delimiters {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		var next []string
		for _, p := range parts {
			next = append(next, strings.Split(p, d)...)
		}
		parts = next
	}

	var final []string
	seen := make(map[string]bool)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && !seen[strings.ToLower(p)] {
			final = append(final, p)
			seen[strings.ToLower(p)] = true
		}
	}

	return final
}
