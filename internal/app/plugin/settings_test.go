package plugin

import (
	"context"
	"testing"
)

type memoryKV struct {
	values map[string]map[string]string
}

func newMemoryKV() *memoryKV {
	return &memoryKV{values: make(map[string]map[string]string)}
}

func (m *memoryKV) Get(_ context.Context, pluginID, key string) (string, bool, error) {
	value, ok := m.values[pluginID][key]
	return value, ok, nil
}

func (m *memoryKV) Set(_ context.Context, pluginID, key, value string) error {
	if m.values[pluginID] == nil {
		m.values[pluginID] = make(map[string]string)
	}
	m.values[pluginID][key] = value
	return nil
}

func (m *memoryKV) Delete(_ context.Context, pluginID, key string) (bool, error) {
	if _, ok := m.values[pluginID][key]; !ok {
		return false, nil
	}
	delete(m.values[pluginID], key)
	return true, nil
}

func (m *memoryKV) Keys(_ context.Context, pluginID string) ([]string, error) {
	keys := make([]string, 0, len(m.values[pluginID]))
	for key := range m.values[pluginID] {
		keys = append(keys, key)
	}
	return keys, nil
}

func (m *memoryKV) Count(_ context.Context, pluginID string) (int, error) {
	return len(m.values[pluginID]), nil
}

func TestSettingStore_RoundTripsASwitchInThePluginKV(t *testing.T) {
	store := NewSettingStore(newMemoryKV())

	if got := store.GetBool(context.Background(), "wy", "cover_in_results", true); got != true {
		t.Fatalf("default = %v, want true", got)
	}
	if err := store.Set(context.Background(), "wy", "cover_in_results", "false"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.GetBool(context.Background(), "wy", "cover_in_results", true); got != false {
		t.Fatalf("stored value = %v, want false", got)
	}

	// The host prefix keeps a setting from colliding with a plugin-owned key.
	if value, found, _ := store.Get(context.Background(), "wy", "cover_in_results"); !found ||
		value != "false" {
		t.Fatalf("raw value = %q found=%v", value, found)
	}
}

func TestSettingStore_WithoutKVReportsUnavailable(t *testing.T) {
	if _, _, err := NewSettingStore(nil).Get(context.Background(), "wy", "cover_in_results"); err == nil {
		t.Fatal("Get = nil error, want ErrPluginSettingsUnavailable")
	}
	if err := NewSettingStore(nil).Set(context.Background(), "wy", "cover_in_results", "true"); err == nil {
		t.Fatal("Set = nil error, want ErrPluginSettingsUnavailable")
	}
}
