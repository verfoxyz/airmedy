package download

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"airmedy/internal/domain"
)

// CatalogRowStates implements domain.CatalogRowStateResolver.
//
// The search screen asks this before offering an action, so a row the user has
// already registered is not offered for a second download: a finished row reads
// as "in library", a queued/active one as "downloading", a permanently failed
// one as "failed" (where the UI offers a retry instead of a fresh enqueue), and
// a registered row with no job at all as "saved" (metadata only, which is what
// a playlist add with auto-download off leaves behind).
//
// It answers for a whole page in one call. The queue is read once — a job list is
// small and already loaded by the downloads panel — and each identity costs one
// indexed lookup by (provider, external_id), which is the same lookup
// EnsureRemoteTrack does.
func (s *Service) CatalogRowStates(
	ctx context.Context, identities []domain.CatalogIdentity,
) (map[domain.CatalogIdentity]string, error) {
	states := make(map[domain.CatalogIdentity]string, len(identities))
	if len(identities) == 0 {
		return states, nil
	}
	if s.merger == nil {
		// Without the merge repository there is no identity lookup; reporting no
		// states is honest, and the search page still works.
		return states, nil
	}

	// The newest job per track decides: a retried job overwrites the failed row,
	// and a finished job leaves an older done row behind.
	jobs, err := s.jobs.List(ctx)
	if err != nil {
		return states, fmt.Errorf("catalog row states: list jobs: %w", err)
	}
	latest := make(map[string]*domain.DownloadJob, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		current := latest[job.TrackID]
		if current == nil || job.UpdatedAt.After(current.UpdatedAt) {
			latest[job.TrackID] = job
		}
	}

	var errs []error
	for _, identity := range identities {
		provider := domain.NormalizeProvider(identity.Provider)
		externalID := strings.TrimSpace(identity.ExternalID)
		if provider == "" || externalID == "" {
			// An incomplete identity was never registrable, so it has no state.
			continue
		}

		// Key by the normalised identity: that is what a caller reads back.
		key := domain.CatalogIdentity{Provider: provider, ExternalID: externalID}
		track, lookupErr := s.merger.FindByRemoteIdentity(ctx, provider, externalID)
		if lookupErr != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", provider, externalID, lookupErr))
			continue
		}
		if track == nil {
			continue
		}

		if !track.IsRemote() {
			states[key] = domain.CatalogRowStateInLibrary
			continue
		}

		job := latest[track.ID]
		switch {
		case job != nil && domain.IsActiveState(job.State):
			states[key] = domain.CatalogRowStateDownloading
		case job != nil && job.State == domain.DownloadStateFailed:
			states[key] = domain.CatalogRowStateFailed
		default:
			// Registered but idle (never queued, canceled, or finished without
			// the merge): the library holds the metadata only. Reporting "saved"
			// keeps the row from looking like a download that never started.
			states[key] = domain.CatalogRowStateSaved
		}
	}

	if len(errs) > 0 {
		return states, errors.Join(errs...)
	}
	return states, nil
}

// catalogLocalTolerance is the ±3s window used to recognise an imported file as
// the same song as a search hit.
const catalogLocalTolerance = 3

// CatalogLocalMatches implements domain.CatalogRowStateResolver.
//
// A file the user imported carries no provider identity, so the identity lookup
// cannot see it and the search row would offer a second download of the same
// song. This answers the other question: does an on-disk track already look
// like this row? The rule matches the merge heuristic — same normalised title,
// same primary artist and duration within ±3s — because a looser rule would
// pair a live version or a cover with the studio track.
func (s *Service) CatalogLocalMatches(
	ctx context.Context, queries []domain.CatalogLocalQuery,
) (map[domain.CatalogIdentity]string, error) {
	matches := make(map[domain.CatalogIdentity]string, len(queries))

	type candidate struct {
		query    domain.CatalogLocalQuery
		identity domain.CatalogIdentity
	}
	valid := make([]candidate, 0, len(queries))
	minDuration, maxDuration := 0, 0
	for _, query := range queries {
		if strings.TrimSpace(query.Title) == "" || query.Duration <= 0 {
			// Without a duration the match is a guess, and a wrong match would
			// silently replace the user's download with a different song.
			continue
		}
		valid = append(valid, candidate{query: query, identity: query.Identity})
		low, high := query.Duration-catalogLocalTolerance, query.Duration+catalogLocalTolerance
		if minDuration == 0 || low < minDuration {
			minDuration = low
		}
		if high > maxDuration {
			maxDuration = high
		}
	}
	if len(valid) == 0 {
		return matches, nil
	}

	tracks, err := s.tracks.FindPlayableByDuration(ctx, minDuration, maxDuration)
	if err != nil {
		return matches, fmt.Errorf("catalog local matches: load candidates: %w", err)
	}

	for _, c := range valid {
		bestID, bestDistance := "", -1
		wantedArtist := ""
		if len(c.query.Artists) > 0 {
			wantedArtist = domain.NormalizationKey(c.query.Artists[0])
		}
		for _, track := range tracks {
			if track == nil {
				continue
			}
			distance := track.Duration - c.query.Duration
			if distance < 0 {
				distance = -distance
			}
			if distance > catalogLocalTolerance {
				continue
			}
			if !sameCatalogTitle(track.Title, c.query.Title) {
				continue
			}
			artist := domain.NormalizationKey(primaryArtist(track))
			if wantedArtist != "" && artist != "" && artist != wantedArtist {
				continue
			}
			if bestID == "" || distance < bestDistance {
				bestID, bestDistance = track.ID, distance
			}
		}
		if bestID != "" {
			matches[c.identity] = bestID
		}
	}
	return matches, nil
}

// GetTrack exposes one library row for the catalogue action path, which needs
// to verify a matched local candidate still exists before reusing it.
func (s *Service) GetTrack(ctx context.Context, trackID string) (*domain.TrackDTO, error) {
	return s.tracks.GetByID(ctx, trackID)
}

// sameCatalogTitle compares two titles after normalisation. An exact match is
// the common case; the provider adds a version suffix ("前前前世 (movie ver.)")
// while the imported file keeps the bare title, so a trailing parenthetical or
// dash suffix is also accepted. The duration window and artist check still
// guard against pairing a cover or a live version.
func sameCatalogTitle(left, right string) bool {
	a := domain.NormalizationKey(left)
	b := domain.NormalizationKey(right)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	for _, separator := range []string{" (", " （", " - ", " – ", " -"} {
		if strings.HasPrefix(longer, shorter+separator) {
			return true
		}
	}
	return false
}
