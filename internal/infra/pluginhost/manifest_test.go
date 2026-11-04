package pluginhost

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airmedy/internal/domain"
)

func TestLoadManifestValid(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, nil))

	manifest, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.ID != "io.airmedy.test" {
		t.Errorf("ID = %q, want io.airmedy.test", manifest.ID)
	}
	if manifest.APIVersion != "^1.0" {
		t.Errorf("APIVersion = %q, want ^1.0", manifest.APIVersion)
	}
	if len(manifest.Capabilities) != 2 || manifest.Capabilities[1] != domain.CapabilityStreamResolve {
		t.Errorf("Capabilities = %v, want [auth stream.resolve]", manifest.Capabilities)
	}
	if manifest.Dir != filepath.Dir(path) {
		t.Errorf("Dir = %q, want %q", manifest.Dir, filepath.Dir(path))
	}
	if manifest.Runtime.Type != domain.RuntimeTypeProcess {
		t.Errorf("Runtime.Type = %q, want process", manifest.Runtime.Type)
	}
	// A bare command is a PATH lookup and must not be rewritten.
	if manifest.Runtime.Command != "node" {
		t.Errorf("Runtime.Command = %q, want node", manifest.Runtime.Command)
	}
}

func TestLoadManifestMissingID(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, map[string]any{"id": nil}))

	_, err := LoadManifest(path)
	assertErrorIs(t, err, domain.ErrPluginManifestInvalid)
	if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("error should name the missing field, got: %v", err)
	}
}

func TestLoadManifestInvalidAPIVersion(t *testing.T) {
	for _, bad := range []string{"", "banana", "^", "1.0.0.0"} {
		t.Run(bad, func(t *testing.T) {
			path := writeManifestFile(t, manifestJSON(t, map[string]any{"apiVersion": bad}))
			_, err := LoadManifest(path)
			assertErrorIs(t, err, domain.ErrPluginManifestInvalid)
		})
	}
}

func TestLoadManifestRejectsUnsupportedRuntime(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, map[string]any{
		"runtime": map[string]any{"type": "wasm", "command": "plugin.wasm"},
	}))
	_, err := LoadManifest(path)
	assertErrorIs(t, err, domain.ErrPluginManifestInvalid)
	if !strings.Contains(err.Error(), "wasm") {
		t.Errorf("error should name the unsupported runtime, got: %v", err)
	}
}

func TestLoadManifestRequiresCommand(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, map[string]any{
		"runtime": map[string]any{"type": "process"},
	}))
	_, err := LoadManifest(path)
	assertErrorIs(t, err, domain.ErrPluginManifestInvalid)
}

func TestLoadManifestMissingFile(t *testing.T) {
	_, err := LoadManifest(filepath.Join(t.TempDir(), "nope.json"))
	assertErrorIs(t, err, domain.ErrPluginManifestInvalid)
}

func TestLoadManifestMalformedJSON(t *testing.T) {
	path := writeManifestFile(t, []byte(`{"id": "x",`))
	_, err := LoadManifest(path)
	assertErrorIs(t, err, domain.ErrPluginManifestInvalid)
}

// TestLoadManifestTolerantCapabilities pins the tolerant-mode rule: capabilities
// the host does not know are kept verbatim and never fail validation.
func TestLoadManifestTolerantCapabilities(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, map[string]any{
		"capabilities": []string{"auth", "git", "hires", "auth", " "},
	}))
	manifest, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	want := []domain.Capability{"auth", "git", "hires"}
	if len(manifest.Capabilities) != len(want) {
		t.Fatalf("Capabilities = %v, want %v", manifest.Capabilities, want)
	}
	for i := range want {
		if manifest.Capabilities[i] != want[i] {
			t.Fatalf("Capabilities = %v, want %v", manifest.Capabilities, want)
		}
	}
}

