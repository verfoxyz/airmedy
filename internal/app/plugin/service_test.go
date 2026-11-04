package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testManifest(id string, capabilities ...domain.Capability) domain.PluginManifest {
	if len(capabilities) == 0 {
		capabilities = []domain.Capability{domain.CapabilityAuth}
	}
	return domain.PluginManifest{
		ID:           id,
		Name:         id,
		Version:      "1.0.0",
		APIVersion:   "^1.0",
		Capabilities: capabilities,
		Runtime: domain.PluginRuntime{
			Type:    domain.RuntimeTypeProcess,
			Command: "node",
		},
	}
}

// fakeLoader serves manifests from memory.
type fakeLoader struct {
	paths       []string
	manifests   map[string]domain.PluginManifest
	discoverErr error
	loadErr     map[string]error
}

func (l *fakeLoader) Discover(string) ([]string, error) {
	return l.paths, l.discoverErr
}

func (l *fakeLoader) Load(path string) (domain.PluginManifest, error) {
	if err := l.loadErr[path]; err != nil {
		return domain.PluginManifest{}, err
	}
	manifest, ok := l.manifests[path]
	if !ok {
		return domain.PluginManifest{}, errors.New("unknown path " + path)
	}
	return manifest, nil
}

// fakeHost records calls and can block to exercise the concurrency cap.
type fakeHost struct {
	manifest     domain.PluginManifest
	capabilities []domain.Capability
	callErr      error
	block        chan struct{}

	mu          sync.Mutex
	closed      bool
	inflight    int
	maxInflight int
	methods     []string
	// unloadReason is what Unload was told, empty when the host was only
	// Close'd (an implementation that predates the notification).
	unloadReason domain.PluginUnloadReason
}

func (h *fakeHost) Manifest() domain.PluginManifest { return h.manifest }

func (h *fakeHost) Capabilities() []domain.Capability {
	return append([]domain.Capability(nil), h.capabilities...)
}

func (h *fakeHost) Call(ctx context.Context, method string, _ any, result any) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return domain.ErrPluginHostClosed
	}
	h.inflight++
	if h.inflight > h.maxInflight {
		h.maxInflight = h.inflight
	}
	h.methods = append(h.methods, method)
	block, callErr := h.block, h.callErr
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		h.inflight--
		h.mu.Unlock()
	}()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if callErr != nil {
		return callErr
	}
	// This fake predates describe, so it behaves like a plugin written before the
	// method existed: it answers "method not found" and keeps its manifest
	// routes. A test that wants dynamic routes installs its own host.
	if method == domain.MethodDescribe {
		return fmt.Errorf("%w: %q", domain.ErrPluginMethodNotFound, method)
	}
	if target, ok := result.(*string); ok {
		*target = method
	}
	return nil
}

func (h *fakeHost) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

// Unload implements domain.PluginUnloader, so the registry's reason reaches a
// fake exactly the way it reaches the real host.
func (h *fakeHost) Unload(reason domain.PluginUnloadReason) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	h.unloadReason = reason
	return nil
}

// unloadedWith reports the reason the host was reclaimed with; ok is false when
// it was only Close'd.
func (h *fakeHost) unloadedWith() (domain.PluginUnloadReason, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unloadReason, h.unloadReason != ""
}

func (h *fakeHost) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// routingCalls counts the calls the stream router made. describe is excluded:
// Start asks every plugin what it serves, which is bookkeeping, not routing.
func (h *fakeHost) routingCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, method := range h.methods {
		if method != domain.MethodDescribe {
			count++
		}
	}
	return count
}

func newTestService(t *testing.T, loader *fakeLoader, config Config) (*Service, *fakeFactory) {
	t.Helper()
	service, factory, _ := newTestServiceWithSettings(t, loader, config, nil)
	return service, factory
}

// newTestServiceWithSettings builds a registry with a settings store, so tests
// can exercise enable state and autostart.
func newTestServiceWithSettings(
	t *testing.T, loader *fakeLoader, config Config, settings domain.PluginSettingsRepository,
) (*Service, *fakeFactory, *fakeSettings) {
	t.Helper()
	factory := &fakeFactory{hosts: make(map[string]*fakeHost)}
	store, _ := settings.(*fakeSettings)
	if store == nil {
		store = newFakeSettings()
	}
	service := NewService(loader, factory.start, store, testLogger(), config)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return service, factory, store
}

