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
	matches := make([]domain.CatalogSearchResult, 0, 4)
	for _, row := range page.Rows {
		if domain.NormalizationKey(row.Title) != wantedTitle {
			continue
		}
		if !s.hasArtist(ctx, row.Artists, artist) {
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

// MatchLocalTrack finds an on-disk row that already represents title + artists.
//
// It is the local-first step of recommendation matching: a hit is playable and
// already has artwork, so the caller skips the cloud search entirely. A
// duration disagreement still matches and is reported as suspicious.
func (s *Service) MatchLocalTrack(
	ctx context.Context, title string, artists []string, duration int,
) (*domain.LocalTrackMatch, error) {
	if s.localMatcher == nil {
		return nil, nil
	}
	return s.localMatcher.MatchLocalTrack(ctx, title, artists, duration)
}

// hasArtist reports whether the row and the wanted artist describe the same
// credit set, expanding one hop through the alias store.
//
// Sources disagree on order and separators ("Sad Keyboard Guy;Gardens" vs
// "Gardens, Sad Keyboard Guy"), so the comparison is on the normalised set.
// A row crediting several artists still matches when one of them is the wanted
// artist: Last.fm names the primary artist while a source may list it as a
// featured credit.
func (s *Service) hasArtist(ctx context.Context, artists []string, wanted string) bool {
	return domain.ArtistNamesCompatibleWithAliases(ctx, s.aliases, artists, []string{wanted})
}
