package lastfm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/google/uuid"
)

// --- Fakes -------------------------------------------------------------------

type fakeScrobbleRepo struct {
	mu sync.Mutex

	rows  map[string]domain.PendingScrobble
	order []string

	enqueuedIDs []string
	deletedIDs  []string
	ignoredIDs  []string
	cleanup     []cleanupRequest

	dueSignal chan struct{}
}

type cleanupRequest struct {
	olderThan   time.Time
	maxAttempts int
	keep        int
}

func newFakeScrobbleRepo() *fakeScrobbleRepo {
	return &fakeScrobbleRepo{
		rows:      make(map[string]domain.PendingScrobble),
		dueSignal: make(chan struct{}, 16),
	}
}

func (f *fakeScrobbleRepo) Enqueue(_ context.Context, scrobble *domain.PendingScrobble) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := *scrobble
	if entry.ID == "" {
		entry.ID = uuid.NewString()
	}
	if f.rows[entry.ID].ID != "" {
		return nil
	}
	if entry.StartedAt < 0 {
		entry.StartedAt = 0
	}
	// The real repository stamps a new row due immediately.
	due := time.Unix(0, 0)
	entry.NextRetryAt = &due
	f.rows[entry.ID] = entry
	f.order = append(f.order, entry.ID)
	f.enqueuedIDs = append(f.enqueuedIDs, entry.ID)
	return nil
}

func (f *fakeScrobbleRepo) Due(
	_ context.Context, now time.Time, maxAttempts, limit int,
) ([]domain.PendingScrobble, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	result := make([]domain.PendingScrobble, 0, len(f.rows))
	for _, id := range f.order {
		entry, ok := f.rows[id]
		if !ok || entry.Attempts >= maxAttempts || entry.NextRetryAt == nil {
			continue
		}
		if entry.NextRetryAt.After(now) {
			continue
		}
		result = append(result, entry)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].StartedAt < result[j].StartedAt })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	select {
	case f.dueSignal <- struct{}{}:
	default:
	}
	return result, nil
}

func (f *fakeScrobbleRepo) Delete(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.rows, id)
		f.deletedIDs = append(f.deletedIDs, id)
	}
	return nil
}

func (f *fakeScrobbleRepo) MarkFailure(_ context.Context, id, message string, nextRetry time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.rows[id]
	entry.Attempts++
	entry.LastError = message
	next := nextRetry
	entry.NextRetryAt = &next
	f.rows[id] = entry
	return nil
}

func (f *fakeScrobbleRepo) MarkIgnored(_ context.Context, id, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.rows[id]
	entry.LastError = message
	entry.NextRetryAt = nil
	f.rows[id] = entry
	f.ignoredIDs = append(f.ignoredIDs, id)
	return nil
}

func (f *fakeScrobbleRepo) Cleanup(
	_ context.Context, olderThan time.Time, maxAttempts, keep int,
) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanup = append(f.cleanup, cleanupRequest{olderThan: olderThan, maxAttempts: maxAttempts, keep: keep})
	return 0, nil
}

func (f *fakeScrobbleRepo) row(id string) domain.PendingScrobble {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id]
}

func (f *fakeScrobbleRepo) onlyID(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.order) != 1 {
		t.Fatalf("rows = %d, want one", len(f.order))
	}
	return f.order[0]
}

// --- Test service ------------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

var scrobbleTestNow = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func newScrobbleTestService(
	repo domain.PendingScrobbleRepository, transport http.RoundTripper,
) *LastFmService {
	return &LastFmService{
		logger:       slog.Default(),
		apiKey:       "key",
		apiSecret:    "secret",
		sessionKey:   "session",
		username:     "listener",
		http:         &http.Client{Transport: transport},
		limiter:      newRateLimiter(1, 0),
		scrobbles:    repo,
		scrobbleWake: make(chan struct{}, 1),
		now:          func() time.Time { return scrobbleTestNow },
	}
}

func scrobbleTrack(title string) *domain.TrackDTO {
	return &domain.TrackDTO{
		Track:   domain.Track{Title: title, Duration: 200},
		Artists: []*domain.Artist{{Name: "Artist"}},
	}
}

