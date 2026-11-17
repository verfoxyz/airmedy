package domain

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrMediaRequestDeferred means the executor cannot run this round because
// playback still owns the cache file. The request stays wanted and the worker
// retries after the session-close wake instead of counting a failure.
var ErrMediaRequestDeferred = errors.New("media request deferred")

// Media request intent. It records what the user asked for, not what the
// executor happens to do: a promotion request still downloads when no cache
// exists, and a download request promotes a cached copy that already meets the
// wanted quality.
const (
	MediaRequestDesiredDownload = "download"
	MediaRequestDesiredPromote  = "promote"
)

// Media request states. wanted/queued/running are unfinished; done, failed and
// canceled are terminal, and a new save may reopen failed/canceled.
const (
	MediaRequestStateWanted   = "wanted"
	MediaRequestStateQueued   = "queued"
	MediaRequestStateRunning  = "running"
	MediaRequestStateDone     = "done"
	MediaRequestStateFailed   = "failed"
	MediaRequestStateCanceled = "canceled"
)

// Running phases. Empty outside running.
const (
	MediaRequestPhaseResolving   = "resolving"
	MediaRequestPhaseDownloading = "downloading"
	MediaRequestPhasePromoting   = "promoting"
	MediaRequestPhaseVerifying   = "verifying"
)

// MediaRequest is the single authoritative record of "the user wants this
// online track in the library".
//
// UNIQUE(provider, external_id) is the idempotency boundary: every save entry
// point upserts here instead of creating its own task, so repeated favorites,
// playlist adds and re-saves converge on one row. Executors (download jobs and
// cached-copy promotion) are details this row drives.
type MediaRequest struct {
	ID         string `db:"id" json:"id"`
	Provider   string `db:"provider" json:"provider"`
	ExternalID string `db:"external_id" json:"external_id"`
	// TrackID is the row the identity currently resolves to. It may be replaced
	// by a merge; the worker re-resolves the identity instead of trusting it.
	TrackID string `db:"track_id" json:"track_id"`
	// Title and Artist are the row's identity snapshot at request time. A merge
	// can delete that row, so the tasks page must not depend on it for its label.
	Title         string     `db:"title" json:"title"`
	Artist        string     `db:"artist" json:"artist"`
	Desired       string     `db:"desired" json:"desired"`
	Quality       string     `db:"quality" json:"quality"`
	State         string     `db:"state" json:"state"`
	Phase         string     `db:"phase" json:"phase"`
	Attempts      int        `db:"attempts" json:"attempts"`
	LastError     string     `db:"last_error" json:"last_error"`
	NextAttemptAt *time.Time `db:"next_attempt_at" json:"next_attempt_at,omitempty"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
	FinishedAt    *time.Time `db:"finished_at" json:"finished_at,omitempty"`
}

// MediaRequestMatchKey is the song identity two requests share across
// providers: normalised artist + NUL + normalised title, the same shape the
// recommendation match cache uses. Empty when either half is unknown, so an
// old row without a snapshot never matches the whole table.
func MediaRequestMatchKey(title, artist string) string {
	titleKey := NormalizationKey(strings.TrimSpace(title))
	artistKey := NormalizationKey(strings.TrimSpace(artist))
	if titleKey == "" || artistKey == "" {
		return ""
	}
	return artistKey + "\x00" + titleKey
}

// MediaRequestEvent is one state/phase transition, kept for the tasks page's
// history detail. It is append-only and never drives behaviour.
type MediaRequestEvent struct {
	ID        string    `db:"id"`
	RequestID string    `db:"request_id"`
	State     string    `db:"state"`
	Phase     string    `db:"phase"`
	Detail    string    `db:"detail"`
	CreatedAt time.Time `db:"created_at"`
}

// MediaRequestRepository persists requests and their transition history.
type MediaRequestRepository interface {
	// Ensure inserts the request unless the identity already has one. It never
	// changes an existing row, so backfill cannot clobber a live request.
	Ensure(ctx context.Context, request *MediaRequest) error
	GetByID(ctx context.Context, id string) (*MediaRequest, error)
	GetByIdentity(ctx context.Context, provider, externalID string) (*MediaRequest, error)
	List(ctx context.Context) ([]*MediaRequest, error)
	// ListActive returns rows the worker must look at: wanted/queued/running,
	// plus failed rows whose retry is due (next_attempt_at <= now).
	ListActive(ctx context.Context, now time.Time) ([]*MediaRequest, error)
	// ListOrphans returns failed rows whose track_id no longer exists. A merge
	// or delete made them permanently unresolvable, so the startup scan settles
	// them without waiting for their retry backoff.
	ListOrphans(ctx context.Context) ([]*MediaRequest, error)
	// UpdateIntent records a new save without touching a live run. reset moves
	// a terminal row back to wanted (attempts and error cleared).
	UpdateIntent(ctx context.Context, id, desired, quality, trackID string, reset bool, at time.Time) error
	MarkRunning(ctx context.Context, id, phase, trackID string, at time.Time) error
	MarkDone(ctx context.Context, id, trackID string, at time.Time) error
	// ReopenDone moves a finished row back to wanted without changing its
	// intent, attempts or quality. Used when the local file the request produced
	// is gone: the next save must run instead of being treated as a no-op.
	ReopenDone(ctx context.Context, id string, at time.Time) error
	MarkFailed(ctx context.Context, id, message string, attempts int, next *time.Time, at time.Time) error
	MarkCanceled(ctx context.Context, id string, at time.Time) error
	RecordEvent(ctx context.Context, event *MediaRequestEvent) error
	// TrimFinished deletes finished history, keeping the newest keep rows.
	// keep == 0 deletes every finished row; live states are never touched.
	TrimFinished(ctx context.Context, keep int) (int64, error)
}

// MediaRequestExecutor is the execution detail behind a request: the download
// pipeline owns cache inspection, quality comparison, promotion and job
// creation. The worker never moves files itself.
type MediaRequestExecutor interface {
	// LocateTrack returns the row currently registered for the identity (remote
	// or local), or nil when the identity has no row.
	LocateTrack(ctx context.Context, provider, externalID string) (*TrackDTO, error)
	// Execute starts the work and reports the phase the request entered.
	// A complete cache at sufficient quality promotes; otherwise it downloads.
	// ErrDownloadAlreadyQueued is success (work is already in flight).
	Execute(ctx context.Context, track *TrackDTO, desired, quality string) (phase string, err error)
	// Status reports a failed delegated run started after the given instant.
	// An empty message means the work is still running or finished successfully.
	Status(ctx context.Context, track *TrackDTO, since time.Time) (failure string, err error)
}