// fakeSettings is an in-memory domain.PluginSettingsRepository. A missing row
// means "disabled", mirroring the SQLite table.
type fakeSettings struct {
	mu   sync.Mutex
	rows map[string]bool
	err  error
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{rows: make(map[string]bool)}
}

func (f *fakeSettings) List(context.Context) ([]domain.PluginSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	rows := make([]domain.PluginSettings, 0, len(f.rows))
	for id, enabled := range f.rows {
		rows = append(rows, domain.PluginSettings{PluginID: id, Enabled: enabled})
	}
	return rows, nil
}

func (f *fakeSettings) Set(_ context.Context, id string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rows[id] = enabled
	return nil
}

func (f *fakeSettings) enabledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.rows))
	for id, enabled := range f.rows {
		if enabled {
			ids = append(ids, id)
		}
	}
	return ids
}

type fakeFactory struct {
	mu        sync.Mutex
	hosts     map[string]*fakeHost
	starts    int
	err       error
	block     chan struct{}
	manifests []domain.PluginManifest
	errs      map[string]error
}

func (f *fakeFactory) start(_ context.Context, manifest domain.PluginManifest) (domain.PluginHost, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	// Record what the registry handed over: the manifest is what the factory
	// launches, so it is the observable half of the Start contract.
	f.manifests = append(f.manifests, manifest)
	if err, fails := f.errs[manifest.ID]; fails && err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	host := &fakeHost{
		manifest:     manifest,
		capabilities: append([]domain.Capability(nil), manifest.Capabilities...),
		block:        f.block,
	}
	f.hosts[manifest.ID] = host
	return host, nil
}

func (f *fakeFactory) created(id string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[id]
}

func (f *fakeFactory) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeFactory) startedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.manifests))
	for _, manifest := range f.manifests {
		ids = append(ids, manifest.ID)
	}
	return ids
}

// TestUnloadReasonReachesThePlugin covers the lifecycle half of the first-batch
// contract at the registry level: the three reasons the host distinguishes
// (user disabled / host restart / app shutdown) must arrive as themselves, not as
// one undifferentiated "stop".
func TestUnloadReasonReachesThePlugin(t *testing.T) {
	manifest := testManifest("io.airmedy.any", domain.CapabilityStreamResolve)
	ctx := context.Background()

	t.Run("user disables the plugin", func(t *testing.T) {
		service, factory := newTestService(t, loaderWith(manifest), Config{Root: "/plugins"})
		t.Cleanup(func() { _ = service.Close(context.Background()) })

		if _, err := service.Scan(ctx); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if err := service.Start(ctx, manifest.ID); err != nil {
			t.Fatalf("Start: %v", err)
		}
		host := factory.created(manifest.ID)

		if err := service.Stop(ctx, manifest.ID); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if reason, ok := host.unloadedWith(); !ok || reason != domain.PluginUnloadReasonDisabled {
			t.Fatalf("unload reason = (%q, %v), want (disabled, true)", reason, ok)
		}
	})

	t.Run("the host restarts the plugin", func(t *testing.T) {
		service, factory := newTestService(t, loaderWith(manifest), Config{Root: "/plugins"})
		t.Cleanup(func() { _ = service.Close(context.Background()) })

		if _, err := service.Scan(ctx); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if err := service.Start(ctx, manifest.ID); err != nil {
			t.Fatalf("Start: %v", err)
		}
		previous := factory.created(manifest.ID)

		if err := service.Restart(ctx, manifest.ID); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		if reason, ok := previous.unloadedWith(); !ok || reason != domain.PluginUnloadReasonReload {
			t.Fatalf("unload reason = (%q, %v), want (reload, true)", reason, ok)
		}
	})

	t.Run("the app shuts down", func(t *testing.T) {
		service, factory := newTestService(t, loaderWith(manifest), Config{Root: "/plugins"})

		if _, err := service.Scan(ctx); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if err := service.Start(ctx, manifest.ID); err != nil {
			t.Fatalf("Start: %v", err)
		}
		host := factory.created(manifest.ID)

		if err := service.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if reason, ok := host.unloadedWith(); !ok || reason != domain.PluginUnloadReasonShutdown {
			t.Fatalf("unload reason = (%q, %v), want (shutdown, true)", reason, ok)
		}
	})
}

