package download

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// Merge rule identifiers, logged so a behaviour change stays traceable.
const (
	// mergeM1: another row already holds this (provider, external_id) and is
	// ready, so the remote row is redundant.
	mergeM1 = "M1"
	// mergeM2: the target path already belongs to a different row.
	mergeM2 = "M2"
	// mergeM3: a local track with the same title/artist/duration exists. v1
	// deliberately does NOT merge here — a live version or a cover merging into
	// the studio track is worse than a duplicate — it only warns.
	mergeM3 = "M3"
	// mergeNone: no survivor rule matched; the imported row simply replaces the
	// remote one.
	mergeNone = "import"
)

// durationMergeTolerance is the ±3s window of the M3 heuristic.
const durationMergeTolerance = 3 * time.Second

// Merger reconciles a finished download with what the library already holds.
//
// The remote row never survives a successful download. Nothing can play it (its
// path is a virtual remote:// reference) and nothing would ever delete it, so
// keeping it would leave a permanently unplayable duplicate in every playlist
// that requested the song. Its associations move onto the surviving row and the
// row is deleted, all in one transaction.
type Merger struct {
	tracks domain.TrackRepository
	repo   domain.DownloadMergeRepository
	tx     domain.TxManager
	logger *slog.Logger
}

// NewMerger wires the merge step. repo and tx are required; tracks is used for
// the plain lookups the merge and the M3 warning need.
func NewMerger(tracks domain.TrackRepository, repo domain.DownloadMergeRepository, tx domain.TxManager, logger *slog.Logger) *Merger {
	return &Merger{tracks: tracks, repo: repo, tx: tx, logger: logger}
}

// MergeInput is one finished finalize run seen through the merge rules.
type MergeInput struct {
	// RemotePath is the remote:// path of the track that was downloaded. It
	// finds the row that must be replaced (invariant R2: a non-ready row's path
	// is exactly RemotePath(provider, external_id)).
	RemotePath string
	// Provider and ExternalID identify the same song at the same source.
	Provider   string
	ExternalID string
	// TargetPath is where the file now lives, TargetTrackID the row the import
	// created for it.
	TargetPath    string
	TargetTrackID string
}

// MergeResult reports which rule fired and which row was removed.
type MergeResult struct {
	// Rule is mergeM1, mergeM2, mergeNone, or "" when nothing was merged.
	Rule string
	// RemovedTrackID is the remote row that was deleted, or "".
	RemovedTrackID string
	// SurvivorTrackID is the row that owns the file after the merge, or "" when
	// nothing was merged. Post-download work that keys on a track id (lyrics)
	// must use this row, not the imported or remote one.
	SurvivorTrackID string
}

// Merge applies M1, then M2, then folds the remote row away.
func (m *Merger) Merge(ctx context.Context, in MergeInput) (MergeResult, error) {
	if m == nil || m.tracks == nil || m.repo == nil || m.tx == nil {
		return MergeResult{}, nil
	}
	if strings.TrimSpace(in.RemotePath) == "" || strings.TrimSpace(in.TargetTrackID) == "" {
		return MergeResult{}, nil
	}

	remote, err := m.tracks.GetByPath(ctx, in.RemotePath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("merge: load remote row %s: %w", in.RemotePath, err)
	}
	if remote == nil || !remote.IsRemote() {
		// Gone, or already a real file: nothing to replace.
		return MergeResult{}, nil
	}
	if remote.ID == in.TargetTrackID {
		// The import updated the existing row in place; there is a single row.
		return MergeResult{}, nil
	}

	survivorID, rule, err := m.pickSurvivor(ctx, in, remote)
	if err != nil {
		return MergeResult{}, err
	}

	if rule != mergeNone {
		if err := m.foldInto(ctx, survivorID, remote.ID); err != nil {
			return MergeResult{}, err
		}
		// The survivor usually already carries the identity (M1 matched by it), but
		// an M2 survivor can be a file-only row. Giving it the identity keeps the
		// search page's "in library" refresh working after the merge.
		if err := m.carryIdentityIfMissing(ctx, survivorID, remote); err != nil {
			return MergeResult{}, err
		}
		m.logger.Info("download: folded remote track row into the downloaded one",
			"rule", rule, "kept", survivorID, "removed", remote.ID, "path", in.TargetPath)
		return MergeResult{Rule: rule, RemovedTrackID: remote.ID, SurvivorTrackID: survivorID}, nil
	}

	// No rule matched. The imported row owns the file and the resolved
	// artists/album the import produced, so it is the row that survives; the
	// remote row is folded away and its identity moves onto the survivor —
	// "the imported row simply replaces the remote one".
	//
	// The previous behaviour did the opposite: it kept the remote row and only
	// flipped its availability to 'ready'. That left the row pointing at
	// remote://<provider>/<id> while a second row held the actual file, so the
	// row was permanently unplayable — IsRemote() still reports true and no
	// decoder can open the virtual path.
	m.warnOnLocalDuplicate(ctx, in, remote.ID)
	if err := m.foldInto(ctx, survivorID, remote.ID); err != nil {
		return MergeResult{}, err
	}
	if err := m.carryIdentity(ctx, survivorID, remote); err != nil {
		return MergeResult{}, err
	}
	m.logger.Info("download: imported row replaced the remote one",
		"rule", mergeNone, "kept", survivorID, "removed", remote.ID, "path", in.TargetPath)
	return MergeResult{Rule: mergeNone, RemovedTrackID: remote.ID, SurvivorTrackID: survivorID}, nil
}

