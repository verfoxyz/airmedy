package mediarequest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"airmedy/internal/domain"

	"github.com/google/uuid"
)

// Service is the only writer of media requests. Entry points call Upsert and
// get back the canonical row; they never create download jobs or promotion
// tasks themselves.
type Service struct {
	repo       domain.MediaRequestRepository
	logger     *slog.Logger
	localTrack LocalTrackChecker
}

func NewService(repo domain.MediaRequestRepository, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger}
}

// UpsertInput is one save. TrackID is the row the caller knows about; the
// worker re-resolves the identity anyway, so an empty id is acceptable.
type UpsertInput struct {
	Provider   string
	ExternalID string
	TrackID    string
	// Title / Artist are stored on first insert, so a later merge that deletes
	// TrackID leaves the tasks page with a readable label.
	Title   string
	Artist  string
	Desired string
	Quality string
}

// LocalTrackChecker answers whether an identity still resolves to a playable
// local row. *download.Service implements it; the narrow port keeps this
// package out of the download pipeline.
type LocalTrackChecker interface {
	HasLocalTrack(ctx context.Context, provider, externalID string) (bool, error)
}

// SetLocalTrackChecker installs the checker used to re-open a finished request
// whose local file no longer exists. Optional: without it a completed request
// stays idempotent even if the file was deleted outside the app.
func (s *Service) SetLocalTrackChecker(checker LocalTrackChecker) {
	s.localTrack = checker
}

// Upsert records the intent, inserting the identity's first request or updating
// the existing one.
//
// Re-saving a finished request with the same intent is a no-op: the file is
// already in the library. A finished/canceled/failed row is reopened as wanted
// so an explicit re-save retries it. A live row keeps its state; only the
// intent is refreshed.
func (s *Service) Upsert(ctx context.Context, in UpsertInput) (*domain.MediaRequest, error) {
	provider := domain.NormalizeProvider(in.Provider)
	externalID := strings.TrimSpace(in.ExternalID)
	if provider == "" || externalID == "" {
		return nil, fmt.Errorf("media request: provider and external id are required")
	}
	desired := in.Desired
	if desired == "" {
		desired = domain.MediaRequestDesiredPromote
	}
	if desired != domain.MediaRequestDesiredDownload && desired != domain.MediaRequestDesiredPromote {
		return nil, fmt.Errorf("media request: unknown desired %q", in.Desired)
	}
	quality := strings.TrimSpace(in.Quality)
	if quality != "" {
		quality = domain.ResolveDownloadQuality(quality)
	}
	trackID := strings.TrimSpace(in.TrackID)
	now := time.Now()

	existing, err := s.repo.GetByIdentity(ctx, provider, externalID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		request := &domain.MediaRequest{
			ID:         uuid.NewString(),
			Provider:   provider,
			ExternalID: externalID,
			TrackID:    trackID,
			Title:      strings.TrimSpace(in.Title),
			Artist:     strings.TrimSpace(in.Artist),
			Desired:    desired,
			Quality:    quality,
			State:      domain.MediaRequestStateWanted,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := s.repo.Ensure(ctx, request); err != nil {
			return nil, err
		}
		created, err := s.repo.GetByIdentity(ctx, provider, externalID)
		if err != nil {
			return nil, err
		}
		if created != nil {
			s.record(ctx, created, "wanted", "created")
		}
		return created, nil
	}

	if existing.State == domain.MediaRequestStateDone &&
		existing.Desired == desired && existing.Quality == quality {
		if !s.localTrackMissing(ctx, provider, externalID) {
			return existing, nil
		}
		// The row says finished, but the file it stood for is gone (deleted in the
		// app or outside it). Re-opening is what makes this save run instead of
		// silently doing nothing.
		if err := s.repo.ReopenDone(ctx, existing.ID, now); err != nil {
			return nil, err
		}
		reopened, err := s.repo.GetByID(ctx, existing.ID)
		if err != nil {
			return nil, err
		}
		if reopened != nil {
			s.record(ctx, reopened, reopened.State, "reopened: local file missing")
		}
		return reopened, nil
	}
	reset := existing.State == domain.MediaRequestStateDone ||
		existing.State == domain.MediaRequestStateFailed ||
		existing.State == domain.MediaRequestStateCanceled
	if err := s.repo.UpdateIntent(ctx, existing.ID, desired, quality, trackID, reset, now); err != nil {
		return nil, err
	}
	updated, err := s.repo.GetByID(ctx, existing.ID)
	if err != nil {
		return nil, err
	}
	if updated != nil {
		s.record(ctx, updated, updated.State, "intent updated")
	}
	return updated, nil
}

// localTrackMissing reports whether a finished request no longer has a local
// row/file behind it. An absent checker means "assume present", preserving the
// old idempotent behaviour.
func (s *Service) localTrackMissing(ctx context.Context, provider, externalID string) bool {
	if s.localTrack == nil {
		return false
	}
	local, err := s.localTrack.HasLocalTrack(ctx, provider, externalID)
	if err != nil {
		s.logger.Warn("media request: cannot verify the local file",
			"provider", provider, "external_id", externalID, "error", err)
		return false
	}
	return !local
}

// InvalidateLocalFile re-opens a finished request after its local file was
// deleted. It reports whether a row changed; the caller wakes the worker.
func (s *Service) InvalidateLocalFile(ctx context.Context, provider, externalID string) (bool, error) {
	provider = domain.NormalizeProvider(provider)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return false, nil
	}
	existing, err := s.repo.GetByIdentity(ctx, provider, externalID)
	if err != nil || existing == nil {
		return false, err
	}
	if existing.State != domain.MediaRequestStateDone {
		return false, nil
	}
	if err := s.repo.ReopenDone(ctx, existing.ID, time.Now()); err != nil {
		return false, err
	}
	if updated, getErr := s.repo.GetByID(ctx, existing.ID); getErr == nil && updated != nil {
		s.record(ctx, updated, updated.State, "reopened: local file deleted")
	}
	return true, nil
}

