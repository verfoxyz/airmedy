package mediarequest

import (
	"context"
	"log/slog"
	"strings"

	"airmedy/internal/domain"
)

// SettingsReader is the settings slice the saver needs. *appsettings.Service
// satisfies it; the narrow port keeps this package out of appsettings.
type SettingsReader interface {
	GetSettings(ctx context.Context) (*domain.AppSettings, error)
}

// Saver is the composition root's adapter for every save entry point: favorites,
// playlist adds (single and batch) and catalog/search/Last.fm saves. It upserts
// one media request and wakes the worker; it never creates a job or a promotion
// task itself.
type Saver struct {
	service  *Service
	worker   *Worker
	settings SettingsReader
	logger   *slog.Logger
}

func NewSaver(
	service *Service, worker *Worker, settings SettingsReader, logger *slog.Logger,
) *Saver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Saver{service: service, worker: worker, settings: settings, logger: logger}
}

// OnRemoteTrackSaved implements catalog.SavedTracks and the playlist/favorite
// hooks. A non-explicit save (adding to a playlist) still follows the
// auto-download setting; an explicit save always records the request.
//
// The returned row state is what the search chip shows immediately: "saved" when
// the identity is already in the library, otherwise "downloading" (the UI's one
// task state, covering resolving/downloading/promoting).
func (s *Saver) OnRemoteTrackSaved(
	ctx context.Context, ev domain.RemoteTrackSaved,
) (string, error) {
	track := ev.Track
	if track == nil || !track.IsRemote() {
		return "", nil
	}
	if !ev.Explicit {
		enabled, err := s.autoDownloadEnabled(ctx)
		if err != nil {
			return "", err
		}
		if !enabled {
			return domain.CatalogRowStateSaved, nil
		}
	}

	request, err := s.service.Upsert(ctx, UpsertInput{
		Provider:   track.Provider,
		ExternalID: track.ExternalID,
		TrackID:    track.ID,
		// Snapshot the label: the merge triggered by this request deletes the
		// remote row, and the tasks page must still name it afterwards.
		Title:  track.Title,
		Artist: requestArtist(track),
		// Execution decides between promotion and download from the cache and
		// quality; every save expresses the same intent.
		Desired: domain.MediaRequestDesiredPromote,
		Quality: s.defaultQuality(ctx),
	})
	if err != nil {
		return "", err
	}
	if s.worker != nil {
		s.worker.Wake()
	}
	if request != nil && request.State == domain.MediaRequestStateDone {
		return domain.CatalogRowStateSaved, nil
	}
	return domain.CatalogRowStateDownloading, nil
}

// UpsertDownload records an explicit re-download: desired=download, which
// reopens a finished request and lets the worker fetch a fresh file.
func (s *Saver) UpsertDownload(
	ctx context.Context, provider, externalID, trackID, quality string,
) error {
	if _, err := s.service.Upsert(ctx, UpsertInput{
		Provider:   provider,
		ExternalID: externalID,
		TrackID:    trackID,
		Desired:    domain.MediaRequestDesiredDownload,
		Quality:    quality,
	}); err != nil {
		return err
	}
	if s.worker != nil {
		s.worker.Wake()
	}
	return nil
}

// SetLocalTrackChecker installs the executor's local-file probe, used to
// re-open a completed request whose file was deleted.
func (s *Saver) SetLocalTrackChecker(checker LocalTrackChecker) {
	s.service.SetLocalTrackChecker(checker)
}

// InvalidateLocalFile implements library.MediaRequestInvalidator: deleting a
// local file re-opens the identity's finished request and wakes the worker, so
// the next save promotes or downloads again instead of being a no-op.
func (s *Saver) InvalidateLocalFile(ctx context.Context, provider, externalID string) error {
	changed, err := s.service.InvalidateLocalFile(ctx, provider, externalID)
	if err != nil {
		return err
	}
	if changed && s.worker != nil {
		s.worker.Wake()
	}
	return nil
}

func (s *Saver) autoDownloadEnabled(ctx context.Context) (bool, error) {
	if s.settings == nil {
		return false, nil
	}
	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		return false, err
	}
	return settings != nil && settings.AutoDownloadOnPlaylistAdd, nil
}

func (s *Saver) defaultQuality(ctx context.Context) string {
	if s.settings == nil {
		return domain.DefaultDownloadQuality
	}
	settings, err := s.settings.GetSettings(ctx)
	if err != nil || settings == nil {
		return domain.DefaultDownloadQuality
	}
	return domain.ResolveDownloadQuality(settings.DownloadQuality)
}

// requestArtist is the label artist stored with the snapshot: the first credit,
// or the raw tag value when relations were not loaded.
func requestArtist(track *domain.TrackDTO) string {
	if track == nil {
		return ""
	}
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			return strings.TrimSpace(artist.Name)
		}
	}
	return strings.TrimSpace(track.RawArtistNames)
}