// carryIdentityIfMissing moves the remote identity onto a survivor that has
// none. A survivor already bound to another identity keeps it (overwriting would
// silently re-label a different online song).
func (m *Merger) carryIdentityIfMissing(ctx context.Context, survivorID string, remote *domain.TrackDTO) error {
	survivor, err := m.tracks.GetByID(ctx, survivorID)
	if err != nil {
		return fmt.Errorf("merge: load survivor %s: %w", survivorID, err)
	}
	if survivor == nil || survivor.Provider != "" {
		return nil
	}
	return m.carryIdentity(ctx, survivorID, remote)
}

// carryIdentity moves the remote row's online identity onto the row that owns
// the downloaded file, after foldInto deleted the remote row.
//
// Without it the identity would be dropped, and a later download of the same
// song would register a fresh remote row: the exact state that made this bug
// permanent. A row that never carried an identity is left alone, which is the
// pre-077 shape.
func (m *Merger) carryIdentity(ctx context.Context, survivorID string, remote *domain.TrackDTO) error {
	if remote.Provider == "" || remote.ExternalID == "" {
		return nil
	}
	if err := m.repo.CarryIdentity(ctx, survivorID, remote.Provider, remote.ExternalID); err != nil {
		return fmt.Errorf("merge: carry identity %s/%s to %s: %w",
			remote.Provider, remote.ExternalID, survivorID, err)
	}
	return nil
}

// pickSurvivor decides which row keeps the associations.
// FindByRemoteIdentity returns the row already registered for an online
// identity, or nil. Used before creating a remote track so the partial unique
// index is a backstop rather than the primary path.
func (m *Merger) FindByRemoteIdentity(ctx context.Context, provider, externalID string) (*domain.TrackDTO, error) {
	return m.repo.TrackByProviderExternalID(ctx, provider, externalID)
}

func (m *Merger) pickSurvivor(ctx context.Context, in MergeInput, remote *domain.TrackDTO) (survivorID, rule string, err error) {
	remoteID := remote.ID
	// M1: the same song is already in the library from an earlier download.
	if in.Provider != "" && in.ExternalID != "" {
		existing, err := m.repo.TrackByProviderExternalID(ctx, in.Provider, in.ExternalID)
		if err != nil {
			return "", "", fmt.Errorf("merge: look up %s/%s: %w", in.Provider, in.ExternalID, err)
		}
		// The remote row must itself belong to this identity before it is
		// deleted. A row with no identity (pre-077 data, or one created before
		// the download finished) looks identical by path but carries the
		// user's playlist membership, favorite flag and play count: absorbing
		// it would silently lose all three. Such a row survives and is retired
		// onto the downloaded file instead (see Merge).
		ownsIdentity := remote.Provider == in.Provider && remote.ExternalID == in.ExternalID
		if ownsIdentity && existing != nil && existing.ID != remoteID && existing.ID != in.TargetTrackID &&
			existing.Availability == domain.AvailabilityReady {
			return existing.ID, mergeM1, nil
		}
	}

	// M2: the target path is already owned by a different row.
	if strings.TrimSpace(in.TargetPath) != "" {
		owner, err := m.tracks.GetByPath(ctx, in.TargetPath)
		if err != nil {
			return "", "", fmt.Errorf("merge: load target row %s: %w", in.TargetPath, err)
		}
		if owner != nil && owner.ID != remoteID && owner.ID != in.TargetTrackID {
			return owner.ID, mergeM2, nil
		}
	}

	return in.TargetTrackID, mergeNone, nil
}

