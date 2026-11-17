package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"airmedy/internal/domain"
)

type recommendationSeedRepository struct {
	db *DB
}

// NewRecommendationSeedRepository returns the local listening-signal reader
// used to rank Last.fm recommendation seeds.
func NewRecommendationSeedRepository(db *DB) domain.RecommendationSeedRepository {
	return &recommendationSeedRepository{db: db}
}

type seedTrackRow struct {
	TrackID         string `db:"track_id"`
	Title           string `db:"title"`
	AlbumName       string `db:"album_name"`
	ArtistsJoined   string `db:"artists"`
	ListenedSeconds int    `db:"listened_seconds"`
	PlayCount       int    `db:"play_count"`
}

// artistsJoined keeps the credit order without forcing a join into every
// query's group-by.
const artistsJoined = `COALESCE((
	SELECT GROUP_CONCAT(name, char(31)) FROM (
		SELECT a.name FROM track_artists ta
		JOIN artists a ON a.id = ta.artist_id
		WHERE ta.track_id = t.id ORDER BY ta.position
	)
), '')`

func (r *recommendationSeedRepository) MostListenedTracks(
	ctx context.Context, since time.Time, limit int,
) ([]domain.RecommendationSeedTrack, error) {
	return r.selectTracks(ctx, `SELECT t.id track_id, t.title,
		COALESCE(al.title, t.raw_album_name, '') album_name,
		`+artistsJoined+` artists,
		SUM(d.listened_seconds) listened_seconds, SUM(d.play_count) play_count
		FROM daily_track_listening_stats d
		JOIN tracks t ON t.id = d.track_id
		LEFT JOIN albums al ON al.id = t.album_id
		WHERE d.local_date >= ?
		GROUP BY t.id
		ORDER BY listened_seconds DESC, play_count DESC, t.title ASC
		LIMIT ?`, since.Format("2006-01-02"), limit)
}

func (r *recommendationSeedRepository) MostPlayedTracks(
	ctx context.Context, limit int,
) ([]domain.RecommendationSeedTrack, error) {
	return r.selectTracks(ctx, `SELECT t.id track_id, t.title,
		COALESCE(al.title, t.raw_album_name, '') album_name,
		`+artistsJoined+` artists,
		0 listened_seconds, t.play_count
		FROM tracks t
		LEFT JOIN albums al ON al.id = t.album_id
		WHERE t.play_count > 0
		ORDER BY t.play_count DESC, t.title ASC
		LIMIT ?`, limit)
}

func (r *recommendationSeedRepository) PlaylistTracks(
	ctx context.Context, limit int,
) ([]domain.RecommendationSeedTrack, error) {
	return r.selectTracks(ctx, `SELECT t.id track_id, t.title,
		COALESCE(al.title, t.raw_album_name, '') album_name,
		`+artistsJoined+` artists,
		0 listened_seconds, t.play_count
		FROM tracks t
		LEFT JOIN albums al ON al.id = t.album_id
		WHERE EXISTS (SELECT 1 FROM playlist_tracks pt WHERE pt.track_id = t.id)
		ORDER BY t.play_count DESC, t.title ASC
		LIMIT ?`, limit)
}

func (r *recommendationSeedRepository) RecentlyListenedTracks(
	ctx context.Context, since time.Time, limit int,
) ([]domain.RecommendationSeedTrack, error) {
	return r.selectTracks(ctx, `SELECT t.id track_id, t.title,
		COALESCE(al.title, t.raw_album_name, '') album_name,
		`+artistsJoined+` artists,
		SUM(d.listened_seconds) listened_seconds, SUM(d.play_count) play_count
		FROM daily_track_listening_stats d
		JOIN tracks t ON t.id = d.track_id
		LEFT JOIN albums al ON al.id = t.album_id
		WHERE d.local_date >= ?
		GROUP BY t.id
		ORDER BY MAX(d.local_date) DESC, t.title ASC
		LIMIT ?`, since.Format("2006-01-02"), limit)
}

func (r *recommendationSeedRepository) TopGenres(
	ctx context.Context, limit int,
) ([]string, error) {
	genres := make([]string, 0, limit)
	if err := r.db.SelectContext(ctx, &genres,
		`SELECT g.name FROM track_genres tg
		 JOIN genres g ON g.id = tg.genre_id
		 GROUP BY g.id
		 ORDER BY COUNT(*) DESC, g.name ASC
		 LIMIT ?`, limit); err != nil {
		return nil, fmt.Errorf("recommendation seeds: top genres: %w", err)
	}
	return genres, nil
}

func (r *recommendationSeedRepository) selectTracks(
	ctx context.Context, query string, args ...any,
) ([]domain.RecommendationSeedTrack, error) {
	rows := make([]seedTrackRow, 0)
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("recommendation seeds: query: %w", err)
	}
	result := make([]domain.RecommendationSeedTrack, 0, len(rows))
	for _, row := range rows {
		result = append(result, domain.RecommendationSeedTrack{
			TrackID:         row.TrackID,
			Title:           row.Title,
			Artists:         splitJoined(row.ArtistsJoined),
			AlbumName:       row.AlbumName,
			ListenedSeconds: row.ListenedSeconds,
			PlayCount:       row.PlayCount,
		})
	}
	return result, nil
}

func splitJoined(value string) []string {
	if value == "" {
		return []string{}
	}
	parts := strings.Split(value, "\x1f")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
