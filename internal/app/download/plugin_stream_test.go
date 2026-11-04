package download_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"airmedy/internal/app/plugin"
	appstream "airmedy/internal/app/stream"
	"airmedy/internal/domain"
	"airmedy/internal/infra/downloader"
	"airmedy/internal/infra/pluginhost"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file are the seam between three subsystems: a plugin
// resolves a URL, the router hands it to the download service, and the real
// downloader fetches the bytes. They use the pluginhost TestHelperProcess pattern
// (this test binary doubles as the plugin) so no Node or other runtime is
// needed.

func streamTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// staticRegistry is a one-route provider table: the resolver looks the track's
// provider up and gets this host, or nothing at all.
type staticRegistry struct {
	id   string
	host domain.PluginHost
}

func (r staticRegistry) HostForProvider(_ context.Context, provider string) (domain.PluginHost, bool) {
	if r.host == nil || provider == "" {
		return nil, false
	}
	return r.host, true
}

// stubPluginHost answers stream.resolve from a canned result.
type stubPluginHost struct {
	result domain.PluginStreamResult

	mu     sync.Mutex
	method string
}

func (h *stubPluginHost) Manifest() domain.PluginManifest { return domain.PluginManifest{} }

func (h *stubPluginHost) Capabilities() []domain.Capability {
	return []domain.Capability{domain.CapabilityStreamResolve}
}

func (h *stubPluginHost) Call(_ context.Context, method string, _ any, result any) error {
	h.mu.Lock()
	h.method = method
	h.mu.Unlock()
	encoded, err := json.Marshal(h.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

func (h *stubPluginHost) Close() error { return nil }

// seedPluginTrack inserts a remote track owned by provider.
func seedPluginTrack(t *testing.T, h *harness, provider, externalID string) *domain.TrackDTO {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID:           "remote-plugin-1",
		Path:         domain.RemotePath(provider, externalID),
		Title:        "晴天",
		Duration:     200,
		Availability: domain.AvailabilityRemote,
		Provider:     provider,
		ExternalID:   externalID,
	}))
	track, err := h.tracks.GetByID(ctx, "remote-plugin-1")
	require.NoError(t, err)
	return track
}

// TestService_PluginStreamHeadersReachDownloader is acceptance case 7: headers
// the plugin returned must arrive at the byte mover unchanged.
func TestService_PluginStreamHeadersReachDownloader(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	registry := staticRegistry{id: "io.airmedy.fake", host: &stubPluginHost{
		result: domain.PluginStreamResult{
			URL:     "http://cdn.example/a.mp3",
			Headers: map[string]string{"Referer": "https://music.163.com/"},
		},
	}}
	h.useResolver(appstream.NewPluginResolver(registry, streamTestLogger()))

	track := seedPluginTrack(t, h, "io.airmedy.fake", "509781655")
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.Quality320k))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the plugin-backed job to finish", func() bool {
		return h.activeJob(t, track.ID) == nil
	})

	h.downloader.mu.Lock()
	headers := h.downloader.lastRef.Headers
	url := h.downloader.lastRef.URL
	h.downloader.mu.Unlock()
	assert.Equal(t, "http://cdn.example/a.mp3", url)
	assert.Equal(t, "https://music.163.com/", headers["Referer"], "plugin headers must reach the downloader")
}

