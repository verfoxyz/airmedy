package sqlite

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func newPromotionTaskRepo(t *testing.T) domain.PromotionTaskRepository {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPromotionTaskRepository(db)
}

func promotionRow(t *testing.T, repo domain.PromotionTaskRepository) (state, lastError string, started, finished bool) {
	t.Helper()
	sqliteRepo := repo.(*promotionTaskRepository)
	var row domain.PromotionTask
	if err := sqliteRepo.db.Get(&row, `SELECT * FROM promotion_tasks`); err != nil {
		t.Fatalf("read promotion task: %v", err)
	}
	return row.State, row.Error, row.StartedAt != nil, row.FinishedAt != nil
}

func TestPromotionTaskRepository_TransitionsOneActiveRow(t *testing.T) {
	repo := newPromotionTaskRepo(t)
	ctx := context.Background()
	task := &domain.PromotionTask{
		TrackID: "remote-1", Provider: "wy", ExternalID: "42",
		Title: "晴天", Artist: "周杰伦",
	}

	id, err := repo.EnsureQueued(ctx, task)
	if err != nil {
		t.Fatalf("EnsureQueued: %v", err)
	}
	if id == "" {
		t.Fatal("EnsureQueued returned an empty id")
	}
	// A repeated request while the row is still active reuses it.
	again, err := repo.EnsureQueued(ctx, &domain.PromotionTask{Provider: "wy", ExternalID: "42"})
	if err != nil {
		t.Fatalf("EnsureQueued again: %v", err)
	}
	if again != id {
		t.Fatalf("second EnsureQueued = %q, want the active row %q", again, id)
	}

	if err := repo.MarkRunning(ctx, id, time.Now()); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if state, _, started, _ := promotionRow(t, repo); state != domain.PromotionStateRunning || !started {
		t.Fatalf("state = %q started = %v, want running with a start time", state, started)
	}

	if err := repo.MarkFinished(ctx, id, domain.PromotionStateFailed, "disk full", time.Now()); err != nil {
		t.Fatalf("MarkFinished: %v", err)
	}
	state, lastError, _, finished := promotionRow(t, repo)
	if state != domain.PromotionStateFailed || lastError != "disk full" || !finished {
		t.Fatalf("state = %q error = %q finished = %v", state, lastError, finished)
	}

	// A finished row no longer blocks a new request for the same identity.
	newID, err := repo.EnsureQueued(ctx, &domain.PromotionTask{Provider: "wy", ExternalID: "42"})
	if err != nil {
		t.Fatalf("EnsureQueued after finish: %v", err)
	}
	if newID == id {
		t.Fatal("a finished row must not be reused as the active one")
	}
}

func TestPromotionTaskRepository_ActivePromotionsReportsOnlyUnfinished(t *testing.T) {
	repo := newPromotionTaskRepo(t)
	ctx := context.Background()

	if _, err := repo.EnsureQueued(ctx, &domain.PromotionTask{Provider: "wy", ExternalID: "queued"}); err != nil {
		t.Fatalf("EnsureQueued queued: %v", err)
	}
	runningID, err := repo.EnsureQueued(ctx, &domain.PromotionTask{Provider: "wy", ExternalID: "running"})
	if err != nil {
		t.Fatalf("EnsureQueued running: %v", err)
	}
	if err := repo.MarkRunning(ctx, runningID, time.Now()); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	doneID, err := repo.EnsureQueued(ctx, &domain.PromotionTask{Provider: "wy", ExternalID: "done"})
	if err != nil {
		t.Fatalf("EnsureQueued done: %v", err)
	}
	if err := repo.MarkFinished(ctx, doneID, domain.PromotionStateDone, "", time.Now()); err != nil {
		t.Fatalf("MarkFinished: %v", err)
	}

	active, err := repo.ActivePromotions(ctx, []domain.CatalogIdentity{
		{Provider: "WY", ExternalID: "queued"},
		{Provider: "wy", ExternalID: "running"},
		{Provider: "wy", ExternalID: "done"},
		{Provider: "wy", ExternalID: "unrelated"},
	})
	if err != nil {
		t.Fatalf("ActivePromotions: %v", err)
	}
	if !active[domain.CatalogIdentity{Provider: "wy", ExternalID: "queued"}] {
		t.Fatal("a queued promotion must be active")
	}
	if !active[domain.CatalogIdentity{Provider: "wy", ExternalID: "running"}] {
		t.Fatal("a running promotion must be active")
	}
	if active[domain.CatalogIdentity{Provider: "wy", ExternalID: "done"}] {
		t.Fatal("a finished promotion is history, not active work")
	}
	if active[domain.CatalogIdentity{Provider: "wy", ExternalID: "unrelated"}] {
		t.Fatal("an identity with no promotion must not be reported")
	}
}