// TestUnloadFallsBackToClose: a PluginHost that predates the notification only
// has Close, and the registry must still reclaim it instead of failing.
func TestUnloadFallsBackToClose(t *testing.T) {
	host := &closeOnlyHost{manifest: testManifest("io.airmedy.legacy")}
	if err := unloadPlugin(host, domain.PluginUnloadReasonReload); err != nil {
		t.Fatalf("unloadPlugin: %v", err)
	}
	if !host.closed {
		t.Fatal("a host without Unload must still be closed")
	}
}

// closeOnlyHost is a PluginHost that deliberately does not implement
// domain.PluginUnloader.
type closeOnlyHost struct {
	manifest domain.PluginManifest
	closed   bool
}

func (h *closeOnlyHost) Manifest() domain.PluginManifest { return h.manifest }
func (h *closeOnlyHost) Capabilities() []domain.Capability {
	return h.manifest.Capabilities
}
func (h *closeOnlyHost) Call(context.Context, string, any, any) error { return nil }
func (h *closeOnlyHost) Close() error                                 { h.closed = true; return nil }

func TestScanRegistersValidPluginsAndSkipsBrokenOnes(t *testing.T) {
	loader := &fakeLoader{
		paths: []string{"/plugins/good/airmedy-plugin.json", "/plugins/bad/airmedy-plugin.json", "/plugins/dup/airmedy-plugin.json"},
		manifests: map[string]domain.PluginManifest{
			"/plugins/good/airmedy-plugin.json": testManifest("io.airmedy.good"),
			"/plugins/dup/airmedy-plugin.json":  testManifest("io.airmedy.good"),
		},
		loadErr: map[string]error{
			"/plugins/bad/airmedy-plugin.json": domain.ErrPluginManifestInvalid,
		},
	}
	service, _ := newTestService(t, loader, Config{Root: "/plugins"})

	registered, err := service.Scan(context.Background())
	if len(registered) != 1 {
		t.Fatalf("registered = %v, want one plugin", registered)
	}
	if err == nil {
		t.Fatal("expected the broken and duplicate manifests to be reported")
	}

	infos := service.List()
	if len(infos) != 1 || infos[0].Manifest.ID != "io.airmedy.good" {
		t.Fatalf("List = %+v", infos)
	}
	if infos[0].Running {
		t.Fatal("plugin should not be running before Start")
	}
	if len(infos[0].Capabilities) != 1 || infos[0].Capabilities[0] != domain.CapabilityAuth {
		t.Fatalf("manifest capabilities not surfaced: %+v", infos[0].Capabilities)
	}
}

func TestScanWithoutRootIsQuiet(t *testing.T) {
	service, _ := newTestService(t, &fakeLoader{}, Config{})
	registered, err := service.Scan(context.Background())
	if err != nil || len(registered) != 0 {
		t.Fatalf("Scan = %v, %v; want no plugins and no error", registered, err)
	}
}

func TestScanReportsDiscoveryFailure(t *testing.T) {
	loader := &fakeLoader{discoverErr: errors.New("disk on fire")}
	service, _ := newTestService(t, loader, Config{Root: "/plugins"})
	if _, err := service.Scan(context.Background()); err == nil {
		t.Fatal("expected discovery error")
	}
}

