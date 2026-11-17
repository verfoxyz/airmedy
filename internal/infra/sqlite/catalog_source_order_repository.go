package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"airmedy/internal/domain"
)

type catalogSourceOrderRepository struct {
	db *DB
}

// NewCatalogSourceOrderRepository returns the persisted search source order and
// enable flags.
func NewCatalogSourceOrderRepository(db *DB) domain.CatalogSourceOrderRepository {
	return &catalogSourceOrderRepository{db: db}
}

// Load returns both preferences. A fresh database has the migrations' '[]'
// defaults; a missing settings row is the same empty preference.
func (r *catalogSourceOrderRepository) Load(
	ctx context.Context,
) (domain.CatalogSourcePreferences, error) {
	var row struct {
		Order    string `db:"catalog_source_order"`
		Disabled string `db:"catalog_disabled_sources"`
	}
	err := r.db.GetContext(ctx, &row,
		`SELECT catalog_source_order, catalog_disabled_sources
		 FROM app_settings WHERE id = 1`)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CatalogSourcePreferences{Order: []string{}, Disabled: []string{}}, nil
	}
	if err != nil {
		return domain.CatalogSourcePreferences{}, fmt.Errorf("catalog source order: load: %w", err)
	}
	return domain.CatalogSourcePreferences{
		Order:    domain.DecodeCatalogSourceOrder(row.Order),
		Disabled: domain.DecodeDisabledSources(row.Disabled),
	}, nil
}

// SaveOrder replaces the order without touching the disabled set or the other
// settings columns, so a later SaveSettings cannot erase it either.
func (r *catalogSourceOrderRepository) SaveOrder(ctx context.Context, order []string) error {
	raw, err := domain.EncodeCatalogSourceOrder(order)
	if err != nil {
		return fmt.Errorf("catalog source order: encode: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO app_settings (id, catalog_source_order, updated_at)
		 VALUES (1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		   catalog_source_order = excluded.catalog_source_order,
		   updated_at = excluded.updated_at`, raw,
	); err != nil {
		return fmt.Errorf("catalog source order: save: %w", err)
	}
	return nil
}

// SetDisabled adds or removes one provider from the disabled set. The read and
// the write share a transaction so two quick toggles cannot drop each other.
func (r *catalogSourceOrderRepository) SetDisabled(
	ctx context.Context, provider string, disabled bool,
) error {
	provider = domain.NormalizeProvider(provider)
	if provider == "" {
		return fmt.Errorf("catalog source order: empty provider")
	}
	return r.db.RunTx(ctx, func(ctx context.Context) error {
		exec := r.db.Ext(ctx)
		var row struct {
			Disabled string `db:"catalog_disabled_sources"`
		}
		err := exec.GetContext(ctx, &row,
			`SELECT catalog_disabled_sources FROM app_settings WHERE id = 1`)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("catalog source order: read disabled: %w", err)
		}

		current := domain.DecodeDisabledSources(row.Disabled)
		next := make([]string, 0, len(current)+1)
		found := false
		for _, entry := range current {
			key := domain.NormalizeProvider(entry)
			if key == provider {
				found = true
				if disabled {
					next = append(next, provider)
				}
				continue
			}
			if key != "" {
				next = append(next, key)
			}
		}
		if disabled && !found {
			next = append(next, provider)
		}
		encoded, err := domain.EncodeDisabledSources(next)
		if err != nil {
			return fmt.Errorf("catalog source order: encode disabled: %w", err)
		}
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO app_settings (id, catalog_disabled_sources, updated_at)
			 VALUES (1, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(id) DO UPDATE SET
			   catalog_disabled_sources = excluded.catalog_disabled_sources,
			   updated_at = excluded.updated_at`, encoded,
		); err != nil {
			return fmt.Errorf("catalog source order: save disabled: %w", err)
		}
		return nil
	})
}