// TestLoadManifestResolvesRelativeCommand checks that an explicit relative path
// is anchored at the manifest directory, which is also the child's working dir.
func TestLoadManifestResolvesRelativeCommand(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, map[string]any{
		"runtime": map[string]any{
			"type":    "process",
			"command": "./bin/plugin",
			"args":    []string{"--flag"},
		},
	}))
	manifest, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	wantCommand := filepath.Join(filepath.Dir(path), "bin", "plugin")
	if manifest.Runtime.Command != wantCommand {
		t.Errorf("Runtime.Command = %q, want %q", manifest.Runtime.Command, wantCommand)
	}
	if len(manifest.Runtime.Args) != 1 || manifest.Runtime.Args[0] != "--flag" {
		t.Errorf("Runtime.Args = %v, want [--flag]", manifest.Runtime.Args)
	}
}

func TestDiscoverManifests(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "one", domain.ManifestFileName)
	second := filepath.Join(root, "nested", "two", domain.ManifestFileName)
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, manifestJSON(t, nil), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	// A hidden directory holds caches, not plugins, and must be skipped.
	hidden := filepath.Join(root, ".cache", domain.ManifestFileName)
	if err := os.MkdirAll(filepath.Dir(hidden), 0o755); err != nil {
		t.Fatalf("mkdir hidden: %v", err)
	}
	if err := os.WriteFile(hidden, manifestJSON(t, nil), 0o644); err != nil {
		t.Fatalf("write hidden manifest: %v", err)
	}

	paths, err := DiscoverManifests(root)
	if err != nil {
		t.Fatalf("DiscoverManifests: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("found %d manifests, want 2: %v", len(paths), paths)
	}
	found := map[string]bool{paths[0]: true, paths[1]: true}
	if !found[first] || !found[second] {
		t.Errorf("paths = %v, want %s and %s", paths, first, second)
	}
}

func TestDiscoverManifestsMissingDirectoryIsEmpty(t *testing.T) {
	paths, err := DiscoverManifests(filepath.Join(t.TempDir(), "not-installed"))
	if err != nil {
		t.Fatalf("missing dir should not be an error: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("paths = %v, want none", paths)
	}
}

func TestDiscoverManifestsRejectsFilePath(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, nil))
	_, err := DiscoverManifests(path)
	if err == nil {
		t.Fatalf("expected an error when the plugin root is a file")
	}
}

func TestValidateManifestIDPattern(t *testing.T) {
	for _, id := range []string{"../escape", "with space", "/absolute"} {
		path := writeManifestFile(t, manifestJSON(t, map[string]any{"id": id}))
		_, err := LoadManifest(path)
		if !errors.Is(err, domain.ErrPluginManifestInvalid) {
			t.Errorf("id %q: expected ErrPluginManifestInvalid, got %v", id, err)
		}
	}
}

// TestLoadManifestProviders is acceptance case 1: the routing declaration is
// trimmed, lower-cased, deduplicated and blank-free, so " WY " and "wy" cannot
// become two routes.
func TestLoadManifestProviders(t *testing.T) {
	t.Run("normalises and deduplicates", func(t *testing.T) {
		manifest, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{
			"providers": []string{" WY ", "kg", "wy", "", "  "},
		})))
		if err != nil {
			t.Fatalf("LoadManifest: %v", err)
		}
		want := []string{"wy", "kg"}
		if len(manifest.Providers) != len(want) {
			t.Fatalf("Providers = %v, want %v", manifest.Providers, want)
		}
		for i := range want {
			if manifest.Providers[i] != want[i] {
				t.Fatalf("Providers = %v, want %v (declaration order kept)", manifest.Providers, want)
			}
		}
	})

	t.Run("absent means no provider", func(t *testing.T) {
		manifest, err := LoadManifest(writeManifestFile(t, manifestJSON(t, nil)))
		if err != nil {
			t.Fatalf("LoadManifest: %v", err)
		}
		if len(manifest.Providers) != 0 {
			t.Fatalf("Providers = %v, want none for a plugin that serves no source", manifest.Providers)
		}
	})

	t.Run("empty array means no provider", func(t *testing.T) {
		manifest, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{
			"providers": []string{},
		})))
		if err != nil {
			t.Fatalf("an empty provider list is legal: %v", err)
		}
		if len(manifest.Providers) != 0 {
			t.Fatalf("Providers = %v, want none", manifest.Providers)
		}
	})

	t.Run("blank entries collapse to no provider", func(t *testing.T) {
		manifest, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{
			"providers": []string{"", "   "},
		})))
		if err != nil {
			t.Fatalf("LoadManifest: %v", err)
		}
		if len(manifest.Providers) != 0 {
			t.Fatalf("Providers = %v, want none (all entries were blank)", manifest.Providers)
		}
	})
}

