package sqlite

import (
	"context"
	"fmt"

	"airmedy/internal/domain"
)

type pluginKVRepository struct {
	db *DB
}

// NewPluginKVRepository returns the SQLite-backed plugin key/value store.
func NewPluginKVRepository(db *DB) domain.PluginKVRepository {
	return &pluginKVRepository{db: db}
}

// Get returns one value. A missing key is (empty, false, nil), not an error:
// `storage.get` reports `found` instead of a failure, because "never written"
// is the normal state of a plugin's first run.
func (r *pluginKVRepository) Get(ctx context.Context, pluginID, key string) (string, bool, error) {
	var rows []struct {
		Value string `db:"value"`
	}
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT value FROM plugin_kv WHERE plugin_id = ? AND key = ?`,
		pluginID, key,
	); err != nil {
		return "", false, fmt.Errorf("failed to read plugin kv %q/%q: %w", pluginID, key, err)
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0].Value, true, nil
}

// Set writes one value, inserting the row when absent.
func (r *pluginKVRepository) Set(ctx context.Context, pluginID, key, value string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO plugin_kv (plugin_id, key, value, updated_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(plugin_id, key) DO UPDATE SET
		   value = excluded.value,
		   updated_at = excluded.updated_at`,
		pluginID, key, value,
	)
	if err != nil {
		return fmt.Errorf("failed to write plugin kv %q/%q: %w", pluginID, key, err)
	}
	return nil
}

// Delete removes one key and reports whether a row existed.
func (r *pluginKVRepository) Delete(ctx context.Context, pluginID, key string) (bool, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM plugin_kv WHERE plugin_id = ? AND key = ?`,
		pluginID, key,
	)
	if err != nil {
		return false, fmt.Errorf("failed to delete plugin kv %q/%q: %w", pluginID, key, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to delete plugin kv %q/%q: %w", pluginID, key, err)
	}
	return removed > 0, nil
}

// Keys lists the plugin's keys in ascending order. The order is part of the
// contract: `storage.keys` promises a stable listing, so a plugin can diff two
// answers without sorting them itself.
func (r *pluginKVRepository) Keys(ctx context.Context, pluginID string) ([]string, error) {
	keys := []string{}
	if err := r.db.SelectContext(ctx, &keys,
		`SELECT key FROM plugin_kv WHERE plugin_id = ? ORDER BY key`,
		pluginID,
	); err != nil {
		return nil, fmt.Errorf("failed to list plugin kv keys for %q: %w", pluginID, err)
	}
	return keys, nil
}

// Count returns how many keys the plugin stores.
func (r *pluginKVRepository) Count(ctx context.Context, pluginID string) (int, error) {
	var count int
	if err := r.db.GetContext(ctx, &count,
		`SELECT COUNT(*) FROM plugin_kv WHERE plugin_id = ?`,
		pluginID,
	); err != nil {
		return 0, fmt.Errorf("failed to count plugin kv keys for %q: %w", pluginID, err)
	}
	return count, nil
}
