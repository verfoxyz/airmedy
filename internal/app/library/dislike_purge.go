package library

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"airmedy/internal/domain"
)

// CountDislikeTargets reports how many local tracks a dislike would delete. It
// matches the library by the same rules the delete flow would use, and changes
// nothing.
func (s *LibraryService) CountDislikeTargets(
	ctx context.Context, kind, matchKey, title, artist string,
) (int, error) {
	tracks, err := s.dislikeTargets(ctx, kind, title, artist)
	if err != nil {
		return 0, err
	}
	return len(tracks), nil
}

// PurgeDislikeTargets deletes every track a dislike names. A local row goes
// through DeleteTrackFile (file, row, artwork with no remaining reference,
// stream cache, saved lyrics, playlist links, search document); a remote row
// goes through RemoveTrackFromLibrary (row, its stream cache and cached audio,
// saved lyrics). Playback is advanced first when the current row is one of the
// targets.
//
// A failure is returned with the number of rows that did succeed: one locked
// file must not hide the deletion of the others.
func (s *LibraryService) PurgeDislikeTargets(
	ctx context.Context, kind, matchKey, title, artist string,
) (int, error) {
	tracks, err := s.dislikeTargets(ctx, kind, title, artist)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var failures []error
	for _, track := range tracks {
		if track == nil || track.ID == "" {
			continue
		}
		s.mu.RLock()
		remover := s.playbackRemover
		s.mu.RUnlock()
		if remover != nil {
			if err := remover.RemoveFromPlaybackAndAdvance(ctx, track.ID); err != nil {
				failures = append(failures, fmt.Errorf("%s: stop playback: %w", track.ID, err))
				continue
			}
		}
		var deleteErr error
		if track.IsRemote() {
			deleteErr = s.RemoveTrackFromLibrary(ctx, track.ID)
		} else {
			deleteErr = s.DeleteTrackFile(ctx, track.ID)
		}
		if deleteErr != nil {
			failures = append(failures, deleteErr)
			continue
		}
		deleted++
	}
	if len(failures) > 0 {
		return deleted, fmt.Errorf("delete %d of %d tracks: %w", len(failures), len(tracks),
			errors.Join(failures...))
	}
	return deleted, nil
}

// dislikeTargets selects the on-disk rows a dislike covers.
//
// A track dislike matches one song by normalized title and a compatible artist
// set, so a feature credit or a different separator does not hide it. An artist
// dislike matches every row sharing one normalized artist key; it is the broad
// case the UI gates behind a second confirmation.
func (s *LibraryService) dislikeTargets(
	ctx context.Context, kind, title, artist string,
) ([]*domain.TrackDTO, error) {
	tracks, err := s.trackRepo.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("dislike purge: list tracks: %w", err)
	}
	targets := make([]*domain.TrackDTO, 0)
	switch kind {
	case domain.LastFmDislikeTrack:
		titleKey := domain.NormalizationKey(title)
		artist = strings.TrimSpace(artist)
		if titleKey == "" || artist == "" {
			return targets, nil
		}
		for _, track := range tracks {
			if !dislikePurgeCandidate(track) {
				continue
			}
			if domain.NormalizationKey(track.Title) != titleKey {
				continue
			}
			if !domain.ArtistSetsCompatible(dislikeTrackArtists(track), []string{artist}) {
				continue
			}
			targets = append(targets, track)
		}
	case domain.LastFmDislikeArtist:
		wanted := make(map[string]struct{})
		for _, part := range domain.ArtistKeyParts([]string{artist}) {
			wanted[part] = struct{}{}
		}
		if len(wanted) == 0 {
			return targets, nil
		}
		for _, track := range tracks {
			if !dislikePurgeCandidate(track) {
				continue
			}
			for _, part := range domain.ArtistKeyParts(dislikeTrackArtists(track)) {
				if _, hit := wanted[part]; hit {
					targets = append(targets, track)
					break
				}
			}
		}
	}
	return targets, nil
}

// dislikePurgeCandidate is a row the delete path can remove. A remote row is
// included: its stream cache and the row itself are part of the purge.
func dislikePurgeCandidate(track *domain.TrackDTO) bool {
	if track == nil {
		return false
	}
	return track.IsRemote() || strings.TrimSpace(track.Path) != ""
}

// dislikeTrackArtists returns the credited names, falling back to the raw tag
// string so a multi-artist credit still splits the same way elsewhere.
func dislikeTrackArtists(track *domain.TrackDTO) []string {
	if track == nil {
		return nil
	}
	names := make([]string, 0, len(track.Artists))
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			names = append(names, artist.Name)
		}
	}
	if len(names) == 0 && strings.TrimSpace(track.RawArtistNames) != "" {
		names = append(names, track.RawArtistNames)
	}
	return names
}
