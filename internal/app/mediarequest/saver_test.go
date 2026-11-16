package mediarequest

import (
	"context"
	"testing"

	"airmedy/internal/domain"
)

type mutableSettings struct {
	value *domain.AppSettings
}

func (m *mutableSettings) GetSettings(context.Context) (*domain.AppSettings, error) {
	return m.value, nil
}

func newSaverForTest(t *testing.T, repo domain.MediaRequestRepository, settings SettingsReader) *Saver {
	t.Helper()
	return NewSaver(NewService(repo, testLogger()), nil, settings, testLogger())
}

func TestSaverExplicitSaveConvergesToOneRequest(t *testing.T) {
	repo := newTestRepo(t)
	saver := newSaverForTest(t, repo, &mutableSettings{value: &domain.AppSettings{
		DownloadQuality: domain.Quality320k,
	}})
	ev := domain.RemoteTrackSaved{Track: remoteTrack(), Explicit: true}

	var state string
	for i := 0; i < 10; i++ {
		resolved, err := saver.OnRemoteTrackSaved(context.Background(), ev)
		if err != nil {
			t.Fatalf("OnRemoteTrackSaved %d: %v", i, err)
		}
		state = resolved
	}
	if state != domain.CatalogRowStateDownloading {
		t.Fatalf("state = %q, want downloading", state)
	}

	requests, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.Desired != domain.MediaRequestDesiredPromote ||
		request.Quality != domain.Quality320k || request.TrackID != "track-remote" {
		t.Fatalf("request = %+v", request)
	}
}

func TestSaverPlaylistAddFollowsTheAutoDownloadSetting(t *testing.T) {
	repo := newTestRepo(t)
	settings := &mutableSettings{value: &domain.AppSettings{AutoDownloadOnPlaylistAdd: false}}
	saver := newSaverForTest(t, repo, settings)
	ev := domain.RemoteTrackSaved{Track: remoteTrack(), PlaylistID: "pl-1"}

	state, err := saver.OnRemoteTrackSaved(context.Background(), ev)
	if err != nil {
		t.Fatalf("OnRemoteTrackSaved: %v", err)
	}
	if state != domain.CatalogRowStateSaved {
		t.Fatalf("state = %q, want saved when auto-download is off", state)
	}
	if requests, _ := repo.List(context.Background()); len(requests) != 0 {
		t.Fatalf("a disabled playlist add created a request: %+v", requests)
	}

	settings.value = &domain.AppSettings{AutoDownloadOnPlaylistAdd: true}
	state, err = saver.OnRemoteTrackSaved(context.Background(), ev)
	if err != nil {
		t.Fatalf("OnRemoteTrackSaved enabled: %v", err)
	}
	if state != domain.CatalogRowStateDownloading {
		t.Fatalf("state = %q, want downloading when auto-download is on", state)
	}
	if requests, _ := repo.List(context.Background()); len(requests) != 1 {
		t.Fatalf("the enabled playlist add did not record a request: %+v", requests)
	}
}

func TestSaverReportsAnAlreadyLocalIdentityAsSaved(t *testing.T) {
	repo := newTestRepo(t)
	saver := newSaverForTest(t, repo, &mutableSettings{value: &domain.AppSettings{}})
	ctx := context.Background()

	if _, err := saver.OnRemoteTrackSaved(ctx, domain.RemoteTrackSaved{
		Track: remoteTrack(), Explicit: true,
	}); err != nil {
		t.Fatalf("OnRemoteTrackSaved: %v", err)
	}
	request, err := repo.GetByIdentity(ctx, "wy", "42")
	if err != nil || request == nil {
		t.Fatalf("request missing: %v", err)
	}
	if err := repo.MarkDone(ctx, request.ID, "track-local", request.UpdatedAt); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	state, err := saver.OnRemoteTrackSaved(ctx, domain.RemoteTrackSaved{
		Track: remoteTrack(), Explicit: true,
	})
	if err != nil {
		t.Fatalf("OnRemoteTrackSaved done: %v", err)
	}
	if state != domain.CatalogRowStateSaved {
		t.Fatalf("state = %q, want saved for a local identity", state)
	}
}
