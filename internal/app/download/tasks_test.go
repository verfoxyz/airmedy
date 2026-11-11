package download_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListTasks_MergesDownloadsAndPromotionsNewestFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	downloadTrack := h.seedRemote(t, "dl-done")
	seedJob(t, h, "job-done", downloadTrack.ID, domain.DownloadStateActive)
	require.NoError(t, h.jobs.MarkDone(ctx, "job-done", "/music/晴天.mp3"))

	taskID, err := h.promotions.EnsureQueued(ctx, &domain.PromotionTask{
		TrackID: "remote-promoted", Provider: "wy", ExternalID: "promoted",
		Title: "提升曲", Artist: "歌手", Quality: domain.Quality320k,
	})
	require.NoError(t, err)
	// A later finish time makes the order deterministic regardless of clock
	// granularity between the two tables.
	require.NoError(t, h.promotions.MarkFinished(ctx, taskID, domain.PromotionStateFailed,
		"disk full", time.Now().Add(time.Minute)))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 2)

	promotion := runs[0]
	assert.Equal(t, domain.TaskKindPromote, promotion.Kind)
	assert.Equal(t, domain.TaskStateFailed, promotion.State)
	assert.Equal(t, "提升曲", promotion.Title)
	assert.Equal(t, "歌手", promotion.Artist)
	assert.Equal(t, "disk full", promotion.Error)

	download := runs[1]
	assert.Equal(t, domain.TaskKindDownload, download.Kind)
	assert.Equal(t, domain.TaskStateDone, download.State)
	assert.Equal(t, "晴天", download.Title)
}

func TestClearTaskHistory_KeepsActiveWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	activeTrack := h.seedRemote(t, "dl-active")
	seedJob(t, h, "job-active", activeTrack.ID, domain.DownloadStateQueued)

	doneTrack := h.seedRemote(t, "dl-history-done")
	seedJob(t, h, "job-history-done", doneTrack.ID, domain.DownloadStateActive)
	require.NoError(t, h.jobs.MarkDone(ctx, "job-history-done", "/music/done.mp3"))

	failedTrack := h.seedRemote(t, "dl-history-failed")
	seedJob(t, h, "job-history-failed", failedTrack.ID, domain.DownloadStateActive)
	require.NoError(t, h.jobs.MarkFailed(ctx, "job-history-failed", "403", nil))

	promotionDone, err := h.promotions.EnsureQueued(ctx, &domain.PromotionTask{
		Provider: "wy", ExternalID: "promo-done",
	})
	require.NoError(t, err)
	require.NoError(t, h.promotions.MarkFinished(ctx, promotionDone, domain.PromotionStateDone, "", time.Now()))
	promotionActive, err := h.promotions.EnsureQueued(ctx, &domain.PromotionTask{
		Provider: "wy", ExternalID: "promo-active",
	})
	require.NoError(t, err)

	require.NoError(t, h.svc.ClearTaskHistory(ctx))

	runs, err := h.svc.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, runs, 2, "only the active download and promotion remain")

	ids := []string{runs[0].ID, runs[1].ID}
	assert.ElementsMatch(t, []string{"job-active", promotionActive}, ids)
	for _, run := range runs {
		assert.Equal(t, domain.TaskStateActive, run.State)
	}
}

func TestTrimFinished_KeepsTheNewestRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "trim")

	for i := 0; i < 502; i++ {
		require.NoError(t, h.jobs.Create(ctx, &domain.DownloadJob{
			ID:      fmt.Sprintf("trim-%d", i),
			TrackID: track.ID,
			State:   domain.DownloadStateDone,
		}))
	}
	require.NoError(t, h.jobs.Create(ctx, &domain.DownloadJob{
		ID: "trim-active", TrackID: track.ID, State: domain.DownloadStateQueued,
	}))

	deleted, err := h.jobs.TrimFinished(ctx, 500)
	require.NoError(t, err)
	assert.EqualValues(t, 2, deleted)

	var remaining int
	require.NoError(t, h.db.Get(&remaining, `SELECT COUNT(*) FROM download_jobs WHERE state = ?`, domain.DownloadStateDone))
	assert.Equal(t, 500, remaining)

	deleted, err = h.jobs.TrimFinished(ctx, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 500, deleted)

	var active int
	require.NoError(t, h.db.Get(&active, `SELECT COUNT(*) FROM download_jobs WHERE id = 'trim-active'`))
	assert.Equal(t, 1, active, "active work is never trimmed")
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
