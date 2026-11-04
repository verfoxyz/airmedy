package wails

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"airmedy/internal/app/plugin"
	"airmedy/internal/domain"
)

// stubLoader serves manifests from memory.
type stubLoader struct {
	path      string
	manifests map[string]domain.PluginManifest
}

func (l *stubLoader) Discover(string) ([]string, error) { return []string{l.path}, nil }

func (l *stubLoader) Load(path string) (domain.PluginManifest, error) {
	manifest, ok := l.manifests[path]
	if !ok {
		return domain.PluginManifest{}, domain.ErrPluginManifestInvalid
	}
	return manifest, nil
}

// stubHost is a running plugin that answers calls trivially.
type stubHost struct{ manifest domain.PluginManifest }

func (h *stubHost) Manifest() domain.PluginManifest { return h.manifest }
func (h *stubHost) Capabilities() []domain.Capability {
	return h.manifest.Capabilities
}
func (h *stubHost) Call(context.Context, string, any, any) error { return nil }
func (h *stubHost) Close() error                                 { return nil }

// stubSettings is an in-memory enable-flag store.
type stubSettings struct{ rows map[string]bool }

func (s *stubSettings) List(context.Context) ([]domain.PluginSettings, error) {
	rows := make([]domain.PluginSettings, 0, len(s.rows))
	for id, enabled := range s.rows {
		rows = append(rows, domain.PluginSettings{PluginID: id, Enabled: enabled})
	}
	return rows, nil
}

func (s *stubSettings) Set(_ context.Context, id string, enabled bool) error {
	s.rows[id] = enabled
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newPluginFixture builds the registry the binding wraps, with two plugins
// serving different providers plus one that serves none.
func newPluginFixture(t *testing.T) (*PluginService, *stubSettings) {
	t.Helper()

	manifestPath := "/plugins/ikun/airmedy-plugin.json"
	lyricsPath := "/plugins/lyrics/airmedy-plugin.json"
	loader := &stubLoader{
		path: manifestPath,
		manifests: map[string]domain.PluginManifest{
			manifestPath: {
				ID:           "lx.ikun",
				Name:         "ikun 音乐源",
				Version:      "2.0.0",
				Author:       "Airmedy",
				APIVersion:   "^1.0",
				Capabilities: []domain.Capability{domain.CapabilityStreamResolve},
				Providers:    []string{"wy", "kg"},
				Runtime:      domain.PluginRuntime{Type: domain.RuntimeTypeProcess, Command: "node"},
				Dir:          "/plugins/ikun",
			},
		},
	}
	settings := &stubSettings{rows: map[string]bool{}}

	factory := func(_ context.Context, manifest domain.PluginManifest) (domain.PluginHost, error) {
		return &stubHost{manifest: manifest}, nil
	}
	svc := plugin.NewService(loader, factory, settings, discardLogger(), plugin.Config{Root: "/plugins"})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	ctx := context.Background()
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// Register a second plugin explicitly: the stub's Discover only lists the
	// first, so this models "a lyrics plugin is installed too".
	if err := svc.Register(domain.PluginManifest{
		ID:           "lx.lyrics",
		Name:         "Lyrics",
		Version:      "1.0.0",
		APIVersion:   "^1.0",
		Capabilities: []domain.Capability{domain.CapabilityLyricFetch},
		Runtime:      domain.PluginRuntime{Type: domain.RuntimeTypeProcess, Command: "node"},
		Dir:          "/plugins/lyrics",
	}); err != nil {
		t.Fatalf("Register lyrics: %v", err)
	}
	_ = lyricsPath

	return NewPluginService(svc), settings
}

// TestListPluginsDTO is the frontend contract: the JSON field names and the flat
// one-row-per-plugin shape the settings page renders.
func TestListPluginsDTO(t *testing.T) {
	svc, _ := newPluginFixture(t)

	dtos, err := svc.ListPlugins(context.Background())
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	if len(dtos) != 2 {
		t.Fatalf("ListPlugins returned %d rows, want 2", len(dtos))
	}

	byID := map[string]PluginInfoDTO{}
	for _, dto := range dtos {
		byID[dto.ID] = dto
	}

	source, ok := byID["lx.ikun"]
	if !ok {
		t.Fatal("the source plugin is missing from the list")
	}
	if source.Author != "Airmedy" || source.Version != "2.0.0" {
		t.Fatalf("metadata lost: %+v", source)
	}
	// The provider declaration is the routing table the UI shows as tags.
	if len(source.Providers) != 2 || source.Providers[0] != "wy" || source.Providers[1] != "kg" {
		t.Fatalf("Providers = %v, want the manifest declaration", source.Providers)
	}

	lyrics, ok := byID["lx.lyrics"]
	if !ok {
		t.Fatal("the lyrics plugin is missing from the list")
	}
	if len(lyrics.Providers) != 0 {
		t.Fatalf("Providers = %v, want none for a plugin that serves no source", lyrics.Providers)
	}

	// The JSON contract: the settings page reads snake_case keys.
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"id", "name", "version", "author", "capabilities", "providers",
		"enabled", "running", "status", "invalid", "last_error",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("DTO is missing the %q key the frontend reads", key)
		}
	}
	// The host/sub-plugin fields are gone from the wire contract, not just
	// zeroed: a stale frontend must fail loudly instead of rendering two rows.
	for _, removed := range []string{"host", "sub_plugins", "host_enabled"} {
		if _, ok := decoded[removed]; ok {
			t.Errorf("DTO still exposes the removed %q field", removed)
		}
	}
}