func TestStartAndCallLifecycle(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/plugins/a/airmedy-plugin.json"},
		manifests: map[string]domain.PluginManifest{"/plugins/a/airmedy-plugin.json": testManifest("io.airmedy.a")},
	}
	service, factory := newTestService(t, loader, Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if err := service.Call(ctx, "io.airmedy.a", "ping", nil, nil); !errors.Is(err, domain.ErrPluginNotRunning) {
		t.Fatalf("call before Start = %v, want ErrPluginNotRunning", err)
	}
	if err := service.Start(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Starting twice must not launch a second process.
	if err := service.Start(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if got := factory.startCount(); got != 1 {
		t.Fatalf("factory started %d hosts, want 1", got)
	}

	var method string
	if err := service.Call(ctx, "io.airmedy.a", "ping", nil, &method); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if method != "ping" {
		t.Fatalf("host saw method %q", method)
	}

	infos := service.List()
	if len(infos) != 1 || !infos[0].Running {
		t.Fatalf("List after Start = %+v", infos)
	}

	if err := service.Stop(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !factory.created("io.airmedy.a").isClosed() {
		t.Fatal("host was not closed by Stop")
	}
	if err := service.Stop(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("stopping a stopped plugin should be a no-op: %v", err)
	}
}

// TestHostLookupOnlyReturnsRunningPlugins pins the registry contract the stream
// resolver relies on: Host is a read-only lookup, and a stopped plugin is
// reported as absent rather than silently started.
func TestHostLookupOnlyReturnsRunningPlugins(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/plugins/a/airmedy-plugin.json"},
		manifests: map[string]domain.PluginManifest{"/plugins/a/airmedy-plugin.json": testManifest("io.airmedy.a")},
	}
	service, _ := newTestService(t, loader, Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if host, ok := service.Host(ctx, "io.airmedy.a"); ok || host != nil {
		t.Fatal("a registered but stopped plugin must not report a host")
	}
	if host, ok := service.Host(ctx, "unknown"); ok || host != nil {
		t.Fatal("an unknown id must not report a host")
	}

	if err := service.Start(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	host, ok := service.Host(ctx, "io.airmedy.a")
	if !ok || host == nil {
		t.Fatal("a running plugin must report its host")
	}

	if err := service.Stop(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if host, ok := service.Host(ctx, "io.airmedy.a"); ok || host != nil {
		t.Fatal("a stopped plugin must not report a host")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := service.Host(cancelled, "io.airmedy.a"); ok {
		t.Fatal("a cancelled context must not report a host")
	}
}

func TestStartUnknownPlugin(t *testing.T) {
	service, _ := newTestService(t, &fakeLoader{}, Config{})
	if err := service.Start(context.Background(), "nope"); !errors.Is(err, domain.ErrPluginNotFound) {
		t.Fatalf("Start unknown = %v, want ErrPluginNotFound", err)
	}
	if err := service.Call(context.Background(), "nope", "ping", nil, nil); !errors.Is(err, domain.ErrPluginNotFound) {
		t.Fatalf("Call unknown = %v, want ErrPluginNotFound", err)
	}
}

func TestStartRespectsRunningLimit(t *testing.T) {
	loader := &fakeLoader{
		paths: []string{"/a", "/b"},
		manifests: map[string]domain.PluginManifest{
			"/a": testManifest("io.airmedy.a"),
			"/b": testManifest("io.airmedy.b"),
		},
	}
	service, _ := newTestService(t, loader, Config{Root: "/plugins", MaxRunning: 1})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if err := service.Start(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Start a: %v", err)
	}
	if err := service.Start(ctx, "io.airmedy.b"); !errors.Is(err, domain.ErrPluginRunningLimit) {
		t.Fatalf("Start b = %v, want ErrPluginRunningLimit", err)
	}
}

func TestStartReleasesHostWhenClosedDuringLaunch(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/a"},
		manifests: map[string]domain.PluginManifest{"/a": testManifest("io.airmedy.a")},
	}
	service, factory := newTestService(t, loader, Config{Root: "/plugins"})
	if _, err := service.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	factory.block = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- service.Start(context.Background(), "io.airmedy.a") }()
	// Give Start time to reach the factory, then shut the registry down.
	time.Sleep(50 * time.Millisecond)
	if err := service.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(factory.block)

	if err := <-done; !errors.Is(err, domain.ErrPluginHostClosed) {
		t.Fatalf("Start after Close = %v, want ErrPluginHostClosed", err)
	}
	if host := factory.created("io.airmedy.a"); host == nil || !host.isClosed() {
		t.Fatal("host launched during shutdown was not reclaimed")
	}
}

func TestCallConcurrencyCap(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/a"},
		manifests: map[string]domain.PluginManifest{"/a": testManifest("io.airmedy.a")},
	}
	service, factory := newTestService(t, loader, Config{Root: "/plugins", MaxConcurrentCalls: 2})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.Start(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	host := factory.created("io.airmedy.a")
	host.mu.Lock()
	host.block = make(chan struct{})
	release := host.block
	host.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.Call(ctx, "io.airmedy.a", "ping", nil, nil); err != nil {
				t.Errorf("Call: %v", err)
			}
		}()
	}

	// Wait until the cap is reached, then give any extra calls a chance to slip
	// through before checking the high-water mark.
	deadline := time.Now().Add(2 * time.Second)
	for {
		host.mu.Lock()
		inflight := host.inflight
		host.mu.Unlock()
		if inflight >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	host.mu.Lock()
	maxInflight := host.maxInflight
	host.mu.Unlock()
	if maxInflight > 2 {
		t.Fatalf("max concurrent calls = %d, want at most 2", maxInflight)
	}

	close(release)
	wg.Wait()
}

func TestCallWaitsForSlotOrCancellation(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/a"},
		manifests: map[string]domain.PluginManifest{"/a": testManifest("io.airmedy.a")},
	}
	service, factory := newTestService(t, loader, Config{Root: "/plugins", MaxConcurrentCalls: 1})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.Start(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	host := factory.created("io.airmedy.a")
	host.mu.Lock()
	host.block = make(chan struct{})
	release := host.block
	host.mu.Unlock()

	first := make(chan error, 1)
	go func() { first <- service.Call(ctx, "io.airmedy.a", "ping", nil, nil) }()
	time.Sleep(50 * time.Millisecond)

	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := service.Call(waitCtx, "io.airmedy.a", "ping", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second call = %v, want context.DeadlineExceeded while the slot is taken", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
}

func TestCloseIsIdempotentAndStopsPlugins(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/a"},
		manifests: map[string]domain.PluginManifest{"/a": testManifest("io.airmedy.a")},
	}
	service, factory := newTestService(t, loader, Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.Start(ctx, "io.airmedy.a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !factory.created("io.airmedy.a").isClosed() {
		t.Fatal("running plugin not stopped by Close")
	}
	if err := service.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := service.Register(testManifest("io.airmedy.b")); !errors.Is(err, domain.ErrPluginHostClosed) {
		t.Fatalf("Register after Close = %v, want ErrPluginHostClosed", err)
	}
	if err := service.Start(ctx, "io.airmedy.a"); !errors.Is(err, domain.ErrPluginHostClosed) {
		t.Fatalf("Start after Close = %v, want ErrPluginHostClosed", err)
	}
}

func TestLoadUsesManifestLoader(t *testing.T) {
	loader := &fakeLoader{
		manifests: map[string]domain.PluginManifest{"/x": testManifest("io.airmedy.x")},
	}
	service, _ := newTestService(t, loader, Config{})

	manifest, err := service.Load(context.Background(), "/x")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if manifest.ID != "io.airmedy.x" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := service.Load(context.Background(), "/x"); !errors.Is(err, domain.ErrPluginAlreadyRegistered) {
		t.Fatalf("second Load = %v, want ErrPluginAlreadyRegistered", err)
	}
	if _, err := service.Load(context.Background(), "/missing"); err == nil || !strings.Contains(err.Error(), "unknown path") {
		t.Fatalf("Load missing = %v", err)
	}
}

func TestFactoryErrorFailsStart(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/a"},
		manifests: map[string]domain.PluginManifest{"/a": testManifest("io.airmedy.a")},
	}
	service, factory := newTestService(t, loader, Config{Root: "/plugins"})
	if _, err := service.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	factory.err = domain.ErrPluginHandshakeTimeout

	err := service.Start(context.Background(), "io.airmedy.a")
	if !errors.Is(err, domain.ErrPluginHandshakeTimeout) {
		t.Fatalf("Start = %v, want the factory error to survive", err)
	}
	// The failed plugin is not left registered as running.
	if infos := service.List(); len(infos) != 1 || infos[0].Running {
		t.Fatalf("List after failed Start = %+v", infos)
	}
}
