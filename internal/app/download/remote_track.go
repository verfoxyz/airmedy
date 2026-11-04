package download

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"airmedy/internal/domain"

	"github.com/google/uuid"
)

// EnsureRemoteTrack registers an online track in the library, or returns the row
// that already represents it.
//
// This is the entry point the search/recommendation side needs: the download
// queue can only work with a tracks row, and a row for a track that has no file
// yet has to be created deliberately (availability=remote, virtual path). The
// (provider, external_id) partial unique index guarantees one row per identity;
// a concurrent insert is resolved by looking again rather than failing.
func (s *Service) EnsureRemoteTrack(ctx context.Context, spec domain.RemoteTrackSpec) (*domain.TrackDTO, error) {
	provider := domain.NormalizeProvider(spec.Provider)
	externalID := strings.TrimSpace(spec.ExternalID)
	if provider == "" || externalID == "" {
		return nil, fmt.Errorf("ensure remote track: provider and external id are required")
	}

	existing, err := s.merger.FindByRemoteIdentity(ctx, provider, externalID)
	if err != nil {
		return nil, fmt.Errorf("ensure remote track: lookup: %w", err)
	}
	if existing != nil {
		// A row registered by an older build may lack the album name (the search
		// result carried it but it was never persisted). Fill it while the row is
		// still remote so the finished file can be tagged with it.
		if existing.IsRemote() && strings.TrimSpace(existing.RawAlbumName) == "" && strings.TrimSpace(spec.AlbumName) != "" {
			tr := existing.Track
			tr.RawAlbumName = strings.TrimSpace(spec.AlbumName)
			if err := s.tracks.Upsert(ctx, &tr); err == nil {
				existing.RawAlbumName = tr.RawAlbumName
			}
		}
		return existing, nil
	}

	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = externalID
	}
	tr := &domain.Track{
		ID:                  uuid.NewString(),
		Path:                domain.RemotePath(provider, externalID),
		Title:               title,
		SortTitle:           strings.ToLower(title),
		RawArtistNames:      strings.Join(nonEmpty(spec.Artists), "; "),
		RawAlbumArtistNames: strings.Join(nonEmpty(spec.Artists), "; "),
		RawAlbumName:        strings.TrimSpace(spec.AlbumName),
		Duration:            spec.Duration,
		Availability:        domain.AvailabilityRemote,
		Provider:            provider,
		ExternalID:          externalID,
	}
	if err := s.tracks.Upsert(ctx, tr); err != nil {
		// Lost a race with another insert of the same identity: the unique index
		// rejected ours, so the row now exists and is the right answer.
		if again, lookupErr := s.merger.FindByRemoteIdentity(ctx, provider, externalID); lookupErr == nil && again != nil {
			return again, nil
		}
		return nil, fmt.Errorf("ensure remote track: insert: %w", err)
	}
	return &domain.TrackDTO{Track: *tr}, nil
}

// DownloadRemote registers the track if needed and queues it. Enqueuing is
// idempotent: an already queued/active track keeps its job instead of erroring,
// because "download this" is a user intent, not a state transition.
func (s *Service) DownloadRemote(ctx context.Context, spec domain.RemoteTrackSpec, playlistID, quality string) (*domain.TrackDTO, error) {
	track, err := s.EnsureRemoteTrack(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.EnqueueWithCover(ctx, track.ID, playlistID, quality, spec.CoverURL); err != nil &&
		!errors.Is(err, domain.ErrDownloadAlreadyQueued) {
		return nil, err
	}
	return track, nil
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}
