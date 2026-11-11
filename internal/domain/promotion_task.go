package domain

import (
	"context"
	"time"
)

// Promotion task states. queued and running are the single active row an entry
// may have; done and failed are history.
const (
	PromotionStateQueued  = "queued"
	PromotionStateRunning = "running"
	PromotionStateDone    = "done"
	PromotionStateFailed  = "failed"
)

// PromotionTask is one request to turn a streamed row into a library file.
//
// It exists because promotion moves the cache file and therefore has to wait
// until no playback session owns it: the request must survive a restart, and
// the download/tasks page reads this table as its history.
type PromotionTask struct {
	ID         string `db:"id"`
	TrackID    string `db:"track_id"`
	Provider   string `db:"provider"`
	ExternalID string `db:"external_id"`
	// Title and Artist are the values at request time; a later rename does not
	// backfill the history row.
	Title      string     `db:"title"`
	Artist     string     `db:"artist"`
	State      string     `db:"state"`
	Quality    string     `db:"quality"`
	Error      string     `db:"error"`
	CreatedAt  time.Time  `db:"created_at"`
	StartedAt  *time.Time `db:"started_at"`
	FinishedAt *time.Time `db:"finished_at"`
}

// PromotionTaskRepository persists promotion requests and their history.
type PromotionTaskRepository interface {
	// EnsureQueued inserts a queued row, or returns the id of the existing
	// queued/running row for the same identity so a repeated request reuses it.
	EnsureQueued(ctx context.Context, task *PromotionTask) (string, error)
	// MarkRunning records that the worker started executing the row.
	MarkRunning(ctx context.Context, id string, at time.Time) error
	// MarkFinished moves the row to done or failed.
	MarkFinished(ctx context.Context, id, state, message string, at time.Time) error
	// ActivePromotions reports which of the given identities have a queued or
	// running promotion. The search-row resolver merges this with active
	// downloads so the UI shows one task state.
	ActivePromotions(ctx context.Context, identities []CatalogIdentity) (map[CatalogIdentity]bool, error)
	// List returns every promotion run, newest first, for the tasks page.
	List(ctx context.Context) ([]*PromotionTask, error)
	// TrimFinished deletes done/failed rows, keeping the newest keep rows.
	// keep == 0 deletes all history; queued/running rows are never touched.
	TrimFinished(ctx context.Context, keep int) (int64, error)
}
