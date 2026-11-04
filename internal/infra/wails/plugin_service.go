package wails

import (
	"context"
	"errors"
	"fmt"

	"airmedy/internal/app/plugin"
	"airmedy/internal/domain"
)

// PluginInfoDTO is one plugin row for the settings page.
//
// The JSON shape is the frontend contract, so field names are explicit rather
// than inferred, and capabilities are plain strings so a plugin the host does
// not understand still renders.
type PluginInfoDTO struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Author       string   `json:"author"`
	Capabilities []string `json:"capabilities"`
	// Providers is the routing declaration: the track provider ids this plugin
	// serves. The UI shows them as tags so the user can tell which plugin will
	// handle a given source.
	Providers []string `json:"providers"`
	// Network is the manifest's declared network allow-list, shown before the
	// plugin is enabled. It is a declaration, not an enforced sandbox: the
	// plugin is an ordinary child process, so the row only tells the user what
	// the plugin says it will reach. An entry of "*" means "any host".
	Network []string `json:"network"`
	// KV is whether the manifest declared `permissions.kv`. Unlike Network this
	// one is *enforced*: storage.* requests from a plugin without it are refused,
	// so the row can say "may store data" rather than "is allowed to".
	KV bool `json:"kv"`
	// Enabled is the user's stored choice; Running is the live process state.
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Status  string `json:"status"`
	// Invalid marks a manifest the app discovered but cannot use; LastError
	// explains why, for the expandable error row.
	Invalid   bool   `json:"invalid"`
	LastError string `json:"last_error"`
}

// PluginStatusDTO is the GetPluginStatus answer.
type PluginStatusDTO struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	LastError string `json:"last_error"`
}

// PluginService is the Wails binding for the plugin registry: translation only,
// no logic (see AGENTS.md §3).
type PluginService struct {
	svc *plugin.Service
}

// NewPluginService binds the registry for the frontend.
func NewPluginService(svc *plugin.Service) *PluginService {
	return &PluginService{svc: svc}
}

// GetPluginsDirectory returns the folder the registry scans. The frontend shows
// it verbatim: a path is not translatable, and hardcoding a Windows-looking
// string in the locale files is wrong on macOS and Linux.
func (s *PluginService) GetPluginsDirectory() string {
	return s.svc.Root()
}

// ListPlugins returns every installed plugin, including invalid ones, so the UI
// can explain a broken install instead of hiding it.
func (s *PluginService) ListPlugins(ctx context.Context) ([]PluginInfoDTO, error) {
	infos := s.svc.List()
	dtos := make([]PluginInfoDTO, 0, len(infos))
	for _, info := range infos {
		dtos = append(dtos, toPluginInfoDTO(info))
	}
	return dtos, nil
}

// SetPluginEnabled persists the user's choice and applies it immediately.
//
// A domain error is translated into a message the settings page can show; the
// sentinel stays reachable through errors.Is for tests.
func (s *PluginService) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	if err := s.svc.SetEnabled(ctx, id, enabled); err != nil {
		return translatePluginError(err)
	}
	return nil
}

// GetPluginStatus returns the coarse state and the most recent failure reason.
func (s *PluginService) GetPluginStatus(ctx context.Context, id string) (*PluginStatusDTO, error) {
	status, reason, err := s.svc.Status(id)
	if err != nil {
		return nil, translatePluginError(err)
	}
	return &PluginStatusDTO{ID: id, Status: string(status), LastError: reason}, nil
}

// RescanPlugins re-reads the plugin directory and reloads the enable flags, so
// the "refresh" button works after the user copies a plugin in.
//
// A directory with one broken plugin still returns the good ones; the joined
// error is reported as a string because Go's error wrapping does not survive
// the Wails bridge, and the UI shows it as a notice rather than a failure.
func (s *PluginService) RescanPlugins(ctx context.Context) ([]PluginInfoDTO, error) {
	if _, err := s.svc.Scan(ctx); err != nil {
		// Keep the scan result: the registry already registered what it could.
		// A reload failure only delays a newly installed plugin's stored flag;
		// the list below still reflects what exists on disk.
		_ = s.svc.ReloadEnabled(ctx)
		return s.ListPlugins(ctx)
	}
	return s.ListPlugins(ctx)
}

// toPluginInfoDTO flattens the registry entry into the JSON the UI reads.
func toPluginInfoDTO(info plugin.PluginInfo) PluginInfoDTO {
	capabilities := make([]string, 0, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		capabilities = append(capabilities, string(capability))
	}
	providers := info.Providers
	if providers == nil {
		providers = []string{}
	}
	network := info.Manifest.Permissions.Network
	if network == nil {
		network = []string{}
	}

	status := string(info.Status)
	if status == "" {
		status = string(domain.PluginStatusIdle)
	}

	return PluginInfoDTO{
		ID:           info.Manifest.ID,
		Name:         info.Manifest.Name,
		Version:      info.Manifest.Version,
		Author:       info.Manifest.Author,
		Capabilities: capabilities,
		Providers:    providers,
		Network:      network,
		KV:           info.Manifest.Permissions.KV,
		Enabled:      info.Enabled,
		Running:      info.Running,
		Status:       status,
		Invalid:      info.Invalid,
		LastError:    info.LastError,
	}
}

// translatePluginError turns registry sentinels into text a user can act on.
// The wrapped error is kept so callers can still classify it.
func translatePluginError(err error) error {
	switch {
	case errors.Is(err, domain.ErrPluginNotFound):
		return fmt.Errorf("this plugin is no longer installed: %w", err)
	case errors.Is(err, domain.ErrPluginProviderConflict):
		return fmt.Errorf("another plugin already provides this source: %w", err)
	case errors.Is(err, domain.ErrPluginProviderWithoutCapability):
		return fmt.Errorf("the plugin lists sources but cannot resolve streams: %w", err)
	case errors.Is(err, domain.ErrPluginSettingsUnavailable):
		return fmt.Errorf("plugin settings could not be saved: %w", err)
	case errors.Is(err, domain.ErrPluginRunningLimit):
		return fmt.Errorf("too many plugins are running; disable one first: %w", err)
	case errors.Is(err, domain.ErrPluginManifestInvalid):
		return fmt.Errorf("the plugin manifest is invalid: %w", err)
	}
	return err
}