// TestListPluginsDTOFlatRows: every entry carries its own switch-able state; no
// row is a child of another.
func TestListPluginsDTOFlatRows(t *testing.T) {
	svc, _ := newPluginFixture(t)

	dtos, err := svc.ListPlugins(context.Background())
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	for _, dto := range dtos {
		if dto.Enabled {
			t.Fatalf("%s is enabled with no settings row", dto.ID)
		}
		if dto.Invalid {
			t.Fatalf("%s should be valid: %s", dto.ID, dto.LastError)
		}
	}
}

// TestSetPluginEnabledPersistsAndStarts: the binding is translation only, but it
// must reach the registry's toggle.
func TestSetPluginEnabledPersistsAndStarts(t *testing.T) {
	svc, settings := newPluginFixture(t)
	ctx := context.Background()

	if err := svc.SetPluginEnabled(ctx, "lx.ikun", true); err != nil {
		t.Fatalf("SetPluginEnabled: %v", err)
	}
	if !settings.rows["lx.ikun"] {
		t.Fatal("the choice was not persisted")
	}

	dtos, err := svc.ListPlugins(ctx)
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	for _, dto := range dtos {
		if dto.ID != "lx.ikun" {
			continue
		}
		if !dto.Running || dto.Status != string(domain.PluginStatusRunning) {
			t.Fatalf("enabled plugin = running:%v status:%q", dto.Running, dto.Status)
		}
	}
}

// TestSetPluginEnabledTranslatesErrors covers the user-facing text: a domain
// sentinel becomes a sentence, and it stays classifiable.
func TestSetPluginEnabledTranslatesErrors(t *testing.T) {
	svc, _ := newPluginFixture(t)
	ctx := context.Background()

	err := svc.SetPluginEnabled(ctx, "io.airmedy.ghost", true)
	if !errors.Is(err, domain.ErrPluginNotFound) {
		t.Fatalf("error = %v, want ErrPluginNotFound to remain reachable", err)
	}
	if msg := err.Error(); msg == domain.ErrPluginNotFound.Error() {
		t.Fatal("the message should be translated, not the raw sentinel")
	}
}

