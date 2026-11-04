package sqlite

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"airmedy/internal/domain"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func newPluginSettingsTestRepo(t *testing.T) domain.PluginSettingsRepository {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPluginSettingsRepository(db)
}

// TestPluginSettingsRepository_DefaultsToDisabled is acceptance case 3: a plugin
// with no row is not enabled. The default is "off" because enabling a plugin
// launches a third-party process.
func TestPluginSettingsRepository_DefaultsToDisabled(t *testing.T) {
	repo := newPluginSettingsTestRepo(t)
	ctx := context.Background()

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List on a fresh database: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("List = %+v, want no rows for a fresh install", rows)
	}
}

func TestPluginSettingsRepository_SetAndList(t *testing.T) {
	repo := newPluginSettingsTestRepo(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "io.airmedy.lx", true); err != nil {
		t.Fatalf("Set enabled: %v", err)
	}
	if err := repo.Set(ctx, "io.airmedy.off", false); err != nil {
		t.Fatalf("Set disabled: %v", err)
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := make(map[string]bool, len(rows))
	for _, row := range rows {
		byID[row.PluginID] = row.Enabled
		if row.UpdatedAt.IsZero() {
			t.Errorf("row %q has no timestamp", row.PluginID)
		}
	}
	if !byID["io.airmedy.lx"] {
		t.Error("enabled plugin lost its flag")
	}
	if enabled, ok := byID["io.airmedy.off"]; !ok || enabled {
		t.Error("an explicit disable must be stored, not omitted")
	}
}

// TestPluginSettingsRepository_SetIsUpsert covers toggling in both directions
// on one row: the UI switch is a Set on every change.
func TestPluginSettingsRepository_SetIsUpsert(t *testing.T) {
	repo := newPluginSettingsTestRepo(t)
	ctx := context.Background()

	for _, want := range []bool{true, false, true} {
		if err := repo.Set(ctx, "io.airmedy.lx", want); err != nil {
			t.Fatalf("Set(%v): %v", want, err)
		}
		rows, err := repo.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %+v, want one row for one plugin", rows)
		}
		if rows[0].Enabled != want {
			t.Fatalf("enabled = %v, want %v", rows[0].Enabled, want)
		}
	}
}

// TestPluginSettingsRepository_RejectsEmptyID keeps a bug from writing a row
// that no plugin can ever read.
func TestPluginSettingsRepository_RejectsEmptyID(t *testing.T) {
	repo := newPluginSettingsTestRepo(t)
	if err := repo.Set(context.Background(), "", true); err == nil {
		t.Fatal("expected an error for an empty plugin id")
	}
}

// TestPluginSettingsMigration_UpDownUp proves the migration is reversible: the
// task contract forbids a destructive migration without a down path.
func TestPluginSettingsMigration_UpDownUp(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	tableExists := func() bool {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'table' AND name = 'plugin_settings'`); err != nil {
			t.Fatalf("failed to inspect schema: %v", err)
		}
		return n > 0
	}

	ctx := context.Background()
	repo := NewPluginSettingsRepository(db)

	driver, err := sqlite3.WithInstance(db.DB.DB, &sqlite3.Config{})
	if err != nil {
		t.Fatalf("failed to create migrate driver: %v", err)
	}
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("failed to create migration source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", driver)
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	const pluginSettingsVersion = 80
	// A fresh database is already at the latest version, so this is a no-op
	// that reports migrate.ErrNoChange; anything else is a real failure.
	if err := m.Migrate(pluginSettingsVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("failed to migrate to %d: %v", pluginSettingsVersion, err)
	}
	if !tableExists() {
		t.Fatal("plugin_settings is missing after the up migration")
	}

	if err := repo.Set(ctx, "io.airmedy.lx", true); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if tableExists() {
		t.Fatal("plugin_settings should be gone after the down migration")
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("re-applying up migration failed: %v", err)
	}
	if !tableExists() {
		t.Fatal("plugin_settings is missing after re-applying the up migration")
	}
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after re-up: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the down migration must drop the data too, got %+v", rows)
	}
}