// warnOnLocalDuplicate logs the M3 case: a local track that looks like the same
// song but came from somewhere else. Deliberately not merged.
func (m *Merger) warnOnLocalDuplicate(ctx context.Context, in MergeInput, remoteID string) {
	target, err := m.tracks.GetByID(ctx, in.TargetTrackID)
	if err != nil || target == nil {
		return
	}
	all, err := m.tracks.GetAll(ctx)
	if err != nil {
		m.logger.Debug("download: skipped the duplicate hint scan", "error", err)
		return
	}
	for _, other := range all {
		if other == nil || other.ID == target.ID || other.ID == remoteID || other.IsRemote() {
			continue
		}
		if looksLikeSameSong(other, target) {
			m.logger.Warn("download: library may already contain this song (not merged)",
				"rule", mergeM3, "downloaded", target.ID, "similar", other.ID,
				"title", other.Title, "duration", other.Duration)
		}
	}
}

// looksLikeSameSong is the M3 heuristic: same title, same primary artist and a
// duration within ±3s.
func looksLikeSameSong(a, b *domain.TrackDTO) bool {
	title := normalizeForMatch(a.Title)
	if title == "" || title != normalizeForMatch(b.Title) {
		return false
	}
	if normalizeForMatch(primaryArtist(a)) != normalizeForMatch(primaryArtist(b)) {
		return false
	}
	diff := int64(a.Duration) - int64(b.Duration)
	if diff < 0 {
		diff = -diff
	}
	return time.Duration(diff)*time.Second <= durationMergeTolerance
}

func normalizeForMatch(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// primaryArtist prefers the resolved entity and falls back to the first raw tag
// value: an imported file carries the raw names, a remote row may carry none.
func primaryArtist(t *domain.TrackDTO) string {
	if len(t.Artists) > 0 && t.Artists[0] != nil && strings.TrimSpace(t.Artists[0].Name) != "" {
		return t.Artists[0].Name
	}
	return t.RawArtistNames
}

// foldInto moves the remote row's playlist entries, favorite flag and play count
// onto survivorID and deletes the remote row, all in one transaction.
//
// A playlist that already lists the survivor keeps a single entry (the position
// transfer is INSERT OR IGNORE), and the position is reused so the song does not
// jump to the top of the playlist when it finishes downloading.
func (m *Merger) foldInto(ctx context.Context, survivorID, remoteID string) error {
	if survivorID == "" {
		return fmt.Errorf("merge: no survivor row for %s", remoteID)
	}
	return m.tx.RunInTx(ctx, func(ctx context.Context) error {
		remote, err := m.tracks.GetByID(ctx, remoteID)
		if err != nil {
			return fmt.Errorf("merge: reload remote row %s: %w", remoteID, err)
		}
		if remote == nil {
			return nil
		}

		memberships, err := m.repo.PlaylistMemberships(ctx, remoteID)
		if err != nil {
			return fmt.Errorf("merge: list playlists of %s: %w", remoteID, err)
		}
		// Lyrics fetched before the merge are attached to the remote row; they
		// would be cascade-deleted when it is folded away.
		if err := m.repo.MoveLyrics(ctx, remoteID, survivorID); err != nil {
			return fmt.Errorf("merge: move lyrics of %s to %s: %w", remoteID, survivorID, err)
		}
		// Deterministic order keeps a failing merge reproducible.
		sort.Slice(memberships, func(i, j int) bool { return memberships[i].PlaylistID < memberships[j].PlaylistID })
		for _, membership := range memberships {
			if err := m.repo.EnsurePlaylistTrack(ctx, membership.PlaylistID, survivorID, membership.Position); err != nil {
				return fmt.Errorf("merge: carry %s into playlist %s: %w", survivorID, membership.PlaylistID, err)
			}
		}

		survivor, err := m.tracks.GetByID(ctx, survivorID)
		if err != nil {
			return fmt.Errorf("merge: load survivor %s: %w", survivorID, err)
		}
		if survivor == nil {
			return fmt.Errorf("merge: survivor %s vanished before the merge", survivorID)
		}
		if !survivor.IsFavorite && remote.IsFavorite {
			if err := m.tracks.SetFavorite(ctx, survivorID, true); err != nil {
				return fmt.Errorf("merge: carry favorite to %s: %w", survivorID, err)
			}
		}
		if remote.PlayCount > 0 {
			if err := m.repo.AddPlayCount(ctx, survivorID, remote.PlayCount); err != nil {
				return fmt.Errorf("merge: carry play count to %s: %w", survivorID, err)
			}
		}

		// The remote row's playlist entries go with it (ON DELETE CASCADE).
		if err := m.tracks.Delete(ctx, remoteID); err != nil {
			return fmt.Errorf("merge: delete remote row %s: %w", remoteID, err)
		}
		return nil
	})
}
