package wails

import (
	"context"

	"airmedy/internal/app/download"
	"airmedy/internal/domain"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Event names the download pipeline emits. The frontend subscribes by these
// strings; keep them in sync with frontend/src/stores/download.ts.
const (
	EventDownloadProgress = "download:progress"
	EventDownloadDone     = "download:done"
	EventDownloadFailed   = "download:failed"
	// EventTaskUpdated announces a promotion-history change. The tasks page
	// reloads; the payload is empty because it reads the list itself.
	EventTaskUpdated = "task:updated"
	// EventTrackMerged announces that a remote row was folded into a local one.
	// The player remaps its current track and queue from the payload.
	EventTrackMerged = "track:merged"
)

// DownloadProgressEvent is what the UI needs for one progress ring. It is a
// flat, small payload on purpose: progress fires per second per job, and the
// frontend only needs the numbers, not the whole row.
type DownloadProgressEvent struct {
	JobID      string `json:"job_id"`
	TrackID    string `json:"track_id"`
	PlaylistID string `json:"playlist_id,omitempty"`
	State      string `json:"state"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	// Percent is precomputed so a ring does not divide by zero on its own.
	Percent int    `json:"percent"`
	Error   string `json:"error,omitempty"`
}

// DownloadService is the Wails binding for the download queue: translation
// only, no logic (see AGENTS.md §3).
type DownloadService struct {
	svc *download.Service
}

// NewDownloadService installs itself as the event sink, so the pipeline is
// constructed before the window and events are simply dropped until one exists.
func NewDownloadService(svc *download.Service) *DownloadService {
	s := &DownloadService{svc: svc}
	// The sink wrapper keeps TaskUpdated off the Wails method surface: it is an
	// event payload trigger, not something the frontend calls.
	svc.SetEventSink(downloadEventSink{DownloadService: s})
	return s
}

// DownloadProgress implements download.EventSink.
func (s *DownloadService) DownloadProgress(job *domain.DownloadJob) {
	emitDownloadEvent(EventDownloadProgress, job)
}

// DownloadDone implements download.EventSink.
func (s *DownloadService) DownloadDone(job *domain.DownloadJob) {
	emitDownloadEvent(EventDownloadDone, job)
}

// DownloadFailed implements download.EventSink. It covers both a scheduled
// retry (state queued, error set) and a permanent failure, so the UI can show
// either without a second event type.
func (s *DownloadService) DownloadFailed(job *domain.DownloadJob) {
	emitDownloadEvent(EventDownloadFailed, job)
}

// downloadEventSink adds the promotion notification without widening the
// service's public (bound) methods.
type downloadEventSink struct {
	*DownloadService
}

// TaskUpdated implements download.EventSink: a promotion task changed state, so
// the tasks page reloads its list.
func (downloadEventSink) TaskUpdated() {
	app := application.Get()
	if app == nil || app.Event == nil {
		return
	}
	app.Event.Emit(EventTaskUpdated, nil)
}

// TrackMerged implements download.EventSink: the player replaces the deleted
// remote row with the survivor before touching playback state again.
func (downloadEventSink) TrackMerged(fromTrackID, toTrackID string) {
	app := application.Get()
	if app == nil || app.Event == nil {
		return
	}
	app.Event.Emit(EventTrackMerged, map[string]string{
		"from_track_id": fromTrackID,
		"to_track_id":   toTrackID,
	})
}

func emitDownloadEvent(name string, job *domain.DownloadJob) {
	app := application.Get()
	if app == nil || app.Event == nil {
		return
	}
	app.Event.Emit(name, toProgressEvent(job))
}

func toProgressEvent(job *domain.DownloadJob) DownloadProgressEvent {
	event := DownloadProgressEvent{
		JobID:      job.ID,
		TrackID:    job.TrackID,
		PlaylistID: deref(job.PlaylistID),
		State:      job.State,
		BytesDone:  job.BytesDone,
		BytesTotal: job.BytesTotal,
		Error:      job.LastError,
	}
	if job.BytesTotal > 0 {
		percent := int(job.BytesDone * 100 / job.BytesTotal)
		if percent > 100 {
			percent = 100
		}
		event.Percent = percent
	}
	return event
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// EnqueueDownload queues a remote track for download.
func (s *DownloadService) EnqueueDownload(trackID, playlistID string) error {
	return s.svc.Enqueue(context.Background(), trackID, playlistID, "")
}

// CancelDownload stops a queued or active job.
func (s *DownloadService) CancelDownload(jobID string) error {
	return s.svc.Cancel(context.Background(), jobID)
}

// RetryDownload re-runs a failed or canceled job.
func (s *DownloadService) RetryDownload(jobID string) error {
	return s.svc.Retry(context.Background(), jobID)
}

// RedownloadTrack deletes a finished file and fetches it again, for a file an
// older build damaged while tagging. The user must trigger it explicitly.
func (s *DownloadService) RedownloadTrack(trackID string) error {
	return s.svc.Redownload(context.Background(), trackID)
}

// ListDownloads returns the queue, newest last.
func (s *DownloadService) ListDownloads() ([]*domain.DownloadJob, error) {
	return s.svc.List(context.Background())
}

// ListTasks returns downloads and promotions merged for the tasks page. Rows
// are newest first; the page groups them by state.
func (s *DownloadService) ListTasks() ([]domain.TaskRun, error) {
	return s.svc.ListTasks(context.Background())
}

// ClearTaskHistory deletes finished downloads and promotions. Active work is
// untouched; the tasks page reloads when task:updated arrives.
func (s *DownloadService) ClearTaskHistory() error {
	return s.svc.ClearTaskHistory(context.Background())
}