// TestService_PluginStreamResolvesAndDownloads is acceptance case 6: a real
// plugin process answers stream.resolve with an httptest URL, and the real
// downloader fetches the bytes into the library root. Finalize is stubbed, so the
// assertion stops at the byte-for-byte file.
func TestService_PluginStreamResolvesAndDownloads(t *testing.T) {
	payload := bytes.Repeat([]byte("ID3"), 700)
	var headerMu sync.Mutex
	var seenReferer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerMu.Lock()
		seenReferer = r.Header.Get("Referer")
		headerMu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	ctx := context.Background()
	logger := streamTestLogger()
	root := t.TempDir()
	writeStreamHelperManifest(t, root, server.URL)

	registry := plugin.NewService(
		pluginhost.NewLoader(logger),
		pluginhost.NewFactory(logger).Start,
		// This test starts plugins explicitly, so it needs no persisted
		// enable flags; a nil store is the documented "not bound" case.
		nil,
		logger,
		plugin.Config{Root: root},
	)
	t.Cleanup(func() { _ = registry.Close(context.Background()) })

	manifests, err := registry.Scan(ctx)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	require.NoError(t, registry.Start(ctx, "io.airmedy.streamtest"))

	h := newHarness(t)
	h.useResolver(appstream.NewPluginResolver(registry, logger))
	h.useDownloader(downloader.New(nil)) // nil properties disables the duration check

	track := seedPluginTrack(t, h, "io.airmedy.streamtest", "509781655")
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.Quality320k))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the plugin-backed download to finish", func() bool {
		return h.activeJob(t, track.ID) == nil
	})

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, domain.DownloadStateDone, jobs[0].State)
	assert.Equal(t, 1, h.finalizer.calls, "a finished download is handed to finalize")

	files, err := filepath.Glob(filepath.Join(h.dir, "*.mp3"))
	require.NoError(t, err)
	require.Len(t, files, 1, "the downloader must leave exactly one finished file")
	onDisk, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal(t, payload, onDisk, "the bytes must come from the URL the plugin resolved")

	headerMu.Lock()
	referer := seenReferer
	headerMu.Unlock()
	assert.Equal(t, "https://music.163.com/", referer,
		"the plugin's headers must reach the HTTP request, not just the downloader call")
}

// TestStreamPluginHelperProcess doubles as the plugin process: when the host
// launches this test binary with GO_WANT_STREAM_PLUGIN_HELPER=1 it speaks the
// line-delimited JSON-RPC protocol on stdio.
func TestStreamPluginHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_STREAM_PLUGIN_HELPER") != "1" {
		return
	}

	encoder := json.NewEncoder(os.Stdout)
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			handleStreamHelperRequest(encoder, trimmed)
		}
		if err != nil {
			os.Exit(0)
		}
	}
}

func handleStreamHelperRequest(encoder *json.Encoder, line []byte) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		return
	}

	reply := func(result any) {
		if len(request.ID) == 0 {
			return
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}

	switch request.Method {
	case "initialize":
		reply(map[string]any{"apiVersion": "1.0", "capabilities": []string{"stream.resolve"}})
	case "stream.resolve":
		reply(map[string]any{
			"url":       os.Getenv("STREAM_PLUGIN_URL"),
			"headers":   map[string]string{"Referer": "https://music.163.com/"},
			"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	case "shutdown":
		reply(map[string]bool{"ok": true})
		os.Exit(0)
	default:
		if len(request.ID) == 0 {
			return
		}
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error":   map[string]any{"code": -32601, "message": "method not found"},
		})
	}
}

// writeStreamHelperManifest writes a manifest that launches this test binary as a
// plugin whose stream.resolve points at streamURL.
func writeStreamHelperManifest(t *testing.T, root, streamURL string) {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)

	manifest := map[string]any{
		"id":         "io.airmedy.streamtest",
		"name":       "Stream Test Plugin",
		"version":    "1.0.0",
		"apiVersion": "^1.0",
		"runtime": map[string]any{
			"type":    "process",
			"command": executable,
			"args":    []string{"-test.run=TestStreamPluginHelperProcess", "--"},
			"env": map[string]string{
				"GO_WANT_STREAM_PLUGIN_HELPER": "1",
				"STREAM_PLUGIN_URL":            streamURL,
			},
		},
		"capabilities": []string{"stream.resolve"},
		// The provider the test's track carries; the router finds this plugin
		// through the declaration instead of asking running plugins.
		"providers": []string{"io.airmedy.streamtest"},
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, domain.ManifestFileName), encoded, 0o644))
}
