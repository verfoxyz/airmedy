package sqlite

import (
	"context"
	"fmt"
	"time"

	"airmedy/internal/domain"
)

type pluginSettingsRepository struct {
	db *DB
}

// NewPluginSettingsRepository returns the SQLite-backed plugin enable flags.
func NewPluginSettingsRepository(db *DB) domain.PluginSettingsRepository {
	return &pluginSettingsRepository{db: db}
}

// List returns every persisted row. Rows for uninstalled plugins are included
// on purpose: a user who removes and later reinstalls a plugin gets their choice
// back, and the registry simply ignores ids it does not know.
func (r *pluginSettingsRepository) List(ctx context.Context) ([]domain.PluginSettings, error) {
	var rows []struct {
		PluginID  string    `db:"plugin_id"`
		Enabled   bool      `db:"enabled"`
		UpdatedAt time.Time `db:"updated_at"`
	}
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT plugin_id, enabled, updated_at FROM plugin_settings ORDER BY plugin_id`,
	); err != nil {
		return nil, fmt.Errorf("failed to list plugin settings: %w", err)
	}

	settings := make([]domain.PluginSettings, 0, len(rows))
	for _, row := range rows {
		settings = append(settings, domain.PluginSettings{
			PluginID:  row.PluginID,
			Enabled:   row.Enabled,
			UpdatedAt: row.UpdatedAt,
		})
	}
	return settings, nil
}

// Set writes one flag, inserting the row when it is absent.
func (r *pluginSettingsRepository) Set(ctx context.Context, pluginID string, enabled bool) error {
	if pluginID == "" {
		return fmt.Errorf("failed to set plugin setting: empty plugin id")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO plugin_settings (plugin_id, enabled, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(plugin_id) DO UPDATE SET
		   enabled = excluded.enabled,
		   updated_at = excluded.updated_at`,
		pluginID,
		enabled,
	)
	if err != nil {
		return fmt.Errorf("failed to set plugin setting %q: %w", pluginID, err)
	}
	return nil
}