// TestLoadManifestNetwork pins the declaration the settings row renders: blank
// entries are dropped and the rest are trimmed, so the badge never shows an empty
// host or a stray space. It is a declaration only — nothing here enforces it.
func TestLoadManifestNetwork(t *testing.T) {
	t.Run("trims and drops blanks", func(t *testing.T) {
		manifest, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{
			"permissions": map[string]any{"network": []string{" api.example.com ", "", "  ", "*"}},
		})))
		if err != nil {
			t.Fatalf("LoadManifest: %v", err)
		}
		want := []string{"api.example.com", "*"}
		if len(manifest.Permissions.Network) != len(want) {
			t.Fatalf("Network = %v, want %v", manifest.Permissions.Network, want)
		}
		for i := range want {
			if manifest.Permissions.Network[i] != want[i] {
				t.Fatalf("Network = %v, want %v (declaration order kept)",
					manifest.Permissions.Network, want)
			}
		}
	})

	t.Run("absent means no declared network", func(t *testing.T) {
		manifest, err := LoadManifest(writeManifestFile(t, manifestJSON(t, nil)))
		if err != nil {
			t.Fatalf("a plugin that declares no network is legal: %v", err)
		}
		if len(manifest.Permissions.Network) != 0 {
			t.Fatalf("Network = %v, want none", manifest.Permissions.Network)
		}
	})
}

// TestLoadManifestProvidersRequireStreamResolve: providers are a routing claim,
// so a manifest that lists them without the capability is rejected. The registry
// would otherwise advertise a route nobody can serve.
func TestLoadManifestProvidersRequireStreamResolve(t *testing.T) {
	path := writeManifestFile(t, manifestJSON(t, map[string]any{
		"capabilities": []string{"auth"},
		"providers":    []string{"wy"},
	}))

	_, err := LoadManifest(path)
	assertErrorIs(t, err, domain.ErrPluginManifestInvalid)
	if !strings.Contains(err.Error(), "stream.resolve") {
		t.Fatalf("error should name the missing capability, got: %v", err)
	}

	// The same plugin without the declaration is a normal auth plugin.
	if _, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{
		"capabilities": []string{"auth"},
	}))); err != nil {
		t.Fatalf("a plugin with no providers must not need stream.resolve: %v", err)
	}
}

// TestLoadManifestAuthorTrims keeps the display-only field tidy.
func TestLoadManifestAuthorTrims(t *testing.T) {
	manifest, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{"author": " Airmedy "})))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Author != "Airmedy" {
		t.Fatalf("author = %q, want the trimmed value", manifest.Author)
	}
}

// TestLoadManifestRuntimeMustBeLaunchable: every plugin is a leaf the app
// launches itself, so a host-defined runtime is no longer a legal shape.
func TestLoadManifestRuntimeMustBeLaunchable(t *testing.T) {
	_, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{
		"runtime": map[string]any{"type": "inprocess"},
	})))
	assertErrorIs(t, err, domain.ErrPluginManifestInvalid)

	if _, err := LoadManifest(writeManifestFile(t, manifestJSON(t, map[string]any{
		"runtime": map[string]any{"type": "process"},
	}))); err == nil {
		t.Fatal("a process runtime without a command must be rejected")
	}
}
