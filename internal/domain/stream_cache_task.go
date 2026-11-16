package domain

import (
	"context"
	"time"
)

// Stream cache task states. active is one download in flight; done and failed
// are history for the tasks page. canceled marks an intentional stop (track
// switch, shutdown, user stop): nothing went wrong, so it is not an error.
const (
	StreamCacheTaskStateActive   = "active"
	StreamCacheTaskStateDone     = "done"
	StreamCacheTaskStateFailed   = "failed"
	StreamCacheTaskStateCanceled = "canceled"
)

// StreamCacheTask is one actual stream-cache download session (a cache hit
// creates none). It exists so the tasks page shows caching alongside downloads
// and promotions; the row keeps the track identity and byte counters so a
// session can be finished or failed after the playback context is gone.
type StreamCacheTask struct {
	ID         string `db:"id"`
	Provider   string `db:"provider"`
	ExternalID string `db:"external_id"`
	Title      string `db:"title"`
	Artist     string `db:"artist"`
	Quality    string `db:"quality"`
	State      string `db:"state"`
	BytesTotal int64  `db:"bytes_total"`
	BytesDone  int64  `db:"bytes_done"`
	Error      string `db:"error"`
	// StartedAt is the session start; the tasks page uses it as CreatedAt.
	StartedAt  time.Time  `db:"started_at"`
	FinishedAt *time.Time `db:"finished_at"`
}

// StreamCacheTaskRepository persists the stream cache's task history.
//
// Unlike the cache entry itself, a row here is per session: playing the same
// song twice leaves two rows, and only finished rows are trimmed.
type StreamCacheTaskRepository interface {
	// Start inserts one active session row. ID and StartedAt are assigned by
	// the caller.
	Start(ctx context.Context, task *StreamCacheTask) error
	// Finish closes a session with its final byte counters.
	Finish(ctx context.Context, id string, totalBytes, doneBytes int64, at time.Time) error
	// Fail closes a session as failed with the reason.
	Fail(ctx context.Context, id, message string, totalBytes, doneBytes int64, at time.Time) error
	// Cancel closes a session that was stopped on purpose (track switch,
	// shutdown, user stop). It records no error: the tasks page must not show it
	// as a failure.
	Cancel(ctx context.Context, id string, totalBytes, doneBytes int64, at time.Time) error
	// MarkActiveInterrupted closes every active row as canceled (shutdown or a
	// crash left them running) and returns how many rows changed.
	MarkActiveInterrupted(ctx context.Context, message string, at time.Time) (int64, error)
	// List returns every session, newest first, for the tasks page.
	List(ctx context.Context) ([]*StreamCacheTask, error)
	// TrimFinished deletes done/failed/canceled rows, keeping the newest keep
	// rows. keep == 0 deletes all history; active rows are never touched.
	TrimFinished(ctx context.Context, keep int) (int64, error)
}
