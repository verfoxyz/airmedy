package download

import (
	"context"
	"fmt"
	"sort"
	"time"

	"airmedy/internal/domain"
)

// taskHistoryLimit is how many finished runs each table keeps. History is for
// the tasks page, not an audit log; trimming the oldest rows keeps the tables
// from growing without bound.
const taskHistoryLimit = 500

// ListTasks merges downloads and promotions into the tasks page's read model.
//
// Both tables are read once. Download rows carry only a track id, so their
// titles are resolved with one batched GetByIDs; a track deleted after its job
// finished simply leaves the row without a title, and the UI falls back to the
// id. The merged list is newest first, using finished_at when the run is over
// and created/started time while it is active.
func (s *Service) ListTasks(ctx context.Context) ([]domain.TaskRun, error) {
	jobs, err := s.jobs.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks: list downloads: %w", err)
	}

	jobTrackIDs := make([]string, 0, len(jobs))
	seen := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		if job == nil || job.TrackID == "" {
			continue
		}
		if _, duplicate := seen[job.TrackID]; duplicate {
			continue
		}
		seen[job.TrackID] = struct{}{}
		jobTrackIDs = append(jobTrackIDs, job.TrackID)
	}
	titles := map[string]*domain.TrackDTO{}
	if len(jobTrackIDs) > 0 {
		tracks, lookupErr := s.tracks.GetByIDs(ctx, jobTrackIDs)
		if lookupErr != nil {
			// The list is still useful without titles; the rows keep their ids.
			s.logger.Warn("tasks: cannot resolve download titles", "error", lookupErr)
		} else {
			for _, track := range tracks {
				if track != nil {
					titles[track.ID] = track
				}
			}
		}
	}

	runs := make([]domain.TaskRun, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		run := taskRunFromJob(job)
		if track := titles[job.TrackID]; track != nil {
			run.Title = track.Title
			run.Artist = tagArtist(track)
		}
		runs = append(runs, run)
	}

	if s.promotionTask != nil {
		tasks, listErr := s.promotionTask.List(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("tasks: list promotions: %w", listErr)
		}
		for _, task := range tasks {
			if task == nil {
				continue
			}
			runs = append(runs, taskRunFromPromotion(task))
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

// ClearTaskHistory deletes every finished run from both tables. Active work is
// untouched, and the tasks page is told to reload.
func (s *Service) ClearTaskHistory(ctx context.Context) error {
	if _, err := s.jobs.TrimFinished(ctx, 0); err != nil {
		return fmt.Errorf("tasks: clear download history: %w", err)
	}
	if s.promotionTask != nil {
		if _, err := s.promotionTask.TrimFinished(ctx, 0); err != nil {
			return fmt.Errorf("tasks: clear promotion history: %w", err)
		}
	}
	s.emitTaskUpdated()
	return nil
}

// trimTaskHistory enforces the cap after a run finishes. A failure only leaves
// a few extra history rows, so it is logged rather than surfaced.
func (s *Service) trimTaskHistory(ctx context.Context) {
	if _, err := s.jobs.TrimFinished(ctx, taskHistoryLimit); err != nil {
		s.logger.Warn("tasks: cannot trim download history", "error", err)
	}
	if s.promotionTask != nil {
		if _, err := s.promotionTask.TrimFinished(ctx, taskHistoryLimit); err != nil {
			s.logger.Warn("tasks: cannot trim promotion history", "error", err)
		}
	}
}

func taskRunFromJob(job *domain.DownloadJob) domain.TaskRun {
	return domain.TaskRun{
		ID:         job.ID,
		Kind:       domain.TaskKindDownload,
		State:      taskStateFromDownload(job.State),
		TrackID:    job.TrackID,
		Quality:    job.Quality,
		Error:      job.LastError,
		CreatedAt:  job.CreatedAt,
		StartedAt:  job.StartedAt,
		FinishedAt: job.FinishedAt,
	}
}

func taskRunFromPromotion(task *domain.PromotionTask) domain.TaskRun {
	return domain.TaskRun{
		ID:         task.ID,
		Kind:       domain.TaskKindPromote,
		State:      taskStateFromPromotion(task.State),
		TrackID:    task.TrackID,
		Title:      task.Title,
		Artist:     task.Artist,
		Quality:    task.Quality,
		Error:      task.Error,
		CreatedAt:  task.CreatedAt,
		StartedAt:  task.StartedAt,
		FinishedAt: task.FinishedAt,
	}
}

func taskStateFromDownload(state string) string {
	switch state {
	case domain.DownloadStateQueued, domain.DownloadStateActive:
		return domain.TaskStateActive
	case domain.DownloadStateDone:
		return domain.TaskStateDone
	case domain.DownloadStateCanceled:
		return domain.TaskStateCanceled
	default:
		return domain.TaskStateFailed
	}
}

func taskStateFromPromotion(state string) string {
	switch state {
	case domain.PromotionStateQueued, domain.PromotionStateRunning:
		return domain.TaskStateActive
	case domain.PromotionStateDone:
		return domain.TaskStateDone
	default:
		return domain.TaskStateFailed
	}
}
