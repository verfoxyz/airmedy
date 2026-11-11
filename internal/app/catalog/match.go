package catalog

import (
	"context"
	"strings"

	"airmedy/internal/domain"
)

// MatchTrackToleranceSeconds is the duration window that still counts as the
// same recording when an external catalogue (Last.fm) is matched to a source.
const MatchTrackToleranceSeconds = 5

// MatchTrack searches the enabled sources for one title + artist and returns
// only the rows that are the same recording.
//
// It is the recommendation page's matching primitive. Last.fm supplies a title
// and artist but no provider identity, so the track has to be looked up in the
// catalogue before it can be played or saved. Search already merges duplicate
// rows across sources, resolves a matching local file and stamps Downloadable;
// this method adds the strict identity check a keyword search cannot promise.
//
// The returned order is the search's own order, so a caller can prefer the first
// matching source. A partial source failure is returned alongside the rows that
// did answer; the caller decides whether the page is usable.
func (s *Service) MatchTrack(
	ctx context.Context, title, artist string, duration int, sources []string,
) ([]domain.CatalogSearchResult, error) {
	title = strings.TrimSpace(title)
	artist = strings.TrimSpace(artist)
	if title == "" || artist == "" {
		return nil, nil
	}

	page, err := s.Search(ctx, domain.CatalogSearchRequest{
		Query:   strings.TrimSpace(title + " " + artist),
		Sources: sources,
		Limit:   domain.MaxCatalogSearchPageSize,
	})
	if err != nil && len(page.Rows) == 0 {
		return nil, err
	}

	wantedTitle := domain.NormalizationKey(title)
	wantedArtist := domain.NormalizationKey(artist)
	matches := make([]domain.CatalogSearchResult, 0, 4)
	for _, row := range page.Rows {
		if domain.NormalizationKey(row.Title) != wantedTitle {
			continue
		}
		if !hasArtist(row.Artists, wantedArtist) {
			continue
		}
		if duration > 0 && row.Duration > 0 {
			difference := row.Duration - duration
			if difference < 0 {
				difference = -difference
			}
			if difference > MatchTrackToleranceSeconds {
				continue
			}
		}
		matches = append(matches, row)
	}
	return matches, err
}

// hasArtist accepts a row whose credited artists include the wanted one.
//
// Last.fm names the primary artist, while a source may list it as a featured
// artist or append collaborators; comparing only the first credit dropped
// playable matches that the page could otherwise offer.
func hasArtist(artists []string, wanted string) bool {
	if wanted == "" {
		return false
	}
	for _, artist := range artists {
		if domain.NormalizationKey(artist) == wanted {
			return true
		}
	}
	return false
}
