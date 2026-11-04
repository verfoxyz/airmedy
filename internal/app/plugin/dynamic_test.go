package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"airmedy/internal/domain"
)

// describeHost is a fake host that answers describe and can change its answer
// between calls, which is how a "the user edited the source list" change looks
// to the registry.
type describeHost struct {
	manifest domain.PluginManifest

	mu          sync.Mutex
	describe    domain.PluginDescribe
	describeErr error
	calls       []string
	closed      bool
}

func (h *describeHost) Manifest() domain.PluginManifest { return h.manifest }

func (h *describeHost) Capabilities() []domain.Capability {
	return h.manifest.Capabilities
}

func (h *describeHost) Call(_ context.Context, method string, _ any, result any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, method)
	if h.closed {
		return domain.ErrPluginHostClosed
	}
	if method != domain.MethodDescribe {
		return fmt.Errorf("%w: %q", domain.ErrPluginMethodNotFound, method)
	}
	if h.describeErr != nil {
		return h.describeErr
	}
	if target, ok := result.(*domain.PluginDescribe); ok {
		*target = h.describe
	}
	return nil
}

func (h *describeHost) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

// currentDescribe returns the answer the host would give now.
func (h *describeHost) currentDescribe() domain.PluginDescribe {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.describe
}

// currentError returns the error the next describe would produce.
func (h *describeHost) currentError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.describeErr
}

func (h *describeHost) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// dynamicFactory hands out one describable host per plugin id and remembers it,
// so a test can drive describe answers after a restart.
type dynamicFactory struct {
	mu    sync.Mutex
	hosts map[string]*describeHost
	err   error
}

func newDynamicFactory() *dynamicFactory {
	return &dynamicFactory{hosts: make(map[string]*describeHost)}
}

// start reuses a host the test pre-seeded for the id (so a test can decide what
// describe will answer) and otherwise creates a fresh one.
func (f *dynamicFactory) start(_ context.Context, manifest domain.PluginManifest) (domain.PluginHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if previous, ok := f.hosts[manifest.ID]; ok {
		// Restart replaces the process, as a real launch would. The answer and
		// the "does not implement describe" stance carry over, because both are
		// properties of the plugin binary, not of the launch.
		host := &describeHost{
			manifest:    manifest,
			describe:    previous.currentDescribe(),
			describeErr: previous.currentError(),
		}
		f.hosts[manifest.ID] = host
		return host, nil
	}
	host := &describeHost{manifest: manifest}
	f.hosts[manifest.ID] = host
	return host, nil
}

func (f *dynamicFactory) host(id string) *describeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[id]
}

// newDynamicService builds a registry whose factory produces describable hosts.
func newDynamicService(t *testing.T, manifests ...domain.PluginManifest) (*Service, *dynamicFactory, *fakeSettings) {
	t.Helper()
	loader := loaderWith(manifests...)
	factory := newDynamicFactory()
	settings := &fakeSettings{rows: make(map[string]bool)}
	service := NewService(loader, factory.start, settings, testLogger(), Config{Root: "/plugins"})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return service, factory, settings
}

