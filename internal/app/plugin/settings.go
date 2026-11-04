package plugin

import (
	"context"
	"fmt"

	"airmedy/internal/domain"
)

// SettingKeyPrefix namespaces host-owned plugin settings inside the plugin's KV
// space. The prefix keeps a host setting from colliding with a key the plugin
// writes itself.
const SettingKeyPrefix = "host.setting."

// SettingStore persists the values of settings a plugin declared through
// describe.
//
// The host owns the value, not the plugin: the plugin is stateless and receives
// the value on the call it affects. Storage reuses the plugin KV repository so
// one plugin cannot read or write another's settings.
type SettingStore struct {
	kv domain.PluginKVRepository
}

// NewSettingStore builds the store over the host's plugin KV repository.
func NewSettingStore(kv domain.PluginKVRepository) *SettingStore {
	return &SettingStore{kv: kv}
}

// Get reads one setting value. found is false when the user never changed it.
func (s *SettingStore) Get(ctx context.Context, pluginID, key string) (value string, found bool, err error) {
	if s == nil || s.kv == nil || pluginID == "" || key == "" {
		return "", false, domain.ErrPluginSettingsUnavailable
	}
	return s.kv.Get(ctx, pluginID, SettingKeyPrefix+key)
}

// Set writes one setting value.
func (s *SettingStore) Set(ctx context.Context, pluginID, key, value string) error {
	if s == nil || s.kv == nil || pluginID == "" || key == "" {
		return domain.ErrPluginSettingsUnavailable
	}
	if err := s.kv.Set(ctx, pluginID, SettingKeyPrefix+key, value); err != nil {
		return fmt.Errorf("set plugin %q setting %q: %w", pluginID, key, err)
	}
	return nil
}

// GetBool reads a switch value, falling back to fallback when the user never
// changed it or the store is unavailable.
func (s *SettingStore) GetBool(ctx context.Context, pluginID, key string, fallback bool) bool {
	value, found, err := s.Get(ctx, pluginID, key)
	if err != nil || !found {
		return fallback
	}
	return value == "true"
}
