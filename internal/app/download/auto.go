package download

import (
	"context"

	"airmedy/internal/domain"
)

// ShouldAutoDownload decides whether adding a track to a playlist should queue
// a download. It is a pure decision so the playlist hook, the Wails layer and
// the tests all agree on it.
//
// The track must be remote (a local file needs nothing) and the setting must be
// on. A track that already has an active job needs nothing either: adding it to
// a second playlist before the first download finished must not start a race.
func ShouldAutoDownload(settings *domain.AppSettings, track *domain.TrackDTO, activeJob *domain.DownloadJob) bool {
	if settings == nil || !settings.AutoDownloadOnPlaylistAdd {
		return false
	}
	if track == nil || !track.IsRemote() {
		return false
	}
	return activeJob == nil
}

// AutoDownloadHook builds the function PlaylistService calls when a track is
// added to a playlist. It consults the settings and the queue, then enqueues.
//
// A failure is logged rather than returned: the playlist edit already
// succeeded, and the user can start the download by hand.
func (s *Service) AutoDownloadHook(ctx context.Context, track *domain.TrackDTO, playlistID string) {
	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		s.logger.Warn("download: cannot read settings for auto-download", "error", err)
		return
	}
	active, err := s.jobs.GetActiveByTrackID(ctx, track.ID)
	if err != nil {
		s.logger.Warn("download: cannot check for an active job", "track_id", track.ID, "error", err)
		return
	}
	if !ShouldAutoDownload(settings, track, active) {
		return
	}
	if err := s.Enqueue(ctx, track.ID, playlistID, settings.DownloadQuality); err != nil {
		// A second playlist add that lost the race is the expected error here.
		s.logger.Info("download: auto-download not queued", "track_id", track.ID, "error", err)
	}
}
