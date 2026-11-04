package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Download job states. Only queued/active block a second enqueue for the same
// track (enforced by a partial unique index); retries reuse the same row.
const (
	DownloadStateQueued   = "queued"
	DownloadStateActive   = "active"
	DownloadStateDone     = "done"
	DownloadStateFailed   = "failed"
	DownloadStateCanceled = "canceled"
)

// DefaultDownloadMaxAttempts is how many times a job is tried before it is
// reported as failed. retryable failures (429, timeouts) schedule a retry;
// permanent ones (403/404) fail immediately.
const DefaultDownloadMaxAttempts = 3

// Download file-name field order. The filename is "<title> - <artist>" or
// "<artist> - <title>"; there is no third option.
const (
	NamingOrderTitleArtist = "title-artist"
	NamingOrderArtistTitle = "artist-title"
	DefaultNamingOrder     = NamingOrderTitleArtist
)

// ResolveNamingOrder returns a known order, falling back to the default for an
// empty or unknown stored value. Reads use this so a hand-edited or migrated
// row can never produce an unnamed layout.
func ResolveNamingOrder(value string) string {
	if strings.TrimSpace(value) == NamingOrderArtistTitle {
		return NamingOrderArtistTitle
	}
	return NamingOrderTitleArtist
}

// NormalizeNamingOrder validates a value being saved; an empty value becomes the
// default so older clients that omit the field keep working.
func NormalizeNamingOrder(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", NamingOrderTitleArtist:
		return NamingOrderTitleArtist, nil
	case NamingOrderArtistTitle:
		return NamingOrderArtistTitle, nil
	default:
		return "", fmt.Errorf("download naming order must be %q or %q",
			NamingOrderTitleArtist, NamingOrderArtistTitle)
	}
}

// DownloadJob is one queued download of a remote track into the local library.
//
// Lifecycle: queued -> active -> done, or active -> failed -> (retry) queued.
// A job is canceled when the user removes it; deleting its playlist leaves the
// job running with PlaylistID cleared (FK ON DELETE SET NULL).
type DownloadJob struct {
	ID          string     `json:"id" db:"id"`
	TrackID     string     `json:"track_id" db:"track_id"`
	PlaylistID  *string    `json:"playlist_id,omitempty" db:"playlist_id"`
	Quality     string     `json:"quality" db:"quality"`
	State       string     `json:"state" db:"state"`
	Attempts    int        `json:"attempts" db:"attempts"`
	MaxAttempts int        `json:"max_attempts" db:"max_attempts"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty" db:"next_retry_at"`
	LastError   string     `json:"last_error,omitempty" db:"last_error"`
	BytesDone   int64      `json:"bytes_done" db:"bytes_done"`
	BytesTotal  int64      `json:"bytes_total" db:"bytes_total"`
	TargetPath  string     `json:"target_path,omitempty" db:"target_path"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty" db:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty" db:"finished_at"`
}

// IsActiveState reports whether the job currently occupies its track's active
// slot (queued or active), i.e. a second enqueue must not create a new row.
func IsActiveState(state string) bool {
	return state == DownloadStateQueued || state == DownloadStateActive
}

