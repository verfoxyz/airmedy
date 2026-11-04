package pluginhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airmedy/internal/domain"
	"airmedy/internal/infra/pluginhost"
)

// These tests launch the real plugins/lx plugin through the real host. The
// example plugin covers the synthetic path; this one covers the plugin a user
// actually installs, including the environment-injected source list and the
// dynamic describe answer the app routes on (acceptance cases 5–10 without the
// GUI).

// installLXPlugin copies plugins/lx into a fresh plugins directory and writes the
// source scripts a user would have imported. It returns the plugins root and the
// AIRMEDY_LX_SOURCES value for those scripts.
func installLXPlugin(t *testing.T, scripts ...string) (string, string) {
	t.Helper()

	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	source := filepath.Join(repoRoot, "plugins", "lx")
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("the lx plugin is not present at %s: %v", source, statErr)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node is not installed, so the lx plugin cannot run: %v", err)
	}

	root := t.TempDir()
	pluginDir := filepath.Join(root, "lx")
	if err := copyTree(source, pluginDir); err != nil {
		t.Fatalf("install lx plugin: %v", err)
	}

	// The core copies user scripts into <DataDir>/plugins/lx/sources/ and passes
	// them through the environment; this reproduces that layout.
	sourcesDir := filepath.Join(pluginDir, "sources")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatalf("create sources dir: %v", err)
	}
	entries := make([]map[string]any, 0, len(scripts))
	for index, body := range scripts {
		id := string(rune('a' + index))
		path := filepath.Join(sourcesDir, id+".js")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write source %s: %v", id, err)
		}
		entries = append(entries, map[string]any{
			"id": id, "name": "source " + id, "path": path, "enabled": true, "position": index,
		})
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode source list: %v", err)
	}
	return root, string(encoded)
}

// copyTree copies a directory, skipping test/ (not needed at runtime) and any
// node_modules.
func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(from, path)
		if relErr != nil {
			return relErr
		}
		if info.IsDir() {
			if info.Name() == "node_modules" || (rel != "." && info.Name() == "test") {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0o644)
	})
}

// sourceScriptServing builds a script that registers one platform and returns a
// fixed URL.
func sourceScriptServing(provider, url string) string {
	return `
const { EVENT_NAMES, send, on } = globalThis.lx;
on(EVENT_NAMES.request, () => Promise.resolve('` + url + `'));
send(EVENT_NAMES.inited, { status: true, sources: {
  ` + provider + `: { name: '` + provider + `', type: 'music', actions: ['musicUrl'], qualitys: ['128k'] },
}});
`
}

// sourceScriptFailing builds a script that registers one platform and always
// rejects: the "supports the platform but cannot get this track" case the
// fallback exists for.
func sourceScriptFailing(provider, message string) string {
	return `
const { EVENT_NAMES, send, on } = globalThis.lx;
on(EVENT_NAMES.request, () => Promise.reject(new Error('` + message + `')));
send(EVENT_NAMES.inited, { status: true, sources: {
  ` + provider + `: { name: '` + provider + `', type: 'music', actions: ['musicUrl'], qualitys: ['128k'] },
}});
`
}

