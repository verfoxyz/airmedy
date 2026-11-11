package domain

import "time"

// Task kinds and states as the tasks page renders them. A download and a
// promotion are merged into one list; the page groups by State and labels the
// row with Kind.
const (
	TaskKindDownload = "download"
	TaskKindPromote  = "promote"

	// TaskStateActive covers every state that still has work in flight:
	// queued/active downloads and queued/running promotions.
	TaskStateActive   = "active"
	TaskStateDone     = "done"
	TaskStateFailed   = "failed"
	TaskStateCanceled = "canceled"
)

// TaskRun is one download or promotion, normalised for the tasks page.
//
// The two tables keep their own state names and columns; TaskRun is the read
// model that lets the UI render both without knowing which table a row came
// from. Title and Artist are snapshots: a promotion stores them at request time,
// and a download resolves them from its track while that row still exists.
type TaskRun struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	TrackID string `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Quality string `json:"quality"`
	Error   string `json:"error"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