func jsonResponse(status int, body string, header map[string]string) *http.Response {
	values := http.Header{}
	values.Set("Content-Type", "application/json")
	for key, value := range header {
		values.Set(key, value)
	}
	return &http.Response{
		StatusCode: status,
		Header:     values,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func scrobbleEntryJSON(artist, title string, timestamp int64, code int) string {
	return fmt.Sprintf(
		`{"artist":{"#text":%q},"track":{"#text":%q},"timestamp":"%d","ignoredMessage":{"code":"%d","#text":""}}`,
		artist, title, timestamp, code,
	)
}

func acceptedBody(entries ...string) string {
	return `{"scrobbles":{"scrobble":[` + strings.Join(entries, ",") +
		`],"@attr":{"accepted":"1","ignored":"0"}}}`
}

// --- Tests -------------------------------------------------------------------

func TestScrobbleIsQueuedBeforeTheRequest(t *testing.T) {
	repo := newFakeScrobbleRepo()
	requests := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return jsonResponse(http.StatusOK, acceptedBody(), nil), nil
	})
	service := newScrobbleTestService(repo, transport)

	service.handleScrobble(scrobbleTrack("Song"), time.Unix(1000, 0))

	repo.mu.Lock()
	enqueued := len(repo.enqueuedIDs)
	repo.mu.Unlock()
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want the play in the durable queue", enqueued)
	}
	if requests != 0 {
		t.Fatal("handleScrobble must not block on the network")
	}
}

func TestScrobbleFailureKeepsTheRowAndSchedulesRetry(t *testing.T) {
	repo := newFakeScrobbleRepo()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("network down")
	})
	service := newScrobbleTestService(repo, transport)

	service.handleScrobble(scrobbleTrack("Song"), time.Unix(1000, 0))
	id := repo.onlyID(t)

	service.flushPendingScrobbles(context.Background())

	entry := repo.row(id)
	if entry.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", entry.Attempts)
	}
	if entry.LastError == "" {
		t.Fatal("the failure reason was not recorded")
	}
	if entry.NextRetryAt == nil || !entry.NextRetryAt.Equal(scrobbleTestNow.Add(time.Minute)) {
		t.Fatalf("next retry = %v, want now + 1m", entry.NextRetryAt)
	}
	if len(repo.deletedIDs) != 0 {
		t.Fatal("a failed scrobble must stay queued")
	}
}

func TestScrobbleRetrySuccessDeletesTheRow(t *testing.T) {
	repo := newFakeScrobbleRepo()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK,
			acceptedBody(scrobbleEntryJSON("Artist", "Song", 1000, 0)), nil), nil
	})
	service := newScrobbleTestService(repo, transport)

	service.handleScrobble(scrobbleTrack("Song"), time.Unix(1000, 0))
	id := repo.onlyID(t)

	service.flushPendingScrobbles(context.Background())

	repo.mu.Lock()
	deleted := len(repo.deletedIDs)
	repo.mu.Unlock()
	if deleted != 1 {
		t.Fatalf("deleted = %d, want the accepted row", deleted)
	}
	if entry := repo.row(id); entry.ID != "" {
		t.Fatalf("row = %+v, want it gone", entry)
	}
}

func TestScrobbleBackoffSequence(t *testing.T) {
	want := []time.Duration{
		time.Minute,
		5 * time.Minute,
		30 * time.Minute,
		2 * time.Hour,
		6 * time.Hour,
		6 * time.Hour,
	}
	for index, expected := range want {
		if got := backoffFor(index + 1); got != expected {
			t.Fatalf("backoffFor(%d) = %s, want %s", index+1, got, expected)
		}
	}
}

func TestScrobbleRateLimitUsesRetryAfter(t *testing.T) {
	repo := newFakeScrobbleRepo()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, "rate limited", map[string]string{
			"Retry-After": "120",
		}), nil
	})
	service := newScrobbleTestService(repo, transport)

	service.handleScrobble(scrobbleTrack("Song"), time.Unix(1000, 0))
	id := repo.onlyID(t)

	service.flushPendingScrobbles(context.Background())

	entry := repo.row(id)
	if entry.NextRetryAt == nil || !entry.NextRetryAt.Equal(scrobbleTestNow.Add(2*time.Minute)) {
		t.Fatalf("next retry = %v, want the Retry-After window (2m)", entry.NextRetryAt)
	}
}