// TestGetPluginStatusDTO covers the second binding method.
func TestGetPluginStatusDTO(t *testing.T) {
	svc, _ := newPluginFixture(t)
	ctx := context.Background()

	status, err := svc.GetPluginStatus(ctx, "lx.ikun")
	if err != nil {
		t.Fatalf("GetPluginStatus: %v", err)
	}
	if status.ID != "lx.ikun" || status.Status != string(domain.PluginStatusIdle) {
		t.Fatalf("status = %+v", status)
	}
	if status.LastError != "" {
		t.Fatalf("LastError = %q, want empty", status.LastError)
	}

	if _, err := svc.GetPluginStatus(ctx, "io.airmedy.ghost"); !errors.Is(err, domain.ErrPluginNotFound) {
		t.Fatalf("unknown id = %v, want ErrPluginNotFound", err)
	}
}

// TestToPluginInfoDTOKeepsProviders pins that the routing declaration reaches the
// frontend even when the plugin also declares a capability the host does not
// understand.
func TestToPluginInfoDTOKeepsProviders(t *testing.T) {
	dto := toPluginInfoDTO(plugin.PluginInfo{
		Manifest: domain.PluginManifest{
			ID:           "lx.ikun",
			Providers:    []string{"wy", "kg", "kw"},
			Capabilities: []domain.Capability{domain.CapabilityStreamResolve, "hires"},
		},
		Capabilities: []domain.Capability{domain.CapabilityStreamResolve, "hires"},
		Providers:    []string{"wy", "kg", "kw"},
	})

	if len(dto.Providers) != 3 {
		t.Fatalf("Providers = %v, want all declared providers", dto.Providers)
	}
	if dto.Providers[2] != "kw" {
		t.Fatalf("Providers = %v, order must match the declaration", dto.Providers)
	}
}

// TestToPluginInfoDTOKeepsNetworkDeclaration pins that the manifest's network
// allow-list reaches the UI, so the row can show a network badge before the user
// enables the plugin. No manifest entry must still marshal as [] so the frontend
// can decide "no badge" without a nil check.
func TestToPluginInfoDTOKeepsNetworkDeclaration(t *testing.T) {
	declared := toPluginInfoDTO(plugin.PluginInfo{
		Manifest: domain.PluginManifest{
			ID:          "lx",
			Permissions: domain.PluginPermissions{Network: []string{"api.example.com", "*"}},
		},
	})
	if len(declared.Network) != 2 || declared.Network[0] != "api.example.com" {
		t.Fatalf("Network = %v, want the declared hosts in order", declared.Network)
	}

	quiet := toPluginInfoDTO(plugin.PluginInfo{
		Manifest: domain.PluginManifest{ID: "io.airmedy.quiet"},
	})
	if quiet.Network == nil {
		t.Fatal("Network must be an empty slice, not nil")
	}
	if len(quiet.Network) != 0 {
		t.Fatalf("Network = %v, want none for a plugin that declares no network", quiet.Network)
	}
}

// TestToPluginInfoDTOKeepsUnknownCapabilities: a plugin may declare a capability
// the host does not understand, and the UI still shows it.
func TestToPluginInfoDTOKeepsUnknownCapabilities(t *testing.T) {
	dto := toPluginInfoDTO(plugin.PluginInfo{
		Manifest: domain.PluginManifest{
			ID:           "io.airmedy.odd",
			Name:         "Odd",
			Capabilities: []domain.Capability{domain.CapabilityAuth, "hires"},
		},
		Capabilities: []domain.Capability{domain.CapabilityAuth, "hires"},
	})

	if len(dto.Capabilities) != 2 || dto.Capabilities[1] != "hires" {
		t.Fatalf("capabilities = %v, want the unknown one kept", dto.Capabilities)
	}
	// Nil slices must marshal as [] so the frontend can map over them.
	if dto.Providers == nil {
		t.Fatal("Providers must be an empty slice, not nil")
	}
	if dto.Status == "" {
		t.Fatal("a plugin with no recorded status must still report one")
	}
}
