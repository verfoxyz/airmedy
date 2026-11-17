package sqlite

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func newSeedRepo(t *testing.T) (*DB, domain.RecommendationSeedRepository) {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewRecommendationSeedRepository(db)
}

func seedLocalFixtures(t *testing.T, db *DB) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO artists (id, name, sort_name) VALUES
			('a1', 'Artist One', 'Artist One'),
			('a2', 'Artist Two', 'Artist Two')`, nil},
		{`INSERT INTO albums (id, title, sort_title) VALUES
			('al1', 'Album One', 'Album One')`, nil},
		{`INSERT INTO tracks (id, path, title, sort_title, album_id, play_count) VALUES
			('t1', '/m/1.flac', 'Song One', 'Song One', 'al1', 30),
			('t2', '/m/2.flac', 'Song Two', 'Song Two', 'al1', 10),
			('t3', '/m/3.flac', 'Song Three', 'Song Three', 'al1', 5)`, nil},
		{`INSERT INTO track_artists (track_id, artist_id, position) VALUES
			('t1', 'a1', 0), ('t1', 'a2', 1), ('t2', 'a2', 0), ('t3', 'a1', 0)`, nil},
		{`INSERT INTO playlists (id, name) VALUES ('p1', 'Mix')`, nil},
		{`INSERT INTO playlist_tracks (playlist_id, track_id, position) VALUES ('p1', 't3', 1)`, nil},
		{`INSERT INTO genres (id, name) VALUES ('g1', 'ambient'), ('g2', 'jazz')`, nil},
		{`INSERT INTO track_genres (track_id, genre_id, position) VALUES
			('t1', 'g1', 0), ('t2', 'g1', 0), ('t3', 'g2', 0)`, nil},
	}
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	longAgo := time.Now().AddDate(0, 0, -100).Format("2006-01-02")
	statements = append(statements,
		struct {
			query string
			args  []any
		}{`INSERT INTO daily_track_listening_stats
			(local_date, track_id, listened_seconds, play_count) VALUES
			(?, 't1', 100, 2), (?, 't2', 50, 1), (?, 't1', 20, 1),
			(?, 't3', 999, 9)`,
			[]any{today, today, yesterday, longAgo}},
	)
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed fixtures: %v\n%s", err, statement.query)
		}
	}
}

func TestRecommendationSeedRepositoryOrdersLocalSignals(t *testing.T) {
	db, repo := newSeedRepo(t)
	seedLocalFixtures(t, db)
	ctx := context.Background()

	listened, err := repo.MostListenedTracks(ctx, time.Now().AddDate(0, 0, -90), 10)
	if err != nil {
		t.Fatalf("MostListenedTracks: %v", err)
	}
	if len(listened) != 2 || listened[0].TrackID != "t1" || listened[1].TrackID != "t2" {
		t.Fatalf("listened = %+v, want t1 then t2", listened)
	}
	if listened[0].ListenedSeconds != 120 || listened[0].PlayCount != 3 {
		t.Fatalf("t1 stats = %+v, want 120s / 3 plays", listened[0])
	}
	if len(listened[0].Artists) != 2 || listened[0].Artists[0] != "Artist One" ||
		listened[0].Artists[1] != "Artist Two" {
		t.Fatalf("t1 artists = %v, want credit order", listened[0].Artists)
	}
	if listened[0].AlbumName != "Album One" {
		t.Fatalf("album = %q, want Album One", listened[0].AlbumName)
	}

	played, err := repo.MostPlayedTracks(ctx, 10)
	if err != nil {
		t.Fatalf("MostPlayedTracks: %v", err)
	}
	if len(played) != 3 || played[0].TrackID != "t1" || played[2].TrackID != "t3" {
		t.Fatalf("played = %+v, want play-count order", played)
	}

	playlist, err := repo.PlaylistTracks(ctx, 10)
	if err != nil {
		t.Fatalf("PlaylistTracks: %v", err)
	}
	if len(playlist) != 1 || playlist[0].TrackID != "t3" {
		t.Fatalf("playlist = %+v, want the playlist member", playlist)
	}

	recent, err := repo.RecentlyListenedTracks(ctx, time.Now().AddDate(0, 0, -30), 10)
	if err != nil {
		t.Fatalf("RecentlyListenedTracks: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("recent = %+v, want the two listened tracks", recent)
	}

	genres, err := repo.TopGenres(ctx, 2)
	if err != nil {
		t.Fatalf("TopGenres: %v", err)
	}
	if len(genres) != 2 || genres[0] != "ambient" {
		t.Fatalf("genres = %v, want ambient first", genres)
	}
}

func TestRecommendationSeedRepositoryHonoursTheHistoryWindow(t *testing.T) {
	db, repo := newSeedRepo(t)
	seedLocalFixtures(t, db)
	// The 100-day-old row outranks everything on raw seconds but falls outside
	// the 30-day window.
	rows, err := repo.MostListenedTracks(context.Background(), time.Now().AddDate(0, 0, -30), 10)
	if err != nil {
		t.Fatalf("MostListenedTracks: %v", err)
	}
	for _, row := range rows {
		if row.TrackID == "t3" {
			t.Fatalf("t3 is outside the requested window: %+v", row)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two in-window tracks", rows)
	}
}
