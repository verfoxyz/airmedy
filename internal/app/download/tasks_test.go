package download_test

import (
	"context"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedMediaRequest inserts one request row in the given state. Requests are the
// tasks page's rows, so the fixtures create them directly instead of driving the
// worker.
func seedMediaRequest(
	t *testing.T, h *harness, id, provider, externalID, trackID, state string,
) *domain.MediaRequest {
	t.Helper()
	ctx := context.Background()
	request := &domain.MediaRequest{
		ID:         id,
		Provider:   provider,
		ExternalID: externalID,
		TrackID:    trackID,
		Desired:    domain.MediaRequestDesiredPromote,
		State:      domain.MediaRequestStateWanted,
		CreatedAt:  time.Now().Add(-time.Minute),
		UpdatedAt:  time.Now().Add(-time.Minute),
	}
	require.NoError(t, h.requests.Ensure(ctx, request))
	switch state {
	case domain.MediaRequestStateRunning:
		require.NoError(t, h.requests.MarkRunning(ctx, id, domain.MediaRequestPhaseDownloading, trackID, time.Now()))
	case domain.MediaRequestStateDone:
		require.NoError(t, h.requests.MarkDone(ctx, id, trackID, time.Now()))
	case domain.MediaRequestStateFailed:
		require.NoError(t, h.requests.MarkFailed(ctx, id, "403 forbidden", 1, nil, time.Now()))
	case domain.MediaRequestStateCanceled:
		require.NoError(t, h.requests.MarkCanceled(ctx, id, time.Now()))
	}
	return request
}

func TestListTasks_ListsOneRowPerRequestNewestFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	doneTrack := h.seedRemote(t, "request-done")
	seedMediaRequest(t, h, "request-done", "wy", "request-done", doneTrack.ID, domain.MediaRequestStateDone)
	failedTrack := h.seedRemote(t, "request-failed")
	seedMediaRequest(t, h, "request-failed", "wy", "request-failed", failedTrack.ID, domain.MediaRequestStateFailed)

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 2)

	byID := map[string]domain.TaskRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}

	failed := byID["request-failed"]
	assert.Equal(t, domain.TaskKindRequest, failed.Kind)
	assert.Equal(t, domain.TaskStateFailed, failed.State)
	assert.Equal(t, "晴天", failed.Title)
	assert.Equal(t, "403 forbidden", failed.Error)

	done := byID["request-done"]
	assert.Equal(t, domain.TaskKindRequest, done.Kind)
	assert.Equal(t, domain.TaskStateDone, done.State)
	assert.Equal(t, "晴天", done.Title)
}

func TestListTasks_UsesSnapshotWhenTheTrackRowIsGone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A promotion merged the remote row into a local one and deleted it. The
	// request still points at the deleted id, but the snapshot keeps its label.
	now := time.Now().Add(-time.Minute)
	request := &domain.MediaRequest{
		ID:         "request-merged",
		Provider:   "wy",
		ExternalID: "42",
		TrackID:    "deleted-track-id",
		Title:      "All Eyes On Me",
		Artist:     "Artist",
		Desired:    domain.MediaRequestDesiredPromote,
		State:      domain.MediaRequestStateDone,
		CreatedAt:  now,
		UpdatedAt:  now,
		FinishedAt: &now,
	}
	require.NoError(t, h.requests.Ensure(ctx, request))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	run := findRun(t, runs, "request-merged")

	assert.Equal(t, "All Eyes On Me", run.Title)
	assert.Equal(t, "Artist", run.Artist)
	assert.NotEqual(t, run.TrackID, run.Title)
	assert.Equal(t, "request-merged", run.RequestID)
	assert.Equal(t, "wy", run.Provider)
	assert.Equal(t, "42", run.ExternalID)
}

func findRun(t *testing.T, runs []domain.TaskRun, id string) domain.TaskRun {
	t.Helper()
	for _, run := range runs {
		if run.ID == id {
			return run
		}
	}
	t.Fatalf("task run %q not found in %+v", id, runs)
	return domain.TaskRun{}
}

func TestListTasks_RepeatedSavesKeepOneRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "repeat")
	request := seedMediaRequest(t, h, "request-repeat", "wy", "repeat", track.ID, domain.MediaRequestStateWanted)

	// The unique key is the identity: re-saving updates the row, never appends.
	require.NoError(t, h.requests.UpdateIntent(
		ctx, request.ID, domain.MediaRequestDesiredDownload, domain.QualityFlac, track.ID, false, time.Now(),
	))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, domain.TaskKindRequest, runs[0].Kind)
	assert.Equal(t, domain.TaskStateActive, runs[0].State)
	assert.Equal(t, domain.QualityFlac, runs[0].Quality)
}

func TestListTasks_MapsEveryRequestState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "states")
	seedMediaRequest(t, h, "wanted", "wy", "wanted", track.ID, domain.MediaRequestStateWanted)
	seedMediaRequest(t, h, "running", "wy", "running", track.ID, domain.MediaRequestStateRunning)
	seedMediaRequest(t, h, "canceled", "wy", "canceled", track.ID, domain.MediaRequestStateCanceled)
	waiting := seedMediaRequest(t, h, "waiting", "wy", "waiting", track.ID, domain.MediaRequestStateWanted)
	next := time.Now().Add(time.Hour)
	require.NoError(t, h.requests.MarkFailed(ctx, waiting.ID, "403 forbidden", 4, &next, time.Now()))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	states := map[string]string{}
	for _, run := range runs {
		states[run.ID] = run.State
	}
	assert.Equal(t, domain.TaskStateActive, states["wanted"])
	assert.Equal(t, domain.TaskStateActive, states["running"])
	assert.Equal(t, domain.TaskStateCanceled, states["canceled"])
	assert.Equal(t, domain.TaskStateWaiting, states["waiting"])
}

func TestClearTaskHistory_KeepsActiveRequests(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	activeTrack := h.seedRemote(t, "request-active")
	seedMediaRequest(t, h, "request-active", "wy", "request-active", activeTrack.ID, domain.MediaRequestStateRunning)

	doneTrack := h.seedRemote(t, "request-history-done")
	seedMediaRequest(t, h, "request-history-done", "wy", "request-history-done", doneTrack.ID, domain.MediaRequestStateDone)

	failedTrack := h.seedRemote(t, "request-history-failed")
	seedMediaRequest(t, h, "request-history-failed", "wy", "request-history-failed", failedTrack.ID, domain.MediaRequestStateFailed)

	require.NoError(t, h.svc.ClearTaskHistory(ctx))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 1, "only the active request remains")
	assert.Equal(t, "request-active", runs[0].ID)
	assert.Equal(t, domain.TaskStateActive, runs[0].State)
}

func TestListTasks_FallsBackToTheTrackIDWhenTheRowIsGone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	track := h.seedRemote(t, "request-no-title")
	seedMediaRequest(t, h, "request-no-title", "wy", "request-no-title", track.ID, domain.MediaRequestStateDone)
	require.NoError(t, h.tracks.Delete(ctx, track.ID))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, track.ID, runs[0].Title)
	assert.Empty(t, runs[0].Artist)
	assert.Equal(t, domain.TaskStateDone, runs[0].State)
}

