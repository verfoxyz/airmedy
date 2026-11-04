package download

import (
	"fmt"
	"strings"
	"unicode"

	"airmedy/internal/domain"
)

// maxPathComponent bounds the file name. Windows rejects a path component over
// 255 bytes and Explorer dislikes anything long enough to scroll, so titles are
// truncated well below the limit.
const maxPathComponent = 120

// fallbacks keep a file name from becoming empty when the source has no artist
// or title at all.
const (
	unknownArtist    = "Unknown Artist"
	defaultTrackName = "Unknown Track"
	// unknownTrackNumber is the tag value used when the catalogue has none.
	unknownTrackNumber = 0
)

// windowsReserved are device names Windows refuses as a file or folder name,
// with or without an extension.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// fileSpec is what a track needs to become a file name on disk.
type fileSpec struct {
	Artist string
	Title  string
	Ext    string
}

// relativePath renders the flat on-disk layout "<title> - <artist>.<ext>" or
// "<artist> - <title>.<ext>" according to the user's naming order. Downloads
// live directly under the download root; there are no artist or album
// subfolders. Every field is sanitised, so the result is a single file name.
func relativePath(spec fileSpec, order string) (string, error) {
	artist := sanitizeComponent(spec.Artist)
	if artist == "" {
		artist = unknownArtist
	}
	title := sanitizeComponent(spec.Title)
	if title == "" {
		title = defaultTrackName
	}
	ext := sanitizeComponent(strings.TrimPrefix(spec.Ext, "."))
	if ext == "" {
		return "", fmt.Errorf("download: cannot build a file name without an extension (track %q)", spec.Title)
	}

	var stem string
	if domain.ResolveNamingOrder(order) == domain.NamingOrderArtistTitle {
		stem = artist + " - " + title
	} else {
		stem = title + " - " + artist
	}
	return truncateRunes(stem, maxPathComponent) + "." + ext, nil
}

// sanitizeComponent makes s safe as a single path component on every platform
// we ship to:
//
//   - control characters are dropped outright,
//   - the reserved set <>:"/\|?* is replaced with an underscore (Windows
//     refuses them, and "/" and "\" would also split a component),
//   - trailing dots and spaces are trimmed (Windows silently strips them, so
//     two different titles would collide),
//   - a reserved device name gets a trailing underscore,
//   - long names are truncated on a rune boundary.
//
// It never returns a path separator, so callers can join the result blindly.
func sanitizeComponent(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return -1
		case strings.ContainsRune(`<>:"/\|?*`, r):
			return '_'
		default:
			return r
		}
	}, s)

	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ". ")

	if s == "" {
		return ""
	}

	if isReservedName(s) {
		s += "_"
	}
	if isRelativeMarker(s) {
		// "." and ".." must never survive as a component: joined into a path
		// they mean "here" and "parent" and would walk out of the download dir.
		s += "_"
	}
	return truncateRunes(s, maxPathComponent)
}

// isRelativeMarker reports names made only of dots ("." / ".." / "..."): after
// sanitising an artist of "../.." each group becomes a literal folder name
// instead of a directory walk.
func isRelativeMarker(s string) bool {
	return strings.Trim(s, ".") == ""
}

// isReservedName checks the stem before the first dot: "CON.mp3" is as
// reserved as "CON".
func isReservedName(s string) bool {
	stem, _, _ := strings.Cut(s, ".")
	return windowsReserved[strings.ToUpper(strings.TrimSpace(stem))]
}

// truncateRunes cuts s to at most limit runes without splitting a multi-byte
// character (a broken truncation would produce mojibake folder names).
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	// Leave room for a "..." marker so two long titles that share a prefix stay
	// distinguishable.
	return string(runes[:limit-3]) + "..."
}

// yearComponent renders a year for a tag, returning 0 when the catalogue had
// none so the writer leaves the frame untouched.
func yearComponent(year int) int {
	if year < 0 {
		return 0
	}
	return year
}

// DefaultDownloadDirName is the folder created under the user's music directory
// when no download directory is configured.
const DefaultDownloadDirName = "Airmedy"
