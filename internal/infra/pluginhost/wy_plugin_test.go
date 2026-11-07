package pluginhost_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"airmedy/internal/domain"
	"airmedy/internal/infra/pluginhost"
)

// These tests launch the real plugins/wy plugin through the real host. They cover
// what an in-process unit test cannot: that the process answers the handshake,
// reports its source table on `describe`, and that a caller mistake crosses the
// wire as a structured -32602 instead of a crash.
//
// No test here touches the network: a search with an invalid request fails before
// the plugin builds an HTTP call, so the real endpoint stays out of CI.

// installWYPlugin copies plugins/wy into a fresh plugins directory and starts it
// through the real host.
func startWYPlugin(t *testing.T) domain.PluginHost {
	t.Helper()

	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	source := filepath.Join(repoRoot, "plugins", "wy")
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("the wy plugin is not present at %s: %v", source, statErr)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node is not installed, so the wy plugin cannot run: %v", err)
	}

	pluginDir := filepath.Join(t.TempDir(), "wy")
	if err := copyTree(source, pluginDir); err != nil {
		t.Fatalf("install wy plugin: %v", err)
	}

	manifest, err := pluginhost.LoadManifest(filepath.Join(pluginDir, domain.ManifestFileName))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	host, err := pluginhost.StartManifest(context.Background(), manifest,
		pluginhost.WithLogger(discardLogger()),
		pluginhost.WithHandshakeTimeout(20*time.Second),
	)
	if err != nil {
		t.Fatalf("start the wy plugin: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

// TestWYPluginIsSearchOnly pins the manifest contract: the plugin declares
// catalog.search, serves no audio provider, and reports its sources on
// describe rather than hard-coding them in the app.
func TestWYPluginIsSearchOnly(t *testing.T) {
	host := startWYPlugin(t)

	manifest := host.Manifest()
	if manifest.ID != "wy" {
		t.Fatalf("id = %q, want wy", manifest.ID)
	}
	if len(manifest.Providers) != 0 {
		t.Fatalf("providers = %v, want none (search-only plugin)", manifest.Providers)
	}
	if !domain.HasCapability(host.Capabilities(), domain.CapabilityCatalogSearch) {
		t.Fatalf("capabilities = %v, want catalog.search", host.Capabilities())
	}
	if domain.HasCapability(host.Capabilities(), domain.CapabilityStreamResolve) {
		t.Fatalf("capabilities = %v, want no stream.resolve", host.Capabilities())
	}

	var describe domain.PluginDescribe
	if err := host.Call(context.Background(), domain.MethodDescribe, nil, &describe); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(describe.SearchSources) != 2 {
		t.Fatalf("search sources = %+v, want wy and kg", describe.SearchSources)
	}
	for _, got := range describe.SearchSources {
		if got.ID != "wy" && got.ID != "kg" {
			t.Fatalf("search source = %+v, want wy or kg", got)
		}
		if got.Name == "" {
			t.Fatalf("search source = %+v, want a label", got)
		}
	}
	if len(describe.Providers) != 0 {
		t.Fatalf("describe providers = %v, want none", describe.Providers)
	}
}

// TestWYPluginAnswersCallerMistakesWithStructuredErrors proves the plugin's
// request validation reaches the host as a JSON-RPC error rather than a crash:
// the RPCError carries -32602, which is what tells the host not to retry.
func TestWYPluginAnswersCallerMistakesWithStructuredErrors(t *testing.T) {
	host := startWYPlugin(t)

	var result domain.PluginSearchResult
	err := host.Call(context.Background(), domain.MethodSearch, domain.PluginSearchRequest{
		Source: "wy", Query: "   ",
	}, &result)
	if err == nil {
		t.Fatal("search with a blank query = nil error, want invalid params")
	}
	var rpcErr *domain.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want a structured RPCError", err)
	}
	if rpcErr.Code != domain.RPCErrorInvalidParams {
		t.Fatalf("code = %d, want %d", rpcErr.Code, domain.RPCErrorInvalidParams)
	}

	err = host.Call(context.Background(), domain.MethodSearch, domain.PluginSearchRequest{
		Source: "tx", Query: "x",
	}, &result)
	if !errors.As(err, &rpcErr) || rpcErr.Code != domain.RPCErrorInvalidParams {
		t.Fatalf("error for an unknown source = %v, want invalid params", err)
	}
}

// TestWYPluginSurvivesAMalformedRequest checks the plugin is still usable after
// answering an error: a session that dies on its first bad request would force
// the host into a restart on every typo.
func TestWYPluginSurvivesAMalformedRequest(t *testing.T) {
	host := startWYPlugin(t)

	var result domain.PluginSearchResult
	_ = host.Call(context.Background(), domain.MethodSearch, map[string]any{"query": 7}, &result)

	var describe domain.PluginDescribe
	if err := host.Call(context.Background(), domain.MethodDescribe, nil, &describe); err != nil {
		t.Fatalf("describe after a bad request: %v", err)
	}
	if len(describe.SearchSources) != 2 {
		t.Fatalf("search sources = %+v, want the plugin still alive", describe.SearchSources)
	}
}