// startLXPlugin installs the plugin, then starts it through the real host with
// the source list in its environment.
func startLXPlugin(t *testing.T, scripts ...string) domain.PluginHost {
	t.Helper()

	root, sourcesEnv := installLXPlugin(t, scripts...)
	manifest, err := pluginhost.LoadManifest(filepath.Join(root, "lx", domain.ManifestFileName))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	host, err := pluginhost.StartManifest(context.Background(), manifest,
		pluginhost.WithLogger(discardLogger()),
		pluginhost.WithEnv("AIRMEDY_LX_SOURCES="+sourcesEnv),
		pluginhost.WithHandshakeTimeout(20*time.Second),
	)
	if err != nil {
		t.Fatalf("start the lx plugin: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

// TestLXPluginIsOneRoutablePlugin is the shape the app depends on: one plugin
// process, a manifest that declares no providers (the routes are dynamic), and a
// describe answer carrying the platforms the imported scripts registered.
func TestLXPluginIsOneRoutablePlugin(t *testing.T) {
	host := startLXPlugin(t, sourceScriptServing("wy", "http://example.com/a.mp3"))

	manifest := host.Manifest()
	if manifest.ID != "lx" {
		t.Fatalf("id = %q, want lx", manifest.ID)
	}
	// Task §2.4: providers are dynamic, so the manifest must deliberately declare
	// none. A static list would be a lie once the user changes their sources.
	if len(manifest.Providers) != 0 {
		t.Fatalf("providers = %v, want none (routes come from describe)", manifest.Providers)
	}
	if !domain.HasCapability(host.Capabilities(), domain.CapabilityStreamResolve) {
		t.Fatalf("capabilities = %v, want stream.resolve", host.Capabilities())
	}

	var describe domain.PluginDescribe
	if err := host.Call(context.Background(), domain.MethodDescribe, nil, &describe); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(describe.Providers) != 1 || describe.Providers[0] != "wy" {
		t.Fatalf("describe providers = %v, want [wy]", describe.Providers)
	}
	if len(describe.Sources) != 1 {
		t.Fatalf("describe sources = %+v, want one", describe.Sources)
	}
	if got := describe.Sources[0].Providers; len(got) != 1 || got[0] != "wy" {
		t.Fatalf("source providers = %v", got)
	}
}

// TestLXPluginFallsBackToTheNextSource is acceptance case 5 end to end: the first
// source cannot get the track, the second can, and the URL comes from the second.
func TestLXPluginFallsBackToTheNextSource(t *testing.T) {
	host := startLXPlugin(t,
		sourceScriptFailing("wy", "无版权"),
		sourceScriptServing("wy", "http://example.com/second.mp3"),
	)

	var result domain.PluginStreamResult
	err := host.Call(context.Background(), domain.MethodStreamResolve, domain.PluginStreamRequest{
		Source: "wy", ID: "HASH1", Quality: "320k",
	}, &result)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if result.URL != "http://example.com/second.mp3" {
		t.Fatalf("url = %q, want the fallback source's URL", result.URL)
	}
}

// TestLXPluginSkipsASourceForAnotherPlatform is acceptance case 6: a source that
// does not serve the platform is skipped silently, not recorded as a failure.
func TestLXPluginSkipsASourceForAnotherPlatform(t *testing.T) {
	host := startLXPlugin(t,
		sourceScriptServing("kg", "http://example.com/kg.mp3"),
		sourceScriptServing("wy", "http://example.com/wy.mp3"),
	)

	var describe domain.PluginDescribe
	if err := host.Call(context.Background(), domain.MethodDescribe, nil, &describe); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(describe.Providers) != 2 {
		t.Fatalf("providers = %v, want both platforms", describe.Providers)
	}

	var result domain.PluginStreamResult
	if err := host.Call(context.Background(), domain.MethodStreamResolve, domain.PluginStreamRequest{
		Source: "wy", ID: "HASH1", Quality: "320k",
	}, &result); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if result.URL != "http://example.com/wy.mp3" {
		t.Fatalf("url = %q, want the wy source's URL", result.URL)
	}
}

// TestLXPluginReportsEveryAttemptWhenAllFail is acceptance case 7: the error is
// unavailable, and its message names each source's result.
func TestLXPluginReportsEveryAttemptWhenAllFail(t *testing.T) {
	host := startLXPlugin(t,
		sourceScriptFailing("wy", "无版权"),
		sourceScriptFailing("wy", "已下架"),
	)

	err := host.Call(context.Background(), domain.MethodStreamResolve, domain.PluginStreamRequest{
		Source: "wy", ID: "HASH1", Quality: "320k",
	}, nil)
	if err == nil {
		t.Fatal("resolving a track no source can serve must fail")
	}

	var rpcErr *domain.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want a structured RPC error", err)
	}
	var data domain.PluginErrorData
	if unmarshalErr := json.Unmarshal(rpcErr.Data, &data); unmarshalErr != nil {
		t.Fatalf("error data = %s: %v", rpcErr.Data, unmarshalErr)
	}
	if data.Kind != domain.StreamErrorUnavailable {
		t.Fatalf("kind = %q, want %q", data.Kind, domain.StreamErrorUnavailable)
	}
	if !strings.Contains(rpcErr.Message, "无版权") || !strings.Contains(rpcErr.Message, "已下架") {
		t.Fatalf("message = %q, want every source's result", rpcErr.Message)
	}
}

// TestLXPluginValidatesACandidate is the import path end to end: the host asks the
// plugin whether a file is a usable custom source before the core stores it.
func TestLXPluginValidatesACandidate(t *testing.T) {
	host := startLXPlugin(t, sourceScriptServing("wy", "http://example.com/a.mp3"))

	dir := t.TempDir()
	good := filepath.Join(dir, "good.js")
	if err := os.WriteFile(good, []byte(sourceScriptServing("tx", "http://example.com/tx.mp3")), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	var validation domain.PluginSourceValidation
	if err := host.Call(context.Background(), domain.MethodValidateSource,
		domain.PluginValidateSourceRequest{Path: good}, &validation); err != nil {
		t.Fatalf("validate a valid source: %v", err)
	}
	if len(validation.Providers) != 1 || validation.Providers[0] != "tx" {
		t.Fatalf("providers = %v, want [tx]", validation.Providers)
	}

	bad := filepath.Join(dir, "bad.js")
	if err := os.WriteFile(bad, []byte("this is not javascript {{{"), 0o644); err != nil {
		t.Fatalf("write bad candidate: %v", err)
	}
	err := host.Call(context.Background(), domain.MethodValidateSource,
		domain.PluginValidateSourceRequest{Path: bad}, nil)
	if err == nil {
		t.Fatal("a syntax error must be rejected at import time")
	}
	var rpcErr *domain.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want a structured RPC error", err)
	}
	if !strings.Contains(rpcErr.Message, "bad.js") {
		t.Fatalf("message = %q, want it to name the file", rpcErr.Message)
	}
}