// TestDescribeReplacesTheManifestRoutes covers acceptance case 10's core half: a
// plugin that answers describe owns its routes, even though its manifest declares
// none. Without this, a dynamic plugin could never be routed at all.
func TestDescribeReplacesTheManifestRoutes(t *testing.T) {
	manifest := testManifest("io.airmedy.lx", domain.CapabilityStreamResolve)
	manifest.Providers = nil // dynamic: the manifest deliberately declares none

	service, factory, _ := newDynamicService(t, manifest)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	factory.mu.Lock()
	factory.hosts["io.airmedy.lx"] = &describeHost{
		manifest: manifest,
		describe: domain.PluginDescribe{
			Providers: []string{" WY ", "kg", "wy", "", "tx"},
			Sources: []domain.PluginSourceDescription{
				{ID: "a1", Name: "ikun", Enabled: true, Providers: []string{"wy"}},
				{ID: "b2", Name: "bad", Enabled: true, Error: "syntax error"},
			},
		},
	}
	factory.mu.Unlock()

	// Start asks describe on its own, so a freshly started dynamic plugin is
	// routable without a separate call.
	if err := service.Start(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := service.HostForProvider(ctx, "wy"); !ok {
		t.Fatal("describe reported wy, so it must be routable")
	}
	if _, ok := service.HostForProvider(ctx, "kg"); !ok {
		t.Fatal("describe reported kg, so it must be routable")
	}
	info := findInfo(t, service.List(), "io.airmedy.lx")
	// describe keeps the plugin's order, drops the blanks and case-folds.
	if got := info.Providers; len(got) != 3 || got[0] != "wy" || got[1] != "kg" || got[2] != "tx" {
		t.Fatalf("Providers = %v, want the normalised describe list", got)
	}
}

// TestDescribeDropsAnEmptyRouteSet proves the "dynamic list is authoritative,
// including empty" rule: a plugin that reported no providers must not silently
// fall back to its manifest declaration, or a disabled source list would keep
// advertising routes it cannot serve.
func TestDescribeDropsAnEmptyRouteSet(t *testing.T) {
	manifest := testManifest("io.airmedy.lx", domain.CapabilityStreamResolve)
	manifest.Providers = []string{"wy"}

	service, factory, _ := newDynamicService(t, manifest)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	factory.mu.Lock()
	factory.hosts["io.airmedy.lx"] = &describeHost{
		manifest: manifest,
		describe: domain.PluginDescribe{}, // the user disabled every source
	}
	factory.mu.Unlock()

	if err := service.Start(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := service.HostForProvider(ctx, "wy"); ok {
		t.Fatal("a describable plugin with no routes must not be routed to")
	}
	if owners := service.Providers(); len(owners) != 0 {
		t.Fatalf("Providers() = %v, want no owners", owners)
	}
}

// TestPluginWithoutDescribeKeepsManifestRoutes: describe is optional. A plugin
// that answers -32601 (every plugin written before this method existed) must keep
// working exactly as before, not be marked broken.
func TestPluginWithoutDescribeKeepsManifestRoutes(t *testing.T) {
	manifest := providerManifest("io.airmedy.static", "wy")

	service, factory, _ := newDynamicService(t, manifest)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	factory.mu.Lock()
	// A plugin written before describe existed answers -32601.
	factory.hosts["io.airmedy.static"] = &describeHost{
		manifest:    manifest,
		describeErr: fmt.Errorf("%w: %q", domain.ErrPluginMethodNotFound, domain.MethodDescribe),
	}
	factory.mu.Unlock()

	if err := service.Start(ctx, "io.airmedy.static"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := service.HostForProvider(ctx, "wy"); !ok {
		t.Fatal("a plugin without describe must still route on its manifest declaration")
	}
	info := findInfo(t, service.List(), "io.airmedy.static")
	if info.Invalid {
		t.Fatalf("a plugin without describe must stay valid: %s", info.LastError)
	}
	if len(info.Providers) != 1 || info.Providers[0] != "wy" {
		t.Fatalf("Providers = %v, want the manifest declaration", info.Providers)
	}
}

// TestRefreshDynamicRereadsRoutesAfterARestart covers acceptance case 10: after
// the source list changes, a restart plus describe must move the routing table.
func TestRefreshDynamicRereadsRoutesAfterARestart(t *testing.T) {
	manifest := testManifest("io.airmedy.lx", domain.CapabilityStreamResolve)

	service, factory, _ := newDynamicService(t, manifest)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	factory.mu.Lock()
	factory.hosts["io.airmedy.lx"] = &describeHost{
		manifest: manifest,
		describe: domain.PluginDescribe{Providers: []string{"wy"}},
	}
	factory.mu.Unlock()

	if err := service.Start(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := service.HostForProvider(ctx, "wy"); !ok {
		t.Fatal("wy must be routable before the change")
	}

	// The factory hands out a new host on restart, carrying the new answer.
	factory.mu.Lock()
	next := &describeHost{manifest: manifest, describe: domain.PluginDescribe{Providers: []string{"kg"}}}
	// Replace the map entry so the next Start uses the new answer; the old host
	// is what Stop closes.
	factory.hosts["io.airmedy.lx"] = next
	factory.mu.Unlock()

	if err := service.Restart(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if _, ok := service.HostForProvider(ctx, "kg"); !ok {
		t.Fatal("kg must be routable after the restart re-read the list")
	}
	if _, ok := service.HostForProvider(ctx, "wy"); ok {
		t.Fatal("wy must stop being routable once the source list dropped it")
	}

	info := findInfo(t, service.List(), "io.airmedy.lx")
	if len(info.Providers) != 1 || info.Providers[0] != "kg" {
		t.Fatalf("Providers = %v, want the new describe answer", info.Providers)
	}
}

// TestRestartRelaunchesTheProcess: Restart must actually replace the host, not
// just re-ask the old one, because the source list reaches the plugin through its
// environment at launch.
func TestRestartRelaunchesTheProcess(t *testing.T) {
	manifest := testManifest("io.airmedy.lx", domain.CapabilityStreamResolve)

	service, factory, _ := newDynamicService(t, manifest)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if err := service.Start(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := factory.host("io.airmedy.lx")

	if err := service.Restart(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !first.isClosed() {
		t.Fatal("Restart must close the previous process")
	}
	if factory.host("io.airmedy.lx") == first {
		t.Fatal("Restart must start a new process, not reuse the old one")
	}
}

// TestLaunchEnvIsInjectedOnEveryStart is the environment half of the contract:
// the registry hands the plugin id to the hook and merges the answer into the
// manifest env, so a start after a list change carries the new list.
func TestLaunchEnvIsInjectedOnEveryStart(t *testing.T) {
	manifest := testManifest("io.airmedy.lx", domain.CapabilityStreamResolve)

	var (
		mu      sync.Mutex
		asked   []string
		payload = "AIRMEDY_LX_SOURCES=[]"
	)
	loader := loaderWith(manifest)
	factory := &manifestRecordingFactory{}
	settings := &fakeSettings{rows: make(map[string]bool)}
	service := NewService(loader, factory.start, settings, testLogger(), Config{
		Root: "/plugins",
		LaunchEnv: func(_ context.Context, pluginID string) []string {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, pluginID)
			if pluginID != "io.airmedy.lx" {
				return nil
			}
			return []string{payload}
		},
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.Start(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	seen := factory.lastManifest()
	if got := seen.Runtime.Env["AIRMEDY_LX_SOURCES"]; got != "[]" {
		t.Fatalf("AIRMEDY_LX_SOURCES = %q, want the hook's value", got)
	}

	// A second start (after a restart) asks again, so the value can change.
	mu.Lock()
	payload = `AIRMEDY_LX_SOURCES=[{"id":"a1"}]`
	mu.Unlock()
	if err := service.Restart(ctx, "io.airmedy.lx"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	seen = factory.lastManifest()
	if got := seen.Runtime.Env["AIRMEDY_LX_SOURCES"]; got != `[{"id":"a1"}]` {
		t.Fatalf("AIRMEDY_LX_SOURCES after restart = %q, want the new value", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 2 {
		t.Fatalf("the launch hook was asked %d times, want it asked again on restart", len(asked))
	}
}

// manifestRecordingFactory records the manifest each launch received, which is
// where the injected environment is observable.
type manifestRecordingFactory struct {
	mu        sync.Mutex
	manifests []domain.PluginManifest
	hosts     map[string]*fakeHost
}

func (f *manifestRecordingFactory) start(_ context.Context, manifest domain.PluginManifest) (domain.PluginHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts == nil {
		f.hosts = make(map[string]*fakeHost)
	}
	f.manifests = append(f.manifests, manifest)
	host := &fakeHost{manifest: manifest}
	f.hosts[manifest.ID] = host
	return host, nil
}

func (f *manifestRecordingFactory) lastManifest() domain.PluginManifest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.manifests) == 0 {
		return domain.PluginManifest{}
	}
	return f.manifests[len(f.manifests)-1]
}

// TestDescribeOnAStoppedPluginFails keeps a stale UI row honest: describe needs a
// running process, and the error must say so instead of returning empty routes
// that look like "the user disabled everything".
func TestDescribeOnAStoppedPluginFails(t *testing.T) {
	manifest := testManifest("io.airmedy.lx", domain.CapabilityStreamResolve)

	service, _, _ := newDynamicService(t, manifest)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, err := service.Describe(ctx, "io.airmedy.lx"); !errors.Is(err, domain.ErrPluginNotRunning) {
		t.Fatalf("Describe on a stopped plugin = %v, want ErrPluginNotRunning", err)
	}
}
