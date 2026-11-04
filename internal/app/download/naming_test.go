package download

import (
	"path/filepath"
	"strings"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelativePath_DefaultIsTitleThenArtist(t *testing.T) {
	got, err := relativePath(fileSpec{Artist: "周杰伦", Title: "晴天", Ext: "mp3"}, "")
	require.NoError(t, err)
	assert.Equal(t, "晴天 - 周杰伦.mp3", got)
}

func TestRelativePath_ArtistTitleOrder(t *testing.T) {
	got, err := relativePath(
		fileSpec{Artist: "周杰伦", Title: "晴天", Ext: "mp3"},
		domain.NamingOrderArtistTitle,
	)
	require.NoError(t, err)
	assert.Equal(t, "周杰伦 - 晴天.mp3", got)
}

func TestRelativePath_UnknownOrderFallsBackToDefault(t *testing.T) {
	got, err := relativePath(fileSpec{Artist: "A", Title: "T", Ext: "mp3"}, "artist-album")
	require.NoError(t, err)
	assert.Equal(t, "T - A.mp3", got)
}

func TestRelativePath_StripsTheExtensionDot(t *testing.T) {
	got, err := relativePath(fileSpec{Artist: "A", Title: "T", Ext: ".flac"}, "")
	require.NoError(t, err)
	assert.Equal(t, "T - A.flac", got, "a leading dot on ext must not double up")
}

func TestRelativePath_MissingFieldsFallBack(t *testing.T) {
	got, err := relativePath(fileSpec{Ext: "mp3"}, "")
	require.NoError(t, err)
	assert.Equal(t, defaultTrackName+" - "+unknownArtist+".mp3", got)
}

func TestRelativePath_RequiresExtension(t *testing.T) {
	_, err := relativePath(fileSpec{Artist: "A", Title: "T"}, "")
	assert.Error(t, err)
}

func TestRelativePath_FlatNameIsASingleComponent(t *testing.T) {
	got, err := relativePath(fileSpec{Artist: "A", Title: "T", Ext: "mp3"}, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(got), got, "the layout is flat: no artist or album subfolders")
	assert.NotContains(t, got, string(filepath.Separator))
}

func TestRelativePath_LongNameIsTruncated(t *testing.T) {
	got, err := relativePath(
		fileSpec{Artist: strings.Repeat("A", maxPathComponent), Title: strings.Repeat("T", maxPathComponent), Ext: "mp3"},
		"",
	)
	require.NoError(t, err)
	stem := strings.TrimSuffix(got, ".mp3")
	assert.LessOrEqual(t, len([]rune(stem)), maxPathComponent)
	assert.True(t, strings.HasSuffix(stem, "..."))
}

func TestSanitizeComponent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"windows reserved chars", `AC/DC: Back?in*Black"<>|`, "AC_DC_ Back_in_Black____"},
		{"separators never survive", `..\..\escape`, ".._.._escape"},
		{"trailing dot and space trimmed", "Album.  ", "Album"},
		{"control characters dropped", "Ti\x00tle\x1f", "Title"},
		{"reserved device name", "CON", "CON_"},
		{"reserved device name with extension", "nul.mp3", "nul.mp3_"},
		{"not reserved when embedded", "CONCERT", "CONCERT"},
		{"empty stays empty", "   ", ""},
		{"only trimmed to nothing", "...", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeComponent(tc.in))
		})
	}
}

func TestSanitizeComponent_TruncatesLongTitlesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("晴", maxPathComponent+50)
	got := sanitizeComponent(long)
	assert.LessOrEqual(t, len([]rune(got)), maxPathComponent)
	assert.True(t, strings.HasSuffix(got, "..."))
	// Must still be valid UTF-8: a byte-wise cut would corrupt the last rune.
	for _, r := range got {
		assert.NotEqual(t, '\uFFFD', r)
	}
}

func TestSanitizeComponent_NeverReturnsAPathSeparator(t *testing.T) {
	for _, in := range []string{`a/b`, `a\b`, `../`, `..\..\..`} {
		got := sanitizeComponent(in)
		assert.NotContains(t, got, "/", "input %q", in)
		assert.NotContains(t, got, `\`, "input %q", in)
		assert.False(t, got == "." || got == "..", "input %q", in)
	}
}

func TestRelativePath_CannotEscapeTheRoot(t *testing.T) {
	got, err := relativePath(fileSpec{
		Artist: "../..", Title: "../../etc/passwd", Ext: "mp3",
	}, "")
	require.NoError(t, err)

	// Every component must be a literal name: no ".", no "..", and no
	// separator surviving inside one, so joining never leaves the root.
	for _, part := range strings.Fields(strings.ReplaceAll(got, string(filepath.Separator), " ")) {
		assert.False(t, isRelativeMarker(part), "component %q is a directory marker", part)
		assert.NotEqual(t, ".", part)
		assert.NotEqual(t, "..", part)
	}

	root := t.TempDir()
	joined := filepath.Clean(filepath.Join(root, got))
	assert.True(t, strings.HasPrefix(joined, filepath.Clean(root)+string(filepath.Separator)),
		"path escaped the download root: %s", joined)
}
