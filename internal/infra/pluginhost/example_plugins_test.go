package pluginhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"airmedy/internal/domain"
	"airmedy/internal/infra/pluginhost"
)

// These tests drive the example plugin in examples/plugins through the real host:
// a manifest on disk is loaded, the process is launched, the handshake runs, and
// stream.resolve is answered by the plugin's own code. They are the "one plugin
// is one functional unit" proof the contract asks for (task §4).

// buildExamplePlugin compiles examples/plugins/echo into a temp dir and returns
// the executable path.
func buildExamplePlugin(t *testing.T) string {
	t.Helper()

	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	source := filepath.Join(repoRoot, "examples", "plugins", "echo")
	if _, err := os.Stat(source); err != nil {
		t.Skipf("example plugin is not present: %v", err)
	}

	// Name it .exe on Windows: `go build -o` appends the extension only when the
	// target has none, and launching the extensionless file fails there.
	out := filepath.Join(t.TempDir(), "echo")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	build := exec.Command("go", "build", "-o", out, "./examples/plugins/echo")
	build.Dir = repoRoot
	build.Env = os.Environ()
	output, err := build.CombinedOutput()
	if err != nil {
		t.Skipf("cannot build the example plugin (%v): %s", err, output)
	}
	return out
}

// exampleTree lays the example out the way a user installs it: one directory
// holding the manifest and the binary.
func exampleTree(t *testing.T, pluginBinary string) (root string, pluginDir string) {
	t.Helper()

	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	root = t.TempDir()
	pluginDir = filepath.Join(root, "echo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("create plugin dir: %v", err)
	}

	// Copy the manifest, then point it at the compiled binary.
	manifest := readJSON(t, filepath.Join(repoRoot, "examples", "plugins", "echo", "airmedy-plugin.json"))
	manifest["runtime"].(map[string]any)["command"] = pluginBinary
	writeJSON(t, filepath.Join(pluginDir, "airmedy-plugin.json"), manifest)

	return root, pluginDir
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return decoded
}

func writeJSON(t *testing.T, path string, value map[string]any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startExample loads and launches the example, returning the running host.
func startExample(t *testing.T) domain.PluginHost {
	t.Helper()

	binary := buildExamplePlugin(t)
	_, pluginDir := exampleTree(t, binary)

	manifest, err := pluginhost.LoadManifest(filepath.Join(pluginDir, "airmedy-plugin.json"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	// The manifest is the routing table: this is the declaration the app reads
	// to decide who serves echo.test.
	if len(manifest.Providers) != 1 || manifest.Providers[0] != "echo.test" {
		t.Fatalf("Providers = %v, want the declared echo.test route", manifest.Providers)
	}

	host, err := pluginhost.StartManifest(context.Background(), manifest,
		pluginhost.WithLogger(discardLogger()),
		pluginhost.WithHandshakeTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatalf("start plugin: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

// TestExamplePluginServesStreamResolve is the headline proof: the app launches
// the plugin, the handshake passes, and stream.resolve returns the plugin's
// fixed URL.
func TestExamplePluginServesStreamResolve(t *testing.T) {
	host := startExample(t)

	var result domain.PluginStreamResult
	if err := host.Call(context.Background(), domain.MethodStreamResolve, domain.PluginStreamRequest{
		Source:  "echo.test",
		ID:      "any-id",
		Quality: "320k",
	}, &result); err != nil {
		t.Fatalf("stream.resolve: %v", err)
	}
	if result.URL == "" {
		t.Fatal("the plugin's URL did not come back")
	}
	if !strings.Contains(result.URL, "echo") {
		t.Fatalf("url = %q, want the plugin's fixed URL", result.URL)
	}
}

// TestExamplePluginReportsAnUndeclaredProviderAsAFailure: a plugin asked for a
// provider it did not declare fails with a structured kind, not with the old
// decline signal. The app never sends such a request, because routing is a
// declaration lookup; the plugin still answers in a way that cannot be retried
// forever.
func TestExamplePluginReportsAnUndeclaredProviderAsAFailure(t *testing.T) {
	host := startExample(t)

	err := host.Call(context.Background(), domain.MethodStreamResolve, domain.PluginStreamRequest{
		Source: "some.other.provider",
		ID:     "id",
	}, nil)
	if err == nil {
		t.Fatal("a provider the plugin does not serve must produce a failure")
	}

	var rpcErr *domain.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want a structured RPC error", err)
	}
	var data domain.PluginErrorData
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("error data = %s: %v", rpcErr.Data, err)
	}
	if data.Kind != domain.StreamErrorUnavailable {
		t.Fatalf("kind = %q, want %q", data.Kind, domain.StreamErrorUnavailable)
	}
}

// TestExamplePluginHandshakeDeclaresItsCapability pins the other half of the
// manifest contract: the live handshake must agree with the declaration, because
// the registry treats the handshake as authoritative once the process is up.
func TestExamplePluginHandshakeDeclaresItsCapability(t *testing.T) {
	host := startExample(t)

	capabilities := host.Capabilities()
	found := false
	for _, capability := range capabilities {
		if capability == domain.CapabilityStreamResolve {
			found = true
		}
	}
	if !found {
		t.Fatalf("Capabilities = %v, want stream.resolve", capabilities)
	}
	if got := host.Manifest().Providers; len(got) != 1 || got[0] != "echo.test" {
		t.Fatalf("Manifest().Providers = %v, want the declared route", got)
	}
}
