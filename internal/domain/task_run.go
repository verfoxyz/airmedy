package domain

import "time"

// Task kinds and states as the tasks page renders them. The page groups rows by
// State and labels them with Kind; a media request is the authoritative kind,
// while a stream-cache session is the only remaining execution-side row.
const (
	TaskKindRequest = "request"
	TaskKindCache   = "cache"

	// TaskStateActive covers every state that still has work in flight:
	// wanted/queued/running requests and active cache sessions.
	TaskStateActive = "active"
	// TaskStateWaiting is a failed request with a scheduled retry. It is not
	// finished, but it is not downloading either; the page says "waiting to
	// retry" instead of leaving the row looking stuck.
	TaskStateWaiting  = "waiting"
	TaskStateDone     = "done"
	TaskStateFailed   = "failed"
	TaskStateCanceled = "canceled"
)

// TaskRun is one media request or stream-cache session, normalised for the
// tasks page.
//
// The tasks page derives its rows from media_requests, so download jobs and
// promotions are execution details and no longer listed separately. Phase is a
// MediaRequestPhase* value while the request runs (resolving/downloading/
// promoting/verifying) and empty otherwise; the UI uses it as the row's status
// text. Title and Artist are resolved from the request's current track row.
type TaskRun struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Phase   string `json:"phase,omitempty"`
	TrackID string `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Quality string `json:"quality"`
	Error   string `json:"error"`
	// RequestID is the stable key of a media request row; it survives a merge
	// that replaces TrackID. Empty for stream-cache sessions.
	RequestID string `json:"request_id"`
	// Provider / ExternalID name the request's identity, so the page can merge
	// rows by identity instead of by the changing track id.
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