// CanTransitionTo validates the job state machine. Retrying a failed job goes
// back to 'queued' on the same row.
func CanTransitionTo(from, to string) bool {
	allowed := map[string][]string{
		DownloadStateQueued:   {DownloadStateActive, DownloadStateCanceled},
		DownloadStateActive:   {DownloadStateDone, DownloadStateFailed, DownloadStateCanceled},
		DownloadStateFailed:   {DownloadStateQueued, DownloadStateCanceled},
		DownloadStateDone:     nil,
		DownloadStateCanceled: nil,
	}
	for _, next := range allowed[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ErrDownloadAlreadyQueued is returned when the track already has a queued or
// active job (partial unique index idx_download_jobs_active).
var ErrDownloadAlreadyQueued = errors.New("download already queued for track")

// DownloadJobRepository persists the download queue.
type DownloadJobRepository interface {
	// Create inserts a job. It returns ErrDownloadAlreadyQueued when an active
	// job already exists for the track (partial unique index).
	Create(ctx context.Context, job *DownloadJob) error
	GetByID(ctx context.Context, id string) (*DownloadJob, error)
	GetActiveByTrackID(ctx context.Context, trackID string) (*DownloadJob, error)
	List(ctx context.Context) ([]*DownloadJob, error)
	// ClaimNext atomically moves the oldest due queued job to active.
	// It returns (nil, nil) when nothing is due.
	ClaimNext(ctx context.Context, now time.Time) (*DownloadJob, error)
	UpdateProgress(ctx context.Context, id string, bytesDone, bytesTotal int64) error
	MarkDone(ctx context.Context, id, targetPath string) error
	// MarkFailed records the failure. nextRetryAt == nil means "permanent";
	// otherwise the row returns to queued with next_retry_at set.
	MarkFailed(ctx context.Context, id, message string, nextRetryAt *time.Time) error
	Cancel(ctx context.Context, id string) error
	// ResetActiveToQueued recovers jobs interrupted by a crash/restart.
	ResetActiveToQueued(ctx context.Context) (int64, error)
	// DeleteByTrack removes every job that belonged to a track. Called when the
	// user deletes the track's file: the history describes a track that is gone.
	DeleteByTrack(ctx context.Context, trackID string) error
}

// Stream resolution errors. The download service maps them to retry policy:
// rate-limit and transport problems are retried, everything else is permanent
// (a retry would just burn the same quota again).
var (
	ErrStreamUnavailable = errors.New("stream unavailable")   // VIP / copyright / no url
	ErrStreamRateLimited = errors.New("stream rate limited")  // 429, back off and retry
	ErrStreamAuth        = errors.New("stream auth rejected") // 403, retrying cannot help
)

// ErrDownloadPermanent marks a download failure a retry cannot fix (403/404/410,
// an unsupported URL scheme). The downloader adapter wraps it.
var ErrDownloadPermanent = errors.New("download permanently failed")

// ErrCorruptDownload marks a file that did not survive validation (short body,
// wrong magic bytes, duration off by more than the tolerance). The partial file
// is discarded and a fresh attempt starts from byte zero, so retrying is only
// useful when the source itself changes.
var ErrCorruptDownload = errors.New("downloaded file failed validation")

// Download quality tiers. The value is stored on the job and on the setting; it
// selects the bitrate asked of the resolver and the extension of the file.
const (
	Quality128k = "128k"
	Quality320k = "320k"
	QualityFlac = "flac"
)

// DefaultDownloadQuality is used whenever an empty or unknown tier shows up.
const DefaultDownloadQuality = Quality320k

// ValidDownloadQualities lists the selectable tiers, best last.
var ValidDownloadQualities = []string{Quality128k, Quality320k, QualityFlac}

// IsValidDownloadQuality reports whether quality is one of the tiers above.
func IsValidDownloadQuality(quality string) bool {
	for _, q := range ValidDownloadQualities {
		if q == quality {
			return true
		}
	}
	return false
}

// ResolveDownloadQuality returns quality when it is valid and
// DefaultDownloadQuality otherwise, so an empty value never reaches the
// resolver (which would silently fall back to its own default).
func ResolveDownloadQuality(quality string) string {
	if IsValidDownloadQuality(quality) {
		return quality
	}
	return DefaultDownloadQuality
}

// StreamRef is a resolved, directly playable/downloadable audio URL.
//
// Expires is advisory only: URLs are short-lived and the downloader re-resolves
// before every attempt, so a stale StreamRef is never reused.
type StreamRef struct {
	URL     string
	Headers map[string]string
	Expires time.Time
}

// StreamResolver turns a remote track into a playable URL.
type StreamResolver interface {
	Resolve(ctx context.Context, track *TrackDTO, quality string) (StreamRef, error)
}

// --- ports the download pipeline is built from ------------------------------
//
// The app layer may not import infra (see AGENTS.md), and the adapters live in
// infra/downloader and infra/metadata. These narrow interfaces are the seam:
// main.go wires the concrete types onto the ports, the download service only
// ever sees the ports.

// DownloadTarget says where one download should land.
type DownloadTarget struct {
	// Dir is the directory the finished file is written to; the partial file
	// is staged in a .tmp subdirectory next to it.
	Dir string
	// Name is the finished file name including the extension.
	Name string
	// Quality selects the bitrate asked of the resolver.
	Quality string
	// ExpectedDuration is the catalogue duration, used to reject a file that
	// decoded to a different length. Zero disables the check.
	ExpectedDuration time.Duration
}

// DownloadResult reports a completed download.
type DownloadResult struct {
	// Path is the final file on disk.
	Path string
	// Container is the detected audio container ("mp3", "flac", ...).
	Container string
	// Bytes is the final file size.
	Bytes int64
}

// DownloadProgressFunc is called while bytes land on disk. done counts bytes
// of the final file (a resumed prefix included); total is 0 when the server
// announced no length.
type DownloadProgressFunc func(done, total int64)

// StreamDownloader moves a resolved stream onto disk and validates it. An
// implementation returns errors wrapping ErrPermanent for failures a retry
// cannot fix, ErrRateLimited when the server asked to slow down, and anything
// else (transport, truncation) for failures worth another attempt.
type StreamDownloader interface {
	Download(ctx context.Context, track *TrackDTO, ref StreamRef, target DownloadTarget, progress DownloadProgressFunc) (DownloadResult, error)
	// PartPath is the staging file for one target, so a failed attempt can be
	// discarded when it is not worth resuming.
	PartPath(dir, name string) string
	// TmpDir is the staging directory under a download root.
	TmpDir(dir string) string
}

// FileValidator re-reads a finished file to learn what it actually contains.
type FileValidator interface {
	// ValidateFile returns the detected container, or an error wrapping
	// ErrCorruptDownload when the bytes are not audio.
	ValidateFile(path string) (string, error)
}

// AudioPropertiesReader reports the decoded duration of a file. It is the
// purpose-built seam for the duration check inside the downloader: the taglib
// extractor implements it, so the check runs for real without the downloader
// depending on cgo.
type AudioPropertiesReader interface {
	Duration(path string) (time.Duration, error)
}

// ArtworkFetcher downloads cover art bytes for tagging. An implementation is
// expected to bound the size and reject non-image content types.
type ArtworkFetcher interface {
	Fetch(ctx context.Context, rawURL string) (data []byte, mimeType string, err error)
}

// TrackMetadataResult is what metadata extraction reports about a downloaded
// file: the fields the tagger writes back and the duration the container
// header claims.
type TrackMetadataResult struct {
	Title           string
	Artist          string
	Album           string
	AlbumArtist     string
	Year            int
	TrackNumber     int
	TotalTracks     int
	DurationSeconds int
	Bitrate         int
}

// TrackTagger stamps tags onto a downloaded file and reads back what the
// finished file says about itself.
type TrackTagger interface {
	Tag(ctx context.Context, path string, update MetadataUpdate) error
	Read(ctx context.Context, path string) (TrackMetadataResult, error)
}

// PlaylistMembership is one playlist entry of a track, with the LexoRank
// position it holds. The merge step reuses the position so a song that finishes
// downloading keeps its place in the playlist instead of jumping to the top.
type PlaylistMembership struct {
	PlaylistID string
	Position   string
}

// DownloadMergeRepository is the narrow persistence surface the merge step needs
// on top of TrackRepository. It lives in its own port because download-specific
// SQL (remote identity lookup, playlist position transfer, play-count folding)
// should not widen TrackRepository for every other caller.
type DownloadMergeRepository interface {
	// TrackByProviderExternalID finds the row for one remote identity and
	// returns nil when there is none.
	TrackByProviderExternalID(ctx context.Context, provider, externalID string) (*TrackDTO, error)
	// PlaylistMemberships lists the playlists holding trackID, with positions.
	PlaylistMemberships(ctx context.Context, trackID string) ([]PlaylistMembership, error)
	// EnsurePlaylistTrack adds trackID to playlistID unless it is already a
	// member, keeping position when it is non-empty.
	EnsurePlaylistTrack(ctx context.Context, playlistID, trackID, position string) error
	// AddPlayCount increases a row's play count by delta.
	AddPlayCount(ctx context.Context, trackID string, delta int) error
	// CarryIdentity moves an online identity onto another row.
	//
	// It is used when the remote row is folded away: the row that owns the file
	// must also own the identity, or a later download of the same song would
	// register a second remote row that can never resolve to a file.
	//
	// The caller must have deleted the row that previously held the identity
	// first: idx_tracks_provider_external is UNIQUE.
	CarryIdentity(ctx context.Context, trackID, provider, externalID string) error
	// MoveLyrics moves the lyric row from one track to another. A download
	// imports the file and only then folds the remote row away; lyrics fetched
	// before that point would otherwise be cascade-deleted with the remote row.
	// An existing lyric on the destination wins.
	MoveLyrics(ctx context.Context, fromTrackID, toTrackID string) error
}

// --- retry policy -----------------------------------------------------------

// RetryPolicy is how one failed attempt is turned into a follow-up. It lives in
// domain so the app layer can decide without importing the infra adapters that
// produce the errors, and so a test can assert the classification once.
type RetryPolicy struct {
	// Retryable reports whether another attempt is worth scheduling.
	Retryable bool
	// Delay is how long to wait before that attempt. Longer for a rate limit,
	// because retrying a 429 immediately just spends more quota.
	Delay time.Duration
}

// NoRetry is the policy for a permanent failure.
var NoRetry = RetryPolicy{Retryable: false}

// Backoff delays for the three attempts of a default job. A rate-limited source
// starts at a full minute instead (see ClassifyDownloadError).
var DefaultRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}

// RateLimitRetryDelay is the first backoff for a 429.
const RateLimitRetryDelay = 60 * time.Second

// ClassifyDownloadError maps a failure to its retry policy. attempts is how many
// attempts the job has already used *excluding* the one that just failed, i.e.
// DownloadJob.Attempts before MarkFailed increments it; the delay escalates per
// attempt and maxAttempts caps the total.
//
// Classification rules, in order:
//   - a rate limit is always retryable, starting at RateLimitRetryDelay;
//   - VIP/copyright/403/404 and any other permanent error never retry;
//   - transport errors, truncation and an unreadable stream retry;
//   - anything unrecognised retries, because a download that gave up for an
//     unknown reason is more annoying than one that tried again.
func ClassifyDownloadError(err error, attempts, maxAttempts int) RetryPolicy {
	if err == nil {
		return NoRetry
	}
	if maxAttempts <= 0 {
		maxAttempts = DefaultDownloadMaxAttempts
	}
	if attempts+1 >= maxAttempts {
		// The attempt that just failed was the last one allowed.
		return NoRetry
	}
	if errors.Is(err, ErrStreamRateLimited) {
		// Every extra rate-limited attempt doubles the wait.
		return RetryPolicy{Retryable: true, Delay: RateLimitRetryDelay * time.Duration(1<<attempts)}
	}
	if errors.Is(err, ErrStreamUnavailable) || errors.Is(err, ErrStreamAuth) ||
		errors.Is(err, ErrCorruptDownload) || errors.Is(err, ErrDownloadPermanent) {
		return NoRetry
	}
	return RetryPolicy{Retryable: true, Delay: retryDelay(attempts)}
}

// retryDelay returns the backoff for the next attempt, holding the last delay
// for any attempt past the table.
func retryDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return DefaultRetryDelays[0]
	}
	if attempts >= len(DefaultRetryDelays) {
		return DefaultRetryDelays[len(DefaultRetryDelays)-1]
	}
	return DefaultRetryDelays[attempts]
}