func (s *Service) List(ctx context.Context) ([]*domain.MediaRequest, error) {
	return s.repo.List(ctx)
}

func (s *Service) Get(ctx context.Context, id string) (*domain.MediaRequest, error) {
	return s.repo.GetByID(ctx, id)
}

// Cancel stops a request. The executor may still finish work it already started;
// a canceled row is terminal until the next save.
func (s *Service) Cancel(ctx context.Context, id string) (*domain.MediaRequest, error) {
	request, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, fmt.Errorf("media request: %s not found", id)
	}
	if err := s.repo.MarkCanceled(ctx, id, time.Now()); err != nil {
		return nil, err
	}
	updated, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if updated != nil {
		s.record(ctx, updated, updated.State, "canceled")
	}
	return updated, nil
}

// Retry re-opens a failed or canceled request with the same intent. A live or
// finished request is returned unchanged, so a double click cannot reset work
// that is already in flight.
func (s *Service) Retry(ctx context.Context, id string) (*domain.MediaRequest, error) {
	request, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, fmt.Errorf("media request: %s not found", id)
	}
	switch request.State {
	case domain.MediaRequestStateFailed, domain.MediaRequestStateCanceled:
	default:
		return request, nil
	}
	now := time.Now()
	if err := s.repo.UpdateIntent(
		ctx, id, request.Desired, request.Quality, request.TrackID, true, now,
	); err != nil {
		return nil, err
	}
	updated, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if updated != nil {
		s.record(ctx, updated, updated.State, "retried")
	}
	return updated, nil
}

// record appends history and ignores failures: the state transition already
// happened, and a missing event only loses detail on the tasks page.
func (s *Service) record(ctx context.Context, request *domain.MediaRequest, state, detail string) {
	if request == nil {
		return
	}
	event := &domain.MediaRequestEvent{
		ID:        uuid.NewString(),
		RequestID: request.ID,
		State:     state,
		Phase:     request.Phase,
		Detail:    detail,
		CreatedAt: time.Now(),
	}
	if err := s.repo.RecordEvent(ctx, event); err != nil {
		s.logger.Warn("media request: cannot record an event",
			"request_id", request.ID, "error", err)
	}
}
