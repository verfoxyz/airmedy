package playlist

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airmedy/internal/domain"
)

type m3u8Repo struct {
	domain.PlaylistRepository
	playlist *domain.Playlist
	tracks   []*domain.TrackDTO
}

func (r *m3u8Repo) GetByID(context.Context, string) (*domain.Playlist, error) { return r.playlist, nil }
func (r *m3u8Repo) GetTracks(context.Context, string) ([]*domain.TrackDTO, error) {
	return r.tracks, nil
}

// TestExportM3U8_RemoteTrackBecomesComment pins that a remote track keeps its
// entry and ordering (#EXTINF survives) but is written as a comment instead of
// a virtual remote:// path, which no player can open.
func TestExportM3U8_RemoteTrackBecomesComment(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.m3u8")
	tracks := []*domain.TrackDTO{
		{Track: domain.Track{ID: "l1", Path: `/music/a.mp3`, Title: "A", Duration: 180}},
		{Track: domain.Track{
			ID: "r1", Path: domain.RemotePath("WY", "42"), Title: "B", Duration: 200,
			Availability: domain.AvailabilityRemote, Provider: "wy", ExternalID: "42",
		}},
		{Track: domain.Track{ID: "l2", Path: `/music/c.mp3`, Title: "C", Duration: 220}},
	}

	svc := NewPlaylistService(
		&m3u8Repo{playlist: &domain.Playlist{ID: "p1", Name: "P"}, tracks: tracks},
		nil, nil, nil, slog.Default(),
	)
	if err := svc.ExportM3U8(context.Background(), "p1", dest); err != nil {
		t.Fatalf("ExportM3U8 failed: %v", err)
	}

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("failed to read export: %v", err)
	}
	got := string(raw)

	if strings.Contains(got, "remote://") {
		t.Fatalf("export leaked a virtual path:\n%s", got)
	}
	if !strings.Contains(got, "# remote: wy/42") {
		t.Fatalf("remote track identity missing from export:\n%s", got)
	}
	if n := strings.Count(got, "#EXTINF:"); n != 3 {
		t.Fatalf("expected 3 #EXTINF entries (ordering preserved), got %d:\n%s", n, got)
	}

	// Order must be A, remote-B, C.
	ia := strings.Index(got, `/music/a.mp3`)
	ib := strings.Index(got, "# remote: wy/42")
	ic := strings.Index(got, `/music/c.mp3`)
	if ia < 0 || ib <= ia || ic <= ib {
		t.Fatalf("track order not preserved (a=%d, remote=%d, c=%d):\n%s", ia, ib, ic, got)
	}
}
