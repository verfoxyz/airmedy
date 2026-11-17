package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func TestDislikePurgeMatchesTrackAndArtistTargets(t *testing.T) {
	svc, db, _, _ := newRemoveStreamedFixture(t)
	ctx := context.Background()
	dir := t.TempDir()

	write := func(id, name, title, artist string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := db.Exec(
			`INSERT INTO tracks (id, path, title, sort_title, availability, raw_artist_names, mtime)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, path, title, title, domain.AvailabilityReady, artist, time.Now(),
		); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		return path
	}

	match := write("local-match", "match.flac", "新·与天齐", "指尖笑")
	sameArtist := write("local-artist", "artist.flac", "Other Song", "指尖笑")
	other := write("local-other", "other.flac", "新·与天齐", "Someone Else")

	count, err := svc.CountDislikeTargets(ctx, domain.LastFmDislikeTrack, "", "新·与天齐", "指尖笑")
	if err != nil {
		t.Fatalf("CountDislikeTargets track: %v", err)
	}
	if count != 1 {
		t.Fatalf("track count = %d, want 1", count)
	}
	count, err = svc.CountDislikeTargets(ctx, domain.LastFmDislikeArtist, "", "", "指尖笑")
	if err != nil {
		t.Fatalf("CountDislikeTargets artist: %v", err)
	}
	if count != 2 {
		t.Fatalf("artist count = %d, want 2", count)
	}

	remover := &fakePlaybackRemover{}
	svc.SetPlaybackRemover(remover)

	deleted, err := svc.PurgeDislikeTargets(ctx, domain.LastFmDislikeArtist, "", "", "指尖笑")
	if err != nil {
		t.Fatalf("PurgeDislikeTargets: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if len(remover.ids) != 2 {
		t.Fatalf("playback removals = %v, want both targets", remover.ids)
	}
	for _, path := range []string{match, sameArtist} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s still exists", path)
		}
	}
	if _, statErr := os.Stat(other); statErr != nil {
		t.Fatalf("the other artist's file was deleted: %v", statErr)
	}
	if trackExists(t, db, "local-match") || trackExists(t, db, "local-artist") {
		t.Fatal("the deleted rows must be gone")
	}
	if !trackExists(t, db, "local-other") {
		t.Fatal("the other artist's row was deleted")
	}
}

func TestDislikePurgeRemovesRemoteRowsAndTheirStreamCache(t *testing.T) {
	svc, db, _, _ := newRemoveStreamedFixture(t)
	ctx := context.Background()
	dir := t.TempDir()

	localPath := filepath.Join(dir, "hit.flac")
	if err := os.WriteFile(localPath, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tracks (id, path, title, sort_title, availability, raw_artist_names, mtime)
		 VALUES ('local-hit', ?, 'Hit', 'Hit', ?, 'Artist', ?)`,
		localPath, domain.AvailabilityReady, time.Now(),
	); err != nil {
		t.Fatalf("insert local row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tracks (id, path, title, sort_title, availability, provider, external_id, raw_artist_names, mtime)
		 VALUES ('remote-hit', ?, 'Hit', 'Hit', ?, 'tx', '002ncO5A4fBJQl', 'Artist', ?)`,
		domain.RemotePath("tx", "002ncO5A4fBJQl"), domain.AvailabilityRemote, time.Now(),
	); err != nil {
		t.Fatalf("insert remote row: %v", err)
	}

	discarder := &fakeStreamCacheDiscarder{}
	svc.SetStreamCacheDiscarder(discarder)
	svc.SetPlaybackRemover(&fakePlaybackRemover{})

	count, err := svc.CountDislikeTargets(ctx, domain.LastFmDislikeTrack, "", "Hit", "Artist")
	if err != nil {
		t.Fatalf("CountDislikeTargets: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want the local and the remote row", count)
	}

	deleted, err := svc.PurgeDislikeTargets(ctx, domain.LastFmDislikeTrack, "", "Hit", "Artist")
	if err != nil {
		t.Fatalf("PurgeDislikeTargets: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if trackExists(t, db, "local-hit") || trackExists(t, db, "remote-hit") {
		t.Fatal("both rows must be gone")
	}
	if _, statErr := os.Stat(localPath); !os.IsNotExist(statErr) {
		t.Fatalf("the local file still exists")
	}
	found := false
	for _, id := range discarder.ids {
		if id == "remote-hit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discarded = %v, want the remote row's stream cache", discarder.ids)
	}
}