// DownloadContent is what the pipeline fetches and writes next to the audio file.
//
// It is the resolved form of the three download_content prefs: the settings hold
// them independently (each one is a request the user pays for), and this struct
// is what a caller acts on, with the one contradiction already collapsed.
type DownloadContent struct {
	// Lyrics asks the finalize step to fetch and store lyrics for the new track.
	Lyrics bool
	// LyricTranslation keeps the translation the source returned next to the
	// original. It is false whenever Lyrics is false: a translation of lyrics
	// that were never fetched cannot exist, and pretending otherwise would make
	// the UI promise something the pipeline never delivers.
	LyricTranslation bool
	// Cover embeds the cover image into the file's tags.
	Cover bool
}

// ResolveDownloadContent collapses the stored prefs into what actually happens.
//
// It exists so the rule "translation implies lyrics" lives in one place: the
// settings page can show the dependent switch dimmed, the pipeline can ask one
// question, and neither has to re-derive the rule.
func ResolveDownloadContent(settings *AppSettings) DownloadContent {
	if settings == nil {
		return DownloadContent{}
	}
	content := DownloadContent{
		Lyrics: settings.DownloadWithLyrics,
		Cover:  settings.DownloadWithCover,
	}
	content.LyricTranslation = settings.DownloadWithLyrics && settings.DownloadWithLyricTranslation
	return content
}
