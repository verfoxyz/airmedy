package sqlite

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"airmedy/internal/domain"
)

func newLxSourceTestRepo(t *testing.T) domain.LxSourceRepository {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewLxSourceRepository(db)
}

// TestLxSourceRepository_Empty mirrors acceptance case 1's starting point: a
// fresh install has no sources, and List answers an empty slice rather than nil
// so the UI can render an empty state without a nil check.
func TestLxSourceRepository_Empty(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	sources, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("List = %+v, want no sources", sources)
	}
}

// TestLxSourceRepository_CreateAndList covers acceptance case 1's storage half:
// an imported source is stored with the file path the core chose, enabled by
// default, and the position it was assigned.
func TestLxSourceRepository_CreateAndList(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	ctx := context.Background()

	first := domain.LxSource{ID: "a1", Name: "ikun", FilePath: "/data/lx/sources/a1.js", Enabled: true, Position: 0}
	second := domain.LxSource{ID: "b2", Name: "another", FilePath: "/data/lx/sources/b2.js", Enabled: true, Position: 1}
	for _, source := range []domain.LxSource{first, second} {
		if err := repo.Create(ctx, source); err != nil {
			t.Fatalf("Create(%s): %v", source.ID, err)
		}
	}

	sources, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("List = %+v, want two sources", sources)
	}
	// Order is the try order, which is what the plugin walks.
	if sources[0].ID != "a1" || sources[1].ID != "b2" {
		t.Fatalf("order = %s, %s; want a1, b2", sources[0].ID, sources[1].ID)
	}
	if !sources[0].Enabled {
		t.Fatalf("a new source must be enabled by default")
	}
	if sources[0].FilePath != first.FilePath {
		t.Fatalf("FilePath = %q, want the stored copy", sources[0].FilePath)
	}
}

// TestLxSourceRepository_DuplicateIDIsRejected: two rows with one id would make
// the environment ambiguous, so the insert fails instead of overwriting.
func TestLxSourceRepository_DuplicateIDIsRejected(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	ctx := context.Background()
	source := domain.LxSource{ID: "dup", Name: "x", FilePath: "/x.js", Enabled: true}
	if err := repo.Create(ctx, source); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Create(ctx, source); err == nil {
		t.Fatal("a duplicate id must be rejected")
	}
}

// TestLxSourceRepository_UpdateToggles: the enable flag is persisted and survives
// a reload, which is what "the source stays disabled across restarts" means.
func TestLxSourceRepository_UpdateToggles(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	ctx := context.Background()
	source := domain.LxSource{ID: "a1", Name: "ikun", FilePath: "/a1.js", Enabled: true}
	if err := repo.Create(ctx, source); err != nil {
		t.Fatalf("Create: %v", err)
	}

	source.Enabled = false
	source.Name = "renamed"
	if err := repo.Update(ctx, source); err != nil {
		t.Fatalf("Update: %v", err)
	}

	sources, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if sources[0].Enabled {
		t.Fatal("the disabled flag must be persisted")
	}
	if sources[0].Name != "renamed" {
		t.Fatalf("Name = %q, want the rename", sources[0].Name)
	}
	if sources[0].UpdatedAt.IsZero() {
		t.Fatal("Update must refresh updated_at")
	}
}

// TestLxSourceRepository_UpdateUnknownIDReportsNotFound keeps a stale UI row from
// silently succeeding.
func TestLxSourceRepository_UpdateUnknownIDReportsNotFound(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	err := repo.Update(context.Background(), domain.LxSource{ID: "ghost", Name: "x"})
	if !errors.Is(err, domain.ErrLxSourceNotFound) {
		t.Fatalf("Update(ghost) = %v, want ErrLxSourceNotFound", err)
	}
}

// TestLxSourceRepository_ReorderRewritesPositions is acceptance case 9's storage
// half: after a reorder, List answers the new order and the positions are a
// contiguous 0..n-1 range.
func TestLxSourceRepository_ReorderRewritesPositions(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	ctx := context.Background()
	for index, id := range []string{"a", "b", "c"} {
		if err := repo.Create(ctx, domain.LxSource{
			ID: id, Name: id, FilePath: "/" + id + ".js", Enabled: true, Position: index,
		}); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	if err := repo.Reorder(ctx, []string{"c", "a", "b"}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}

	sources, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"c", "a", "b"}
	for index, source := range sources {
		if source.ID != want[index] {
			t.Fatalf("position %d = %s, want %s", index, source.ID, want[index])
		}
		if source.Position != index {
			t.Fatalf("%s position = %d, want %d", source.ID, source.Position, index)
		}
	}
}

// TestLxSourceRepository_ReorderIgnoresUnknownIDs: a stale UI list (a source
// removed in another window) must not fail the whole reorder.
func TestLxSourceRepository_ReorderIgnoresUnknownIDs(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	ctx := context.Background()
	if err := repo.Create(ctx, domain.LxSource{ID: "a", Name: "a", FilePath: "/a.js", Enabled: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Reorder(ctx, []string{"ghost", "a"}); err != nil {
		t.Fatalf("Reorder with an unknown id: %v", err)
	}
	sources, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if sources[0].Position != 0 {
		t.Fatalf("a position = %d, want 0", sources[0].Position)
	}
}

// TestLxSourceRepository_DeleteIsIdempotent: the row may already be gone when the
// user clicks delete twice.
func TestLxSourceRepository_DeleteIsIdempotent(t *testing.T) {
	repo := newLxSourceTestRepo(t)
	ctx := context.Background()
	if err := repo.Create(ctx, domain.LxSource{ID: "a", Name: "a", FilePath: "/a.js", Enabled: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := repo.Delete(ctx, "a"); err != nil {
			t.Fatalf("Delete %d: %v", i, err)
		}
	}
	sources, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("List = %+v, want empty", sources)
	}
}

// TestLxSourceRepository_MigrationHasADownPath: AGENTS.md requires every schema
// change to be reversible, and a drop table is the only honest down for a new
// table.
func TestLxSourceRepository_MigrationHasADownPath(t *testing.T) {
	up, err := migrationsFS.ReadFile("migrations/000081_lx_sources.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	down, err := migrationsFS.ReadFile("migrations/000081_lx_sources.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	if len(up) == 0 || len(down) == 0 {
		t.Fatal("both migration files must have content")
	}
}