func TestScrobbleBatchIsFiftyOldestFirst(t *testing.T) {
	repo := newFakeScrobbleRepo()
	var forms []url.Values
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			return nil, err
		}
		forms = append(forms, request.PostForm)
		return jsonResponse(http.StatusOK, acceptedBody(), nil), nil
	})
	service := newScrobbleTestService(repo, transport)

	for index := 1; index <= 60; index++ {
		service.handleScrobble(scrobbleTrack(fmt.Sprintf("Song %d", index)), time.Unix(int64(index), 0))
	}

	service.flushPendingScrobbles(context.Background())
	if len(forms) != 1 {
		t.Fatalf("requests = %d, want one batch", len(forms))
	}
	form := forms[0]
	for index := 0; index < scrobbleBatchLimit; index++ {
		if form.Get(fmt.Sprintf("artist[%d]", index)) != "Artist" {
			t.Fatalf("batch is missing artist[%d]", index)
		}
		if got := form.Get(fmt.Sprintf("timestamp[%d]", index)); got != fmt.Sprintf("%d", index+1) {
			t.Fatalf("timestamp[%d] = %s, want ascending play order", index, got)
		}
	}
	if form.Get(fmt.Sprintf("artist[%d]", scrobbleBatchLimit)) != "" {
		t.Fatal("a batch must not exceed fifty scrobbles")
	}

	service.flushPendingScrobbles(context.Background())
	if len(forms) != 2 || forms[1].Get("timestamp[9]") != "60" {
		t.Fatalf("second batch = %+v, want the remaining ten plays", forms)
	}
}

func TestScrobbleIgnoredEntryIsNotRetried(t *testing.T) {
	repo := newFakeScrobbleRepo()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, acceptedBody(
			scrobbleEntryJSON("Artist", "Song 1", 1, 2),
			scrobbleEntryJSON("Artist", "Song 2", 2, 0),
		), nil), nil
	})
	service := newScrobbleTestService(repo, transport)

	service.handleScrobble(scrobbleTrack("Song 1"), time.Unix(1, 0))
	service.handleScrobble(scrobbleTrack("Song 2"), time.Unix(2, 0))

	repo.mu.Lock()
	firstID, secondID := repo.order[0], repo.order[1]
	repo.mu.Unlock()

	service.flushPendingScrobbles(context.Background())

	repo.mu.Lock()
	ignored := append([]string{}, repo.ignoredIDs...)
	deleted := append([]string{}, repo.deletedIDs...)
	repo.mu.Unlock()

	if len(ignored) != 1 || ignored[0] != firstID {
		t.Fatalf("ignored = %v, want the row Last.fm refused", ignored)
	}
	if len(deleted) != 1 || deleted[0] != secondID {
		t.Fatalf("deleted = %v, want the accepted row", deleted)
	}
	if entry := repo.row(firstID); entry.NextRetryAt != nil {
		t.Fatalf("ignored row = %+v, want no retry time", entry)
	}
}

func TestScrobbleQueueFlushesDueRowsAtStartup(t *testing.T) {
	repo := newFakeScrobbleRepo()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, acceptedBody(), nil), nil
	})
	service := newScrobbleTestService(repo, transport)

	service.StartScrobbleQueue()
	defer service.StopScrobbleQueue()

	select {
	case <-repo.dueSignal:
	case <-time.After(2 * time.Second):
		t.Fatal("the queue did not flush at startup")
	}

	repo.mu.Lock()
	cleanups := append([]cleanupRequest{}, repo.cleanup...)
	repo.mu.Unlock()
	if len(cleanups) == 0 {
		t.Fatal("the startup flush must apply the retention cleanup")
	}
	last := cleanups[len(cleanups)-1]
	if last.maxAttempts != scrobbleMaxAttempts || last.keep != scrobbleQueueLimit {
		t.Fatalf("cleanup = %+v, want the retry and size limits", last)
	}
	if !last.olderThan.Equal(scrobbleTestNow.Add(-scrobbleRetention)) {
		t.Fatalf("cleanup cutoff = %v, want 30 days back", last.olderThan)
	}
}
