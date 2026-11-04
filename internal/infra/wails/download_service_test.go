package wails

import (
	"encoding/json"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The frontend reads these field names; the JSON shape is the contract.

func TestToProgressEvent_ComputesPercent(t *testing.T) {
	playlist := "pl-1"
	job := &domain.DownloadJob{
		ID: "job-1", TrackID: "track-1", PlaylistID: &playlist,
		State: domain.DownloadStateActive, BytesDone: 512, BytesTotal: 1024,
	}

	event := toProgressEvent(job)
	assert.Equal(t, 50, event.Percent)
	assert.Equal(t, "pl-1", event.PlaylistID)

	raw, err := json.Marshal(event)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	for _, key := range []string{"job_id", "track_id", "playlist_id", "state", "bytes_done", "bytes_total", "percent"} {
		assert.Contains(t, decoded, key, "the frontend reads %q", key)
	}
}

func TestToProgressEvent_NoTotalMeansZeroPercent(t *testing.T) {
	// A server that sends no Content-Length leaves BytesTotal at 0; the ring
	// must show an indeterminate state instead of dividing by zero.
	event := toProgressEvent(&domain.DownloadJob{ID: "j", State: domain.DownloadStateActive, BytesDone: 10})
	assert.Zero(t, event.Percent)
}

func TestToProgressEvent_ClampsPercentAndKeepsError(t *testing.T) {
	job := &domain.DownloadJob{
		ID: "j", State: domain.DownloadStateQueued,
		BytesDone: 2000, BytesTotal: 1000, LastError: "http 429",
	}
	event := toProgressEvent(job)
	assert.Equal(t, 100, event.Percent)
	assert.Equal(t, "http 429", event.Error)
	assert.Empty(t, event.PlaylistID, "a nil playlist must not panic")
}
