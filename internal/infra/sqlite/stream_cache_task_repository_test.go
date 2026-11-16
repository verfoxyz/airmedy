package sqlite

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func newStreamCacheTaskRepo(t *testing.T) domain.StreamCacheTaskRepository {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return NewStreamCacheTaskRepository(db)
}

func cacheTask(id, title string, started time.Time) *domain.StreamCacheTask {
	return &domain.StreamCacheTask{
		ID: id, Provider: "wy", ExternalID: id, Title: title, Artist: "歌手",
		Quality: "320k", StartedAt: started,
	}
}

func TestStreamCacheTaskRepository_StartFinishAndFail(t *testing.T) {
	repo := newStreamCacheTaskRepo(t)
	ctx := context.Background()
	now := time.Now()

	if err := repo.Start(ctx, cacheTask("done-1", "缓存曲", now)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tasks, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != domain.StreamCacheTaskStateActive {
		t.Fatalf("started row = %+v, want one active row", tasks)
	}

	if err := repo.Finish(ctx, "done-1", 2048, 2048, now.Add(time.Minute)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := repo.Start(ctx, cacheTask("failed-1", "失败曲", now.Add(2*time.Minute))); err != nil {
		t.Fatalf("Start failed row: %v", err)
	}
	if err := repo.Fail(ctx, "failed-1", "no source", 0, 0, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	tasks, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]*domain.StreamCacheTask{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	if got := byID["done-1"]; got == nil || got.State != domain.StreamCacheTaskStateDone ||
		got.BytesTotal != 2048 || got.BytesDone != 2048 || got.FinishedAt == nil {
		t.Fatalf("finished row = %+v", got)
	}
	if got := byID["failed-1"]; got == nil || got.State != domain.StreamCacheTaskStateFailed ||
		got.Error != "no source" || got.FinishedAt == nil {
		t.Fatalf("failed row = %+v", got)
	}
}

func TestStreamCacheTaskRepository_MarkActiveInterruptedLeavesFinishedRows(t *testing.T) {
	repo := newStreamCacheTaskRepo(t)
	ctx := context.Background()
	now := time.Now()

	if err := repo.Start(ctx, cacheTask("active-1", "进行中", now)); err != nil {
		t.Fatalf("Start active: %v", err)
	}
	if err := repo.Start(ctx, cacheTask("done-1", "已完成", now)); err != nil {
		t.Fatalf("Start done: %v", err)
	}
	if err := repo.Finish(ctx, "done-1", 100, 100, now.Add(time.Minute)); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	changed, err := repo.MarkActiveInterrupted(ctx, "interrupted", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("MarkActiveInterrupted: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}

	tasks, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, task := range tasks {
		switch task.ID {
		case "active-1":
			if task.State != domain.StreamCacheTaskStateCanceled || task.Error != "" {
				t.Fatalf("interrupted row = %+v, want canceled without an error", task)
			}
		case "done-1":
			if task.State != domain.StreamCacheTaskStateDone {
				t.Fatalf("finished row was changed: %+v", task)
			}
		}
	}
}

// TestStreamCacheTaskRepository_CancelRecordsNoError: an intentional stop is
// history, not a failure.
func TestStreamCacheTaskRepository_CancelRecordsNoError(t *testing.T) {
	repo := newStreamCacheTaskRepo(t)
	ctx := context.Background()
	now := time.Now()

	if err := repo.Start(ctx, cacheTask("active-1", "进行中", now)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := repo.Cancel(ctx, "active-1", 1000, 400, now.Add(time.Second)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	got := byID(t, repo)["active-1"]
	if got == nil || got.State != domain.StreamCacheTaskStateCanceled ||
		got.Error != "" || got.BytesDone != 400 || got.FinishedAt == nil {
		t.Fatalf("canceled row = %+v", got)
	}
}

func byID(t *testing.T, repo domain.StreamCacheTaskRepository) map[string]*domain.StreamCacheTask {
	t.Helper()
	tasks, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	rows := make(map[string]*domain.StreamCacheTask, len(tasks))
	for _, task := range tasks {
		rows[task.ID] = task
	}
	return rows
}

func TestStreamCacheTaskRepository_TrimFinishedKeepsNewest(t *testing.T) {
	repo := newStreamCacheTaskRepo(t)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 3; i++ {
		id := "done-" + string(rune('a'+i))
		if err := repo.Start(ctx, cacheTask(id, id, now)); err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
		if err := repo.Finish(ctx, id, 1, 1, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Finish %s: %v", id, err)
		}
	}
	if err := repo.Start(ctx, cacheTask("active-1", "进行中", now)); err != nil {
		t.Fatalf("Start active: %v", err)
	}

	deleted, err := repo.TrimFinished(ctx, 1)
	if err != nil {
		t.Fatalf("TrimFinished: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	tasks, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("remaining = %v, want the newest finished row and the active one", ids)
	}

	deleted, err = repo.TrimFinished(ctx, 0)
	if err != nil {
		t.Fatalf("TrimFinished(0): %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	tasks, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "active-1" {
		t.Fatalf("remaining = %+v, want only the active row", tasks)
	}

	// A canceled row is history too: clear-history removes it.
	if err := repo.Start(ctx, cacheTask("canceled-1", "已取消", now)); err != nil {
		t.Fatalf("Start canceled: %v", err)
	}
	if err := repo.Cancel(ctx, "canceled-1", 10, 4, now.Add(time.Second)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	deleted, err = repo.TrimFinished(ctx, 0)
	if err != nil {
		t.Fatalf("TrimFinished after cancel: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want the canceled row", deleted)
	}
}
