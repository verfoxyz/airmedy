package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"airmedy/internal/domain"

	"github.com/misa198/lexorank-go"
)

// downloadMergeRepository implements domain.DownloadMergeRepository: the
// download-specific SQL the merge step needs on top of TrackRepository.
type downloadMergeRepository struct {
	db     *DB
	tracks domain.TrackRepository
}

// NewDownloadMergeRepository returns the merge persistence surface.
func NewDownloadMergeRepository(db *DB, tracks domain.TrackRepository) domain.DownloadMergeRepository {
	return &downloadMergeRepository{db: db, tracks: tracks}
}

// maxPlayCount clamps the folded play count so adding two large counts cannot
// overflow the INTEGER column through Go's int.
const maxPlayCount = 1 << 30

// TrackByProviderExternalID uses idx_tracks_provider_external. The DTO is loaded
// through TrackRepository so callers get relationships populated exactly like
// any other track lookup.
func (r *downloadMergeRepository) TrackByProviderExternalID(ctx context.Context, provider, externalID string) (*domain.TrackDTO, error) {
	if provider == "" || externalID == "" {
		return nil, nil
	}
	var id string
	err := r.db.Ext(ctx).GetContext(ctx, &id,
		`SELECT id FROM tracks WHERE provider = ? AND external_id = ? LIMIT 1`, provider, externalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up track by provider %q and external id %q: %w", provider, externalID, err)
	}
	return r.tracks.GetByID(ctx, id)
}

func (r *downloadMergeRepository) PlaylistMemberships(ctx context.Context, trackID string) ([]domain.PlaylistMembership, error) {
	var rows []struct {
		PlaylistID string `db:"playlist_id"`
		Position   string `db:"position"`
	}
	if err := r.db.Ext(ctx).SelectContext(ctx, &rows,
		`SELECT playlist_id, position FROM playlist_tracks WHERE track_id = ? ORDER BY playlist_id`, trackID); err != nil {
		return nil, fmt.Errorf("list playlist memberships of %s: %w", trackID, err)
	}
	out := make([]domain.PlaylistMembership, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.PlaylistMembership{PlaylistID: row.PlaylistID, Position: row.Position})
	}
	return out, nil
}

// EnsurePlaylistTrack keeps the entry idempotent: a playlist that already lists
// the surviving track must not gain a second row (the primary key is
// (playlist_id, track_id), so INSERT OR IGNORE is the whole fix).
func (r *downloadMergeRepository) EnsurePlaylistTrack(ctx context.Context, playlistID, trackID, position string) error {
	if position == "" {
		// Nothing to inherit (the row predates LexoRank): append after the
		// current maximum so the order stays valid.
		rank, err := r.nextPosition(ctx, playlistID)
		if err != nil {
			return err
		}
		position = rank
	}
	if _, err := r.db.Ext(ctx).ExecContext(ctx,
		`INSERT OR IGNORE INTO playlist_tracks (playlist_id, track_id, position) VALUES (?, ?, ?)`,
		playlistID, trackID, position); err != nil {
		return fmt.Errorf("ensure %s in playlist %s: %w", trackID, playlistID, err)
	}
	return nil
}

// nextPosition returns the rank that sorts after the playlist's current last
// track.
func (r *downloadMergeRepository) nextPosition(ctx context.Context, playlistID string) (string, error) {
	var maxRank string
	if err := r.db.Ext(ctx).GetContext(ctx, &maxRank,
		`SELECT COALESCE(MAX(position), '') FROM playlist_tracks WHERE playlist_id = ?`, playlistID); err != nil {
		// No rows yet is not an error for MAX(), but a closed DB is.
		return "", fmt.Errorf("read last position of playlist %s: %w", playlistID, err)
	}
	if maxRank == "" {
		return lexorank.Middle().String(), nil
	}
	parsed, err := lexorank.ParseRank(maxRank)
	if err != nil {
		return "", fmt.Errorf("parse position %q of playlist %s: %w", maxRank, playlistID, err)
	}
	return parsed.GenNext().String(), nil
}

// CarryIdentity gives an online identity to the row that owns the downloaded
// file.
//
// The identity lives on exactly one row (idx_tracks_provider_external is
// UNIQUE), so the caller deletes the remote row first, in the same
// transaction. Keeping the identity matters: without it a later download of the
// same song creates a second remote row whose path can never be replaced by a
// file, and that row is unplayable forever.
func (r *downloadMergeRepository) CarryIdentity(ctx context.Context, trackID, provider, externalID string) error {
	provider = domain.NormalizeProvider(provider)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return fmt.Errorf("carry identity to %s: provider and external id are required", trackID)
	}
	_, err := r.db.Ext(ctx).ExecContext(ctx,
		`UPDATE tracks
		    SET provider = ?, external_id = ?, availability = ?, updated_at = ?
		  WHERE id = ?`,
		provider, externalID, domain.AvailabilityReady, time.Now(), trackID)
	if err != nil {
		return fmt.Errorf("carry identity %s/%s to %s: %w", provider, externalID, trackID, err)
	}
	return nil
}

func (r *downloadMergeRepository) MoveLyrics(ctx context.Context, fromTrackID, toTrackID string) error {
	if fromTrackID == "" || toTrackID == "" || fromTrackID == toTrackID {
		return nil
	}
	if _, err := r.db.Ext(ctx).ExecContext(ctx, `
		INSERT OR IGNORE INTO lyrics (track_id, content, source, meta_content, meta_source, created_at, updated_at)
		SELECT ?, content, source, meta_content, meta_source, created_at, updated_at
		  FROM lyrics WHERE track_id = ?`,
		toTrackID, fromTrackID); err != nil {
		return fmt.Errorf("move lyrics from %s to %s: %w", fromTrackID, toTrackID, err)
	}
	if _, err := r.db.Ext(ctx).ExecContext(ctx,
		`DELETE FROM lyrics WHERE track_id = ?`, fromTrackID); err != nil {
		return fmt.Errorf("remove lyrics of %s after moving: %w", fromTrackID, err)
	}
	return nil
}

func (r *downloadMergeRepository) AddPlayCount(ctx context.Context, trackID string, delta int) error {
	if delta <= 0 {
		return nil
	}
	_, err := r.db.Ext(ctx).ExecContext(ctx,
		`UPDATE tracks SET play_count = MIN(COALESCE(play_count, 0) + ?, ?), updated_at = ? WHERE id = ?`,
		delta, maxPlayCount, time.Now(), trackID)
	if err != nil {
		return fmt.Errorf("add play count to %s: %w", trackID, err)
	}
	return nil
}
