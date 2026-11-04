package catalog

import (
	"testing"

	"airmedy/internal/domain"
)

// TestService_RemembersCoversForTheSavePath pins the fallback the search page
// relies on: when the plugin omits cover_url (thumbnails off), a later action can
// still embed the last cover the host saw for that identity.
func TestService_RemembersCoversForTheSavePath(t *testing.T) {
	service := NewService(Deps{})
	identity := domain.CatalogIdentity{Provider: "wy", ExternalID: "1"}

	service.rememberCovers([]domain.CatalogSearchResult{
		{
			Provider: "wy", ExternalID: "1",
			CoverURL:      "https://p1.music.126.net/cover.jpg?param=100y100",
			CoverURLLarge: "https://p1.music.126.net/cover.jpg?param=500y500",
		},
		{Provider: "wy", ExternalID: "2"},
	})

	if got := service.cachedCover(identity); got != "https://p1.music.126.net/cover.jpg?param=500y500" {
		t.Fatalf("cached cover = %q, want the large rendition", got)
	}
	if got := service.cachedCover(domain.CatalogIdentity{Provider: "wy", ExternalID: "2"}); got != "" {
		t.Fatalf("cover for a row without one = %q, want empty", got)
	}
}

// TestFuseRanks_CarriesTheCoverURL: the merged row must keep the URL the source
// reported, otherwise the save action never receives artwork.
func TestFuseRanks_CarriesTheCoverURL(t *testing.T) {
	fused := fuseRanks("晴天", []string{"wy"}, map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{{
			ExternalID:    "1",
			Title:         "晴天",
			CoverURL:      " https://p1.music.126.net/cover.jpg ",
			CoverURLLarge: " https://p1.music.126.net/cover-500.jpg ",
		}}},
	})

	if len(fused) != 1 {
		t.Fatalf("fused = %+v, want one row", fused)
	}
	if fused[0].CoverURL != "https://p1.music.126.net/cover.jpg" {
		t.Fatalf("CoverURL = %q, want the trimmed source URL", fused[0].CoverURL)
	}
	if fused[0].CoverURLLarge != "https://p1.music.126.net/cover-500.jpg" {
		t.Fatalf("CoverURLLarge = %q, want the trimmed large URL", fused[0].CoverURLLarge)
	}
}