// TestListTasks_MergesStreamCacheSessions: caching stays its own kind on the
// tasks page, ordered by the same finish-or-start rule as the requests.
func TestListTasks_MergesStreamCacheSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	started := time.Now()

	require.NoError(t, h.cacheTasks.Start(ctx, &domain.StreamCacheTask{
		ID: "cache-done", Provider: "wy", ExternalID: "done", Title: "缓存完成",
		Artist: "歌手", Quality: domain.Quality320k, StartedAt: started,
	}))
	require.NoError(t, h.cacheTasks.Finish(ctx, "cache-done", 2048, 2048, started.Add(time.Minute)))
	require.NoError(t, h.cacheTasks.Start(ctx, &domain.StreamCacheTask{
		ID: "cache-active", Provider: "wy", ExternalID: "active", Title: "缓存中",
		Artist: "歌手", StartedAt: started.Add(2 * time.Minute),
	}))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 2)

	assert.Equal(t, domain.TaskKindCache, runs[0].Kind)
	assert.Equal(t, domain.TaskStateActive, runs[0].State)
	assert.Equal(t, "缓存中", runs[0].Title)

	assert.Equal(t, domain.TaskKindCache, runs[1].Kind)
	assert.Equal(t, domain.TaskStateDone, runs[1].State)
	assert.Equal(t, "缓存完成", runs[1].Title)
	assert.Equal(t, "歌手", runs[1].Artist)
}

// TestListTasks_MapsACanceledCacheSessionToCanceled: an intentional stop (track
// switch, shutdown) is history, not an error, so ListTasks must not report it as
// failed.
func TestListTasks_MapsACanceledCacheSessionToCanceled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	started := time.Now()

	require.NoError(t, h.cacheTasks.Start(ctx, &domain.StreamCacheTask{
		ID: "cache-canceled", Provider: "wy", ExternalID: "42", Title: "切歌中断",
		Artist: "歌手", Quality: domain.Quality320k, StartedAt: started,
	}))
	require.NoError(t, h.cacheTasks.Cancel(ctx, "cache-canceled", 4096, 1024, started.Add(time.Minute)))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, domain.TaskKindCache, runs[0].Kind)
	assert.Equal(t, domain.TaskStateCanceled, runs[0].State)
	assert.Empty(t, runs[0].Error)
}

func TestClearTaskHistory_KeepsActiveStreamCacheSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	started := time.Now()

	require.NoError(t, h.cacheTasks.Start(ctx, &domain.StreamCacheTask{
		ID: "cache-done", Provider: "wy", ExternalID: "done", Title: "缓存完成", StartedAt: started,
	}))
	require.NoError(t, h.cacheTasks.Finish(ctx, "cache-done", 1, 1, started.Add(time.Minute)))
	require.NoError(t, h.cacheTasks.Start(ctx, &domain.StreamCacheTask{
		ID: "cache-active", Provider: "wy", ExternalID: "active", Title: "缓存中", StartedAt: started,
	}))

	require.NoError(t, h.svc.ClearTaskHistory(ctx))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 1, "only the in-flight cache session remains")
	assert.Equal(t, "cache-active", runs[0].ID)
	assert.Equal(t, domain.TaskKindCache, runs[0].Kind)
	assert.Equal(t, domain.TaskStateActive, runs[0].State)
}

func TestDownloadCompletionEmitsTaskUpdated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "done-event")

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.Quality320k))
	job, err := h.jobs.ClaimNext(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NotNil(t, job)
	h.svc.ProcessJobForTest(job)

	_, done, _ := h.sink.counts()
	assert.Equal(t, 1, done)
	assert.Greater(t, h.sink.taskUpdates(), 0, "the tasks page must reload after a download finishes")
}

// TestDownloadCompletionEmitsTrackMerged: the finished download folds the
// remote row into the imported one; the player must be told which id survives.
func TestDownloadCompletionEmitsTrackMerged(t *testing.T) {
	h := newHarness(t)
	row := h.downloadAndImport(t, 200, 200)

	merged := h.sink.mergedCalls()
	require.Len(t, merged, 1)
	assert.Equal(t, "remote-duration", merged[0][0])
	assert.Equal(t, row.ID, merged[0][1])
}

func TestDownloadFailureEmitsTaskUpdated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "failed-event")
	h.resolver.err = domain.ErrStreamUnavailable

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.Quality320k))
	job, err := h.jobs.ClaimNext(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NotNil(t, job)
	h.svc.ProcessJobForTest(job)

	_, _, failed := h.sink.counts()
	assert.Equal(t, 1, failed)
	assert.Greater(t, h.sink.taskUpdates(), 0, "the tasks page must reload after a download fails")
}
