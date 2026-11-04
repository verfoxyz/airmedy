package player

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"airmedy/internal/domain"
)

type restoreTrackRepo struct {
	domain.TrackRepository
	tracks map[string]*domain.TrackDTO
}

func (r *restoreTrackRepo) GetByID(_ context.Context, id string) (*domain.TrackDTO, error) {
	return r.tracks[id], nil
}

type restoreStateRepo struct {
	domain.PlayerStateRepository
	state *domain.PlayerState
}

func (r *restoreStateRepo) Load(context.Context) (*domain.PlayerState, error) { return r.state, nil }
func (r *restoreStateRepo) Save(context.Context, *domain.PlayerState) error   { return nil }

type restoreSettingsRepo struct{}

func (restoreSettingsRepo) Load(context.Context) (*domain.AppSettings, error) {
	return &domain.AppSettings{}, nil
}
func (restoreSettingsRepo) Save(context.Context, *domain.AppSettings) error { return nil }

// TestRestoreState_KeepsRemoteTracks covers queue restore: a track whose audio
// is not on disk (virtual remote:// path) must stay in the restored queue, while
// a local track whose file disappeared is still dropped.
func TestRestoreState_KeepsRemoteTracks(t *testing.T) {
	remote := &domain.TrackDTO{Track: domain.Track{
		ID: "remote-1", Path: domain.RemotePath("wy", "1"), Title: "晴天",
		Availability: domain.AvailabilityRemote, Provider: "wy", ExternalID: "1",
	}}
	missingLocal := &domain.TrackDTO{Track: domain.Track{
		ID:           "local-gone",
		Path:         filepath.Join(t.TempDir(), "gone.mp3"),
		Availability: domain.AvailabilityReady,
	}}

	s := &PlayerService{
		player:       &fakePlayer{status: domain.PlayerStatus{Volume: 1}},
		queue:        NewQueueService(slog.Default()),
		logger:       slog.Default(),
		artworkCache: &fakeArtworkCache{},
		trackRepo: &restoreTrackRepo{tracks: map[string]*domain.TrackDTO{
			"remote-1":   remote,
			"local-gone": missingLocal,
		}},
		stateRepo: &restoreStateRepo{state: &domain.PlayerState{
			QueueTrackIDs:    []string{"remote-1", "local-gone"},
			OriginalTrackIDs: []string{"remote-1", "local-gone"},
			Volume:           1,
		}},
		settingsRepo: restoreSettingsRepo{},
		playCounted:  map[string]bool{},
		npReported:   map[string]bool{},
		posConfirmed: map[string]bool{},
	}

	s.restoreState(context.Background())

	queue := s.queue.GetQueue()
	if len(queue) != 1 {
		t.Fatalf("expected 1 restored track (remote only), got %d", len(queue))
	}
	if queue[0].ID != "remote-1" {
		t.Fatalf("expected the remote track to be restored, got %q", queue[0].ID)
	}
}
