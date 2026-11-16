package download

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// taskHistoryLimit is how many finished runs each table keeps. History is for
// the tasks page, not an audit log; trimming the oldest rows keeps the tables
// from growing without bound.
const taskHistoryLimit = 500

// ListTasks builds the tasks page's read model: one row per media request,
// plus one per stream-cache session.
//
// Requests are authoritative — a download job or a promotion never adds a row
// of its own, so repeated saves of the same identity converge on one line. The
// title and artist prefer the request's own snapshot, then the current track row
// (one batched lookup). A merge may have deleted that row, so the snapshot is
// what keeps the label readable instead of falling back to a bare track id. The
// list is newest first, using finished_at when the run is over and created_at
// while it is active.
func (s *Service) ListTasks(ctx context.Context) ([]domain.TaskRun, error) {
	runs := make([]domain.TaskRun, 0)

	if s.requests != nil {
		requests, err := s.requests.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("tasks: list requests: %w", err)
		}

		trackIDs := make([]string, 0, len(requests))
		seen := make(map[string]struct{}, len(requests))
		for _, request := range requests {
			if request == nil || request.TrackID == "" {
				continue
			}
			if _, duplicate := seen[request.TrackID]; duplicate {
				continue
			}
			seen[request.TrackID] = struct{}{}
			trackIDs = append(trackIDs, request.TrackID)
		}
		titles := map[string]*domain.TrackDTO{}
		if len(trackIDs) > 0 {
			tracks, lookupErr := s.tracks.GetByIDs(ctx, trackIDs)
			if lookupErr != nil {
				// The list is still useful without titles; the snapshots carry the label.
				s.logger.Warn("tasks: cannot resolve request titles", "error", lookupErr)
			} else {
				for _, track := range tracks {
					if track != nil {
						titles[track.ID] = track
					}
				}
			}
		}

		for _, request := range requests {
			if request == nil {
				continue
			}
			run := taskRunFromRequest(request)
			if track := titles[request.TrackID]; track != nil {
				run.Title = track.Title
				run.Artist = tagArtist(track)
			}
			if strings.TrimSpace(run.Title) == "" {
				// Old rows predate the snapshot; the identity is a stable label and
				// never shows the track id a merge already invalidated.
				run.Title = request.ExternalID
			}
			runs = append(runs, run)
		}
	}

	if s.cacheTasks != nil {
		tasks, listErr := s.cacheTasks.List(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("tasks: list stream cache: %w", listErr)
		}
		for _, task := range tasks {
			if task == nil {
				continue
			}
			runs = append(runs, taskRunFromCacheTask(task))
		}
	}

	sort.SliceStable(runs, func(i, j int) bool {
		return taskSortTime(runs[i]).After(taskSortTime(runs[j]))
	})
	return runs, nil
}

// taskSortTime is the ordering key the tasks page contract names: the finish
// time when the run is over, otherwise its creation time.
func taskSortTime(run domain.TaskRun) time.Time {
	if run.FinishedAt != nil {
		return *run.FinishedAt
	}
	return run.CreatedAt
}

// ClearTaskHistory deletes every finished run from the request history and the
// stream-cache log. Active work is untouched, and the tasks page is told to
// reload.
func (s *Service) ClearTaskHistory(ctx context.Context) error {
	if s.requests != nil {
		if _, err := s.requests.TrimFinished(ctx, 0); err != nil {
			return fmt.Errorf("tasks: clear request history: %w", err)
		}
	}
	// Execution history is kept for diagnostics only; the requests are the rows
	// the page shows, so their old jobs go too.
	if _, err := s.jobs.TrimFinished(ctx, 0); err != nil {
		return fmt.Errorf("tasks: clear download history: %w", err)
	}
	if s.cacheTasks != nil {
		if _, err := s.cacheTasks.TrimFinished(ctx, 0); err != nil {
			return fmt.Errorf("tasks: clear stream cache history: %w", err)
		}
	}
	s.emitTaskUpdated()
	return nil
}

// trimTaskHistory enforces the cap after a run finishes. A failure only leaves
// a few extra history rows, so it is logged rather than surfaced.
func (s *Service) trimTaskHistory(ctx context.Context) {
	if s.requests != nil {
		if _, err := s.requests.TrimFinished(ctx, taskHistoryLimit); err != nil {
			s.logger.Warn("tasks: cannot trim request history", "error", err)
		}
	}
	if _, err := s.jobs.TrimFinished(ctx, taskHistoryLimit); err != nil {
		s.logger.Warn("tasks: cannot trim download history", "error", err)
	}
	if s.cacheTasks != nil {
		if _, err := s.cacheTasks.TrimFinished(ctx, taskHistoryLimit); err != nil {
			s.logger.Warn("tasks: cannot trim stream cache history", "error", err)
		}
	}
}

func taskRunFromRequest(request *domain.MediaRequest) domain.TaskRun {
	run := domain.TaskRun{
		ID:         request.ID,
		RequestID:  request.ID,
		Kind:       domain.TaskKindRequest,
		State:      taskStateFromRequest(request),
		Phase:      request.Phase,
		TrackID:    request.TrackID,
		Title:      request.Title,
		Artist:     request.Artist,
		Provider:   request.Provider,
		ExternalID: request.ExternalID,
		Quality:    request.Quality,
		Error:      request.LastError,
		CreatedAt:  request.CreatedAt,
		FinishedAt: request.FinishedAt,
	}
	if run.State == domain.TaskStateActive {
		// Requests do not store a start timestamp; updated_at is the closest
		// observable moment the work last advanced.
		started := request.UpdatedAt
		run.StartedAt = &started
	}
	return run
}

func taskRunFromCacheTask(task *domain.StreamCacheTask) domain.TaskRun {
	started := task.StartedAt
	return domain.TaskRun{
		ID:         task.ID,
		Kind:       domain.TaskKindCache,
		State:      taskStateFromCache(task.State),
		Title:      task.Title,
		Artist:     task.Artist,
		Quality:    task.Quality,
		Error:      task.Error,
		Provider:   task.Provider,
		ExternalID: task.ExternalID,
		CreatedAt:  task.StartedAt,
		StartedAt:  &started,
		FinishedAt: task.FinishedAt,
	}
}

func taskStateFromRequest(request *domain.MediaRequest) string {
	switch request.State {
	case domain.MediaRequestStateWanted, domain.MediaRequestStateQueued,
		domain.MediaRequestStateRunning:
		return domain.TaskStateActive
	case domain.MediaRequestStateDone:
		return domain.TaskStateDone
	case domain.MediaRequestStateFailed:
		if request.NextAttemptAt != nil {
			// A retry is scheduled: the row is waiting, not finished.
			return domain.TaskStateActive
		}
		return domain.TaskStateFailed
	case domain.MediaRequestStateCanceled:
		return domain.TaskStateCanceled
	default:
		return domain.TaskStateFailed
	}
}

func taskStateFromCache(state string) string {
	switch state {
	case domain.StreamCacheTaskStateActive:
		return domain.TaskStateActive
	case domain.StreamCacheTaskStateDone:
		return domain.TaskStateDone
	case domain.StreamCacheTaskStateCanceled:
		// An intentional stop (track switch, shutdown) is history, not an error.
		return domain.TaskStateCanceled
	default:
		return domain.TaskStateFailed
	}
}
