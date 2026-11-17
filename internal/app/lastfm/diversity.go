package lastfm

import (
	"airmedy/internal/domain"
)

// Diversity limits shared by the discover list and the radio queue.
const (
	// diversityArtistLimit is how many tracks one artist may contribute. The
	// cap also guarantees three artists in the first ten tracks whenever the
	// candidate pool has three artists; a narrower pool cannot, so the rule
	// never blocks a short list.
	diversityArtistLimit = 2
)

// diversityState tracks the counts a batch has already spent.
type diversityState struct {
	artistCounts map[string]int
	albums       map[string]struct{}
	lastArtist   string
}

func newDiversityState(initialArtistCounts map[string]int) *diversityState {
	counts := make(map[string]int, len(initialArtistCounts))
	for key, count := range initialArtistCounts {
		counts[key] = count
	}
	return &diversityState{
		artistCounts: counts,
		albums:       make(map[string]struct{}),
	}
}

// selectDiverseTracks picks up to limit candidates in their existing order.
// The per-artist cap is hard; the album rule and the adjacent-artist rule relax
// in that order, so a pool that cannot satisfy them still fills the list.
//
// initialArtistCounts carries the per-artist usage of an existing list, so
// "load more" keeps the limit global.
func selectDiverseTracks(
	candidates []LastFmTrack, limit int, initialArtistCounts map[string]int,
) []LastFmTrack {
	if limit <= 0 || len(candidates) == 0 {
		return []LastFmTrack{}
	}
	state := newDiversityState(initialArtistCounts)
	taken := make([]bool, len(candidates))
	selected := make([]LastFmTrack, 0, limit)

	for _, rules := range []struct{ album, adjacent bool }{
		{album: true, adjacent: true},
		{album: false, adjacent: true},
		{album: false, adjacent: false},
	} {
		for index, track := range candidates {
			if len(selected) >= limit {
				return selected
			}
			if taken[index] {
				continue
			}
			if !state.artistAllowed(track.Artist) {
				continue
			}
			if rules.adjacent && state.isAdjacent(track.Artist) {
				continue
			}
			if rules.album {
				if album := albumIdentity(track); album != "" {
					if _, used := state.albums[album]; used {
						continue
					}
				}
			}
			state.take(track)
			taken[index] = true
			selected = append(selected, track)
		}
	}
	return selected
}

func (s *diversityState) artistAllowed(artist string) bool {
	key := domain.NormalizationKey(artist)
	if key == "" {
		return true
	}
	return s.artistCounts[key] < diversityArtistLimit
}

func (s *diversityState) isAdjacent(artist string) bool {
	key := domain.NormalizationKey(artist)
	return key != "" && key == s.lastArtist
}

func (s *diversityState) take(track LastFmTrack) {
	artistKey := domain.NormalizationKey(track.Artist)
	if artistKey != "" {
		s.artistCounts[artistKey]++
		s.lastArtist = artistKey
	}
	if album := albumIdentity(track); album != "" {
		s.albums[album] = struct{}{}
	}
}

// albumIdentity scopes an album name to its artist: two artists commonly share
// generic titles ("Greatest Hits"), and they are not the same album.
func albumIdentity(track LastFmTrack) string {
	album := domain.NormalizationKey(track.Album)
	if album == "" {
		return ""
	}
	artist := domain.NormalizationKey(track.Artist)
	return artist + "\x00" + album
}
