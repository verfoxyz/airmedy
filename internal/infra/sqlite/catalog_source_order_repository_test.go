package sqlite

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"airmedy/internal/domain"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func newSourceOrderStore(t *testing.T) (*DB, domain.CatalogSourceOrderRepository) {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewCatalogSourceOrderRepository(db)
}

func TestSourceOrderRepositorySavesAndLoads(t *testing.T) {
	_, repo := newSourceOrderStore(t)
	ctx := context.Background()

	preferences, err := repo.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(preferences.Order) != 0 || len(preferences.Disabled) != 0 {
		t.Fatalf("preferences = %+v, want empty defaults", preferences)
	}

	if err := repo.SaveOrder(ctx, []string{"wy", "kw", "kg"}); err != nil {
		t.Fatalf("SaveOrder: %v", err)
	}
	preferences, err = repo.Load(ctx)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if !reflect.DeepEqual(preferences.Order, []string{"wy", "kw", "kg"}) {
		t.Fatalf("order = %v, want the saved list", preferences.Order)
	}
}

// The order and the disabled set are separate writes: neither may replace the
// other's value.
func TestSourceOrderAndDisabledAreIndependent(t *testing.T) {
	_, repo := newSourceOrderStore(t)
	ctx := context.Background()

	if err := repo.SaveOrder(ctx, []string{"wy", "kg"}); err != nil {
		t.Fatalf("SaveOrder: %v", err)
	}
	if err := repo.SetDisabled(ctx, "kg", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	preferences, err := repo.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(preferences.Order, []string{"wy", "kg"}) {
		t.Fatalf("order = %v, want it untouched by the toggle", preferences.Order)
	}
	if !reflect.DeepEqual(preferences.Disabled, []string{"kg"}) {
		t.Fatalf("disabled = %v, want [kg]", preferences.Disabled)
	}

	// A later order save keeps the disabled source switched off.
	if err := repo.SaveOrder(ctx, []string{"kg", "wy"}); err != nil {
		t.Fatalf("SaveOrder after disable: %v", err)
	}
	preferences, err = repo.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(preferences.Disabled, []string{"kg"}) {
		t.Fatalf("disabled = %v, want the toggle preserved", preferences.Disabled)
	}

	// Toggling back on removes it without touching the order.
	if err := repo.SetDisabled(ctx, "kg", false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	preferences, err = repo.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(preferences.Disabled) != 0 {
		t.Fatalf("disabled = %v, want empty", preferences.Disabled)
	}
	if !reflect.DeepEqual(preferences.Order, []string{"kg", "wy"}) {
		t.Fatalf("order = %v, want it untouched", preferences.Order)
	}
}

// A settings-page save rewrites the row through SettingsRepository; both
// preference columns live outside its column list and must survive.
func TestSourcePreferencesSurviveASettingsSave(t *testing.T) {
	db, repo := newSourceOrderStore(t)
	ctx := context.Background()
	settings := NewSettingsRepository(db)

	if err := repo.SaveOrder(ctx, []string{"kg", "wy"}); err != nil {
		t.Fatalf("SaveOrder: %v", err)
	}
	if err := repo.SetDisabled(ctx, "wy", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	loaded, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("Load settings: %v", err)
	}
	loaded.Language = "de"
	if err := settings.Save(ctx, loaded); err != nil {
		t.Fatalf("Save settings: %v", err)
	}

	preferences, err := repo.Load(ctx)
	if err != nil {
		t.Fatalf("Load preferences: %v", err)
	}
	if !reflect.DeepEqual(preferences.Order, []string{"kg", "wy"}) {
		t.Fatalf("order = %v, want it untouched", preferences.Order)
	}
	if !reflect.DeepEqual(preferences.Disabled, []string{"wy"}) {
		t.Fatalf("disabled = %v, want it untouched", preferences.Disabled)
	}
}

func TestCatalogDisabledSourcesMigrationDownAndUp(t *testing.T) {
	db, _ := newSourceOrderStore(t)

	hasColumn := func(name string) bool {
		var columns []string
		if err := db.Select(&columns, `SELECT name FROM pragma_table_info('app_settings')`); err != nil {
			t.Fatalf("pragma_table_info: %v", err)
		}
		for _, column := range columns {
			if column == name {
				return true
			}
		}
		return false
	}

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

	if !hasColumn("catalog_source_order") || !hasColumn("catalog_disabled_sources") {
		t.Fatal("the up migrations did not add both preference columns")
	}
	if err := m.Migrate(111); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if hasColumn("catalog_disabled_sources") {
		t.Fatal("the down migration kept catalog_disabled_sources")
	}
	if !hasColumn("catalog_source_order") {
		t.Fatal("the 000112 down migration dropped the order column too")
	}
	if err := m.Up(); err != nil {
		t.Fatalf("re-applying up migrations failed: %v", err)
	}
	if !hasColumn("catalog_disabled_sources") {
		t.Fatal("the up migration did not recreate catalog_disabled_sources")
	}
}
