package sqlite

import (
	"context"
	"log/slog"
	"testing"

	"airmedy/internal/domain"
)

func newPluginKVTestRepo(t *testing.T) domain.PluginKVRepository {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPluginKVRepository(db)
}

func TestPluginKVRepository_MissingKeyIsNotFoundNotAnError(t *testing.T) {
	repo := newPluginKVTestRepo(t)
	ctx := context.Background()

	value, found, err := repo.Get(ctx, "plug", "absent")
	if err != nil {
		t.Fatalf("Get on a missing key: %v", err)
	}
	if found || value != "" {
		t.Fatalf("Get = (%q, %v), want (\"\", false)", value, found)
	}
}

func TestPluginKVRepository_SetIsAnUpsert(t *testing.T) {
	repo := newPluginKVTestRepo(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "plug", "token", `{"v":"first"}`); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := repo.Set(ctx, "plug", "token", `{"v":"second"}`); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}

	value, found, err := repo.Get(ctx, "plug", "token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || value != `{"v":"second"}` {
		t.Fatalf("Get = (%q, %v), want the second value", value, found)
	}

	count, err := repo.Count(ctx, "plug")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("Count = %d, want 1 after an overwrite", count)
	}
}

// The isolation rule: two plugins writing the same key must not see each
// other's value. This is the whole reason the store takes a plugin id.
func TestPluginKVRepository_IsolatesPlugins(t *testing.T) {
	repo := newPluginKVTestRepo(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "alpha", "shared", "a"); err != nil {
		t.Fatalf("Set alpha: %v", err)
	}
	if err := repo.Set(ctx, "beta", "shared", "b"); err != nil {
		t.Fatalf("Set beta: %v", err)
	}

	alpha, found, err := repo.Get(ctx, "alpha", "shared")
	if err != nil || !found || alpha != "a" {
		t.Fatalf("alpha Get = (%q, %v, %v), want (\"a\", true, nil)", alpha, found, err)
	}

	keys, err := repo.Keys(ctx, "beta")
	if err != nil {
		t.Fatalf("Keys beta: %v", err)
	}
	if len(keys) != 1 || keys[0] != "shared" {
		t.Fatalf("Keys beta = %v, want [shared]", keys)
	}

	if count, err := repo.Count(ctx, "gamma"); err != nil || count != 0 {
		t.Fatalf("Count gamma = (%d, %v), want (0, nil)", count, err)
	}
}

func TestPluginKVRepository_DeleteIsIdempotent(t *testing.T) {
	repo := newPluginKVTestRepo(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "plug", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	deleted, err := repo.Delete(ctx, "plug", "k")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !deleted {
		t.Fatal("Delete = false, want true for an existing key")
	}

	deleted, err = repo.Delete(ctx, "plug", "k")
	if err != nil {
		t.Fatalf("Delete again: %v", err)
	}
	if deleted {
		t.Fatal("Delete = true, want false for an absent key")
	}
}

func TestPluginKVRepository_KeysAreSorted(t *testing.T) {
	repo := newPluginKVTestRepo(t)
	ctx := context.Background()

	for _, key := range []string{"z", "a", "m"} {
		if err := repo.Set(ctx, "plug", key, "v"); err != nil {
			t.Fatalf("Set %q: %v", key, err)
		}
	}

	keys, err := repo.Keys(ctx, "plug")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	want := []string{"a", "m", "z"}
	if len(keys) != len(want) {
		t.Fatalf("Keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("Keys = %v, want %v", keys, want)
		}
	}
}
