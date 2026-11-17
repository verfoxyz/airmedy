package domain

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// CatalogSourcePreferences is the persisted shape of the search source
// settings: the order the user arranged and the sources they switched off.
//
// Disabled is a negative list on purpose: a newly installed source is enabled
// without the user doing anything, and an old row without the preference simply
// has an empty list.
type CatalogSourcePreferences struct {
	Order    []string
	Disabled []string
}

// CatalogSourceOrderRepository persists the search source order and the
// per-source enable flags.
//
// Both live in the same settings row but are written through separate methods,
// so saving an order never re-enables a source and toggling a source never
// reorders the list. A disabled source stays in Order: re-enabling it returns
// to its old position.
type CatalogSourceOrderRepository interface {
	// Load returns the stored preferences. A missing or unreadable row is an
	// empty preference, not an error.
	Load(ctx context.Context) (CatalogSourcePreferences, error)
	// SaveOrder replaces the order and leaves the disabled set untouched.
	SaveOrder(ctx context.Context, order []string) error
	// SetDisabled switches one provider off or on, adding it to / removing it
	// from the disabled set without touching the order.
	SetDisabled(ctx context.Context, provider string, disabled bool) error
}

// NormalizeCatalogSourceOrder resolves a stored order against the sources that
// currently exist.
//
// Unknown, empty and duplicate providers are dropped; available sources the
// stored order does not mention are appended in alphabetical order, so
// re-enabling a source puts it at the end instead of failing. An empty order
// therefore means "alphabetical", which is the historical behaviour.
func NormalizeCatalogSourceOrder(stored, available []string) []string {
	known := make(map[string]struct{}, len(available))
	for _, provider := range available {
		if key := NormalizeProvider(provider); key != "" {
			known[key] = struct{}{}
		}
	}
	missing := make([]string, 0, len(known))
	for provider := range known {
		missing = append(missing, provider)
	}
	sort.Strings(missing)

	result := make([]string, 0, len(known))
	seen := make(map[string]struct{}, len(known))
	for _, provider := range stored {
		key := NormalizeProvider(provider)
		if key == "" {
			continue
		}
		if _, exists := known[key]; !exists {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	for _, provider := range missing {
		if _, duplicate := seen[provider]; duplicate {
			continue
		}
		seen[provider] = struct{}{}
		result = append(result, provider)
	}
	return result
}

// EncodeCatalogSourceOrder serializes an order for storage.
func EncodeCatalogSourceOrder(order []string) (string, error) {
	if order == nil {
		order = []string{}
	}
	encoded, err := json.Marshal(order)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// DecodeCatalogSourceOrder parses a stored order. An empty or malformed value
// is an empty order, not an error: the caller normalizes it against the
// available sources, which is the only place the meaning is known.
func DecodeCatalogSourceOrder(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var order []string
	if err := json.Unmarshal([]byte(raw), &order); err != nil {
		return nil
	}
	return order
}

// NormalizeDisabledSources resolves the stored disabled set against the sources
// that currently exist. Unknown providers are dropped, so uninstalling a plugin
// does not leave a permanent exclusion behind.
func NormalizeDisabledSources(stored, available []string) []string {
	known := make(map[string]struct{}, len(available))
	for _, provider := range available {
		if key := NormalizeProvider(provider); key != "" {
			known[key] = struct{}{}
		}
	}
	result := make([]string, 0, len(stored))
	seen := make(map[string]struct{}, len(stored))
	for _, provider := range stored {
		key := NormalizeProvider(provider)
		if key == "" {
			continue
		}
		if _, exists := known[key]; !exists {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// EncodeDisabledSources serializes a disabled set for storage.
func EncodeDisabledSources(providers []string) (string, error) {
	if providers == nil {
		providers = []string{}
	}
	encoded, err := json.Marshal(providers)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// DecodeDisabledSources parses a stored disabled set. A malformed value is an
// empty set: the caller normalizes it against the installed sources anyway.
func DecodeDisabledSources(raw string) []string {
	return DecodeCatalogSourceOrder(raw)
}
