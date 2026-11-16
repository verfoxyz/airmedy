package download_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tieredResolver answers per quality: a tier in refs succeeds, every other tier
// reports "no link at this quality". It records the order it was asked.
type tieredResolver struct {
	mu        sync.Mutex
	qualities []string
	refs      map[string]domain.StreamRef
}

func (r *tieredResolver) Resolve(_ context.Context, _ *domain.TrackDTO, quality string) (domain.StreamRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.qualities = append(r.qualities, quality)
	if ref, ok := r.refs[quality]; ok {
		return ref, nil
	}
	return domain.StreamRef{}, fmt.Errorf("%w: no %s link", domain.ErrStreamQualityUnavailable, quality)
}

func (r *tieredResolver) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.qualities...)
}

// TestDownloadFallsBackToALowerQuality: a source without a flac link must
// download 320k instead of failing, and the job/finalizer must record 320k.
func TestDownloadFallsBackToALowerQuality(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	resolver := &tieredResolver{refs: map[string]domain.StreamRef{
		domain.Quality320k: {URL: "http://cdn.example/320.mp3"},
	}}
	h.useResolver(resolver)

	track := seedPluginTrack(t, h, "wy", "42")
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.QualityFlac))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the fallback download to finish", func() bool {
		return h.activeJob(t, track.ID) == nil
	})

	assert.Equal(t, []string{domain.QualityFlac, domain.Quality320k}, resolver.calls())

	h.finalizer.mu.Lock()
	finalizedQuality := h.finalizer.last.Quality
	h.finalizer.mu.Unlock()
	assert.Equal(t, domain.Quality320k, finalizedQuality, "the file must be finalized as the tier that resolved")

	jobs, err := h.jobs.List(ctx)
	require.NoError(t, err)
	var job *domain.DownloadJob
	for _, candidate := range jobs {
		if candidate.TrackID == track.ID {
			job = candidate
			break
		}
	}
	require.NotNil(t, job)
	assert.Equal(t, domain.DownloadStateDone, job.State)
	assert.Equal(t, domain.Quality320k, job.Quality, "the job must record the actual tier")
}

// TestDownloadReportsEveryAttemptedQualityWhenAllFail: the error names the tried
// tiers so the user can act on it, and the job ends failed.
func TestDownloadReportsEveryAttemptedQualityWhenAllFail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	resolver := &tieredResolver{}
	h.useResolver(resolver)

	track := seedPluginTrack(t, h, "wy", "43")
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.QualityFlac))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the job to fail", func() bool {
		return h.activeJob(t, track.ID) == nil
	})

	assert.Equal(t, []string{domain.QualityFlac, domain.Quality320k, domain.Quality128k}, resolver.calls())

	jobs, err := h.jobs.List(ctx)
	require.NoError(t, err)
	var job *domain.DownloadJob
	for _, candidate := range jobs {
		if candidate.TrackID == track.ID {
			job = candidate
			break
		}
	}
	require.NotNil(t, job)
	assert.Equal(t, domain.DownloadStateFailed, job.State)
	assert.Contains(t, job.LastError, "no playable quality among")
	assert.Contains(t, job.LastError, domain.Quality128k)
}
