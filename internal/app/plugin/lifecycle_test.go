package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"airmedy/internal/domain"
)

// providerManifest builds a plugin that declares providers and the capability
// that makes them routable.
func providerManifest(id string, providers ...string) domain.PluginManifest {
	manifest := testManifest(id, domain.CapabilityStreamResolve)
	manifest.Providers = providers
	manifest.Dir = "/plugins/" + id
	return manifest
}

// loaderWith builds a loader that serves manifests keyed by a stable path so
// discovery order can be controlled per test.
func loaderWith(manifests ...domain.PluginManifest) *fakeLoader {
	loader := &fakeLoader{manifests: make(map[string]domain.PluginManifest, len(manifests))}
	for _, manifest := range manifests {
		path := "/plugins/" + manifest.ID + "/airmedy-plugin.json"
		loader.paths = append(loader.paths, path)
		loader.manifests[path] = manifest
	}
	return loader
}

// TestRegisterMarksBothSidesOfAProviderConflict is acceptance case 2: two
// plugins declaring the same provider have no deterministic owner, so both are
// invalid and each message names the other plugin.
//
// The assertion is deliberately order-independent: discovery order must not
// decide a winner, because the user could not predict it.
func TestRegisterMarksBothSidesOfAProviderConflict(t *testing.T) {
	first := providerManifest("io.airmedy.alpha", "wy", "kg")
	second := providerManifest("io.airmedy.beta", "kg", "tx")

	service, _ := newTestService(t, loaderWith(first, second), Config{Root: "/plugins"})
	if _, err := service.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	infos := map[string]PluginInfo{}
	for _, info := range service.List() {
		infos[info.Manifest.ID] = info
	}
	if len(infos) != 2 {
		t.Fatalf("List = %+v, want both conflicting plugins kept", infos)
	}

	for id, other := range map[string]string{
		"io.airmedy.alpha": "io.airmedy.beta",
		"io.airmedy.beta":  "io.airmedy.alpha",
	} {
		info := infos[id]
		if !info.Invalid {
			t.Fatalf("%s declares a provider that %s also declares, so it must be invalid", id, other)
		}
		if !strings.Contains(info.LastError, domain.ErrPluginProviderConflict.Error()) {
			t.Fatalf("%s reason = %q, want the conflict sentinel", id, info.LastError)
		}
		if !strings.Contains(info.LastError, other) {
			t.Fatalf("%s LastError = %q, want it to name %s", id, info.LastError, other)
		}
		if !strings.Contains(info.LastError, "kg") {
			t.Fatalf("%s LastError = %q, want it to name the contested provider", id, info.LastError)
		}
		if info.EffectiveEnabled() {
			t.Fatalf("%s is invalid, so it must never be effective-enabled", id)
		}
	}
}

// TestProviderReasonsAreDerivedNotSticky proves the invalidation is recomputed
// rather than latched: re-applying an empty conflict map clears a stored
// provider reason, while a start failure on the same entry survives.
func TestProviderReasonsAreDerivedNotSticky(t *testing.T) {
	service, factory, _ := newTestServiceWithSettings(
		t, loaderWith(providerManifest("io.airmedy.alpha", "wy")), Config{Root: "/plugins"}, nil)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	factory.err = errors.New("handshake refused")
	if err := service.Start(ctx, "io.airmedy.alpha"); err == nil {
		t.Fatal("Start should fail")
	}

	service.mu.Lock()
	service.entries["io.airmedy.alpha"].invalidReason = fmt.Errorf(
		"%w: provider %q is also declared by %s",
		domain.ErrPluginProviderConflict, "wy", "io.airmedy.beta")
	service.applyProviderReasonsLocked(map[string]error{})
	cleared := service.entries["io.airmedy.alpha"].invalidReason
	service.mu.Unlock()
	if cleared != nil {
		t.Fatalf("invalidReason = %v, want the stale conflict cleared", cleared)
	}

	if _, reason := mustStatus(t, service, "io.airmedy.alpha"); !strings.Contains(reason, "handshake") {
		t.Fatalf("reason = %q, the start failure must survive the conflict refresh", reason)
	}
}

// TestRegisterRejectsProvidersWithoutStreamResolve: a provider list only means
// something next to stream.resolve, so the combination is invalid.
func TestRegisterRejectsProvidersWithoutStreamResolve(t *testing.T) {
	manifest := testManifest("io.airmedy.lyrics", domain.CapabilityLyricFetch)
	manifest.Providers = []string{"wy"}

	service, _ := newTestService(t, loaderWith(manifest), Config{Root: "/plugins"})
	if _, err := service.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	info := findInfo(t, service.List(), "io.airmedy.lyrics")
	if !info.Invalid {
		t.Fatal("providers without stream.resolve must be invalid")
	}
	if !strings.Contains(info.LastError, domain.ErrPluginProviderWithoutCapability.Error()) {
		t.Fatalf("reason = %q, want the capability sentinel", info.LastError)
	}
}

// TestPluginWithoutProvidersIsValidAndNeverRouted is acceptance case 5: a plugin
// that serves no track source (a lyrics-only plugin, say) is a normal plugin.
func TestPluginWithoutProvidersIsValidAndNeverRouted(t *testing.T) {
	lyrics := testManifest("io.airmedy.lyrics", domain.CapabilityLyricFetch)

	service, factory := newTestService(t, loaderWith(lyrics), Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	info := findInfo(t, service.List(), "io.airmedy.lyrics")
	if info.Invalid {
		t.Fatalf("a plugin with no providers must be valid, got: %s", info.LastError)
	}
	if len(info.Providers) != 0 {
		t.Fatalf("Providers = %v, want none", info.Providers)
	}

	if err := service.Start(ctx, "io.airmedy.lyrics"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := service.HostForProvider(ctx, "wy"); ok {
		t.Fatal("a plugin that declares no provider must not be routed to")
	}
	if factory.created("io.airmedy.lyrics") == nil {
		t.Fatal("the plugin must still start; it is just not a route")
	}
}

// TestHostForProviderRoutesOnlyToTheDeclaringPlugin is acceptance case 3: the
// lookup returns exactly the plugin that declared the provider, so no other
// running plugin is consulted.
func TestHostForProviderRoutesOnlyToTheDeclaringPlugin(t *testing.T) {
	owner := providerManifest("io.airmedy.owner", "wy")
	other := providerManifest("io.airmedy.other", "kg")

	service, factory := newTestService(t, loaderWith(owner, other), Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, id := range []string{"io.airmedy.owner", "io.airmedy.other"} {
		if err := service.Start(ctx, id); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}

	host, ok := service.HostForProvider(ctx, "wy")
	if !ok {
		t.Fatal("the declaring plugin must be found")
	}
	if got := host.Manifest().ID; got != "io.airmedy.owner" {
		t.Fatalf("HostForProvider(wy) = %q, want the declaring plugin", got)
	}
	// Normalisation is shared with the track path, so padding and case still hit.
	if _, ok := service.HostForProvider(ctx, "  WY "); !ok {
		t.Fatal("the lookup must normalise the provider id the way tracks are")
	}
	// The other plugin's handler was never consulted: its call counter stays 0.
	if got := factory.created("io.airmedy.other").routingCalls(); got != 0 {
		t.Fatalf("the non-owning plugin received %d routing calls, want 0", got)
	}
}

// TestHostForProviderNormalisesDeclaredIDs: a manifest handed to Register (not
// through the manifest loader) may carry padded ids; the lookup still answers.
func TestHostForProviderNormalisesDeclaredIDs(t *testing.T) {
	manifest := testManifest("io.airmedy.odd", domain.CapabilityStreamResolve)
	manifest.Providers = []string{" WY "}

	service, _ := newTestService(t, loaderWith(manifest), Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.Start(ctx, "io.airmedy.odd"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, ok := service.HostForProvider(ctx, "wy"); !ok {
		t.Fatal("a declared \" WY \" must route as \"wy\"")
	}
	if owners := service.Providers(); owners["wy"] != "io.airmedy.odd" {
		t.Fatalf("Providers() = %v, want the normalised key", owners)
	}
}

// TestHostForProviderMisses cover acceptance case 4's lookup half: an unknown
// provider, a stopped owner and an invalid owner all report "not routed".
func TestHostForProviderMisses(t *testing.T) {
	service, _ := newTestService(t,
		loaderWith(providerManifest("io.airmedy.owner", "wy"), providerManifest("io.airmedy.alpha", "kg"), providerManifest("io.airmedy.beta", "kg")),
		Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if _, ok := service.HostForProvider(ctx, "tx"); ok {
		t.Fatal("nobody declares tx, so nothing may be routed")
	}
	if _, ok := service.HostForProvider(ctx, ""); ok {
		t.Fatal("an empty provider is not a route")
	}
	// The owner is registered but not started.
	if _, ok := service.HostForProvider(ctx, "wy"); ok {
		t.Fatal("a stopped plugin must not be routed to")
	}
	// kg is claimed twice, so both claimants are invalid.
	if _, ok := service.HostForProvider(ctx, "kg"); ok {
		t.Fatal("a conflicted provider has no owner and must not be routed")
	}
}

// TestProvidersReturnsTheOwnershipMap is what the error messages and the UI read:
// provider → plugin id for every plugin that claims it.
func TestProvidersReturnsTheOwnershipMap(t *testing.T) {
	service, _ := newTestService(t,
		loaderWith(providerManifest("io.airmedy.alpha", "wy", "kg"), testManifest("io.airmedy.lyrics", domain.CapabilityLyricFetch)),
		Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	owners := service.Providers()
	if owners["wy"] != "io.airmedy.alpha" || owners["kg"] != "io.airmedy.alpha" {
		t.Fatalf("owners = %v, want both providers mapped to the declaring plugin", owners)
	}
	if len(owners) != 2 {
		t.Fatalf("owners = %v, want no entry for a plugin without providers", owners)
	}
}

// TestStartEnabledStartsOnlyEnabledPlugins is the happy half of autostart:
// enabled plugins start, disabled ones do not.
func TestStartEnabledStartsOnlyEnabledPlugins(t *testing.T) {
	on := providerManifest("io.airmedy.on", "wy")
	off := providerManifest("io.airmedy.off", "kg")

	service, factory, settings := newTestServiceWithSettings(
		t, loaderWith(on, off), Config{Root: "/plugins"}, nil)
	if err := settings.Set(context.Background(), "io.airmedy.on", true); err != nil {
		t.Fatalf("seed enabled flag: %v", err)
	}

	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.StartEnabled(ctx); err != nil {
		t.Fatalf("StartEnabled: %v", err)
	}

	started := factory.startedIDs()
	if len(started) != 1 || started[0] != "io.airmedy.on" {
		t.Fatalf("started = %v, want just the enabled plugin", started)
	}
	if factory.created("io.airmedy.off") != nil {
		t.Fatal("a disabled plugin must not start")
	}
}

// TestStartEnabledIsolatesFailures: one broken plugin must not stop the others,
// and the app must still come up.
func TestStartEnabledIsolatesFailures(t *testing.T) {
	good := providerManifest("io.airmedy.good", "wy")
	bad := providerManifest("io.airmedy.bad", "kg")

	service, factory, settings := newTestServiceWithSettings(
		t, loaderWith(good, bad), Config{Root: "/plugins"}, nil)
	factory.errs = map[string]error{"io.airmedy.bad": errors.New("handshake refused")}
	if err := settings.Set(context.Background(), "io.airmedy.good", true); err != nil {
		t.Fatalf("seed enabled flag: %v", err)
	}
	if err := settings.Set(context.Background(), "io.airmedy.bad", true); err != nil {
		t.Fatalf("seed enabled flag: %v", err)
	}

	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	err := service.StartEnabled(ctx)
	if err == nil {
		t.Fatal("StartEnabled should report the failed plugin")
	}
	if !strings.Contains(err.Error(), "io.airmedy.bad") {
		t.Fatalf("error should name the failing plugin, got: %v", err)
	}

	byID := map[string]PluginInfo{}
	for _, info := range service.List() {
		byID[info.Manifest.ID] = info
	}
	if !byID["io.airmedy.good"].Running {
		t.Fatal("a failure in one plugin must not stop the healthy one")
	}
	if byID["io.airmedy.good"].Status != domain.PluginStatusRunning {
		t.Fatalf("good status = %q, want running", byID["io.airmedy.good"].Status)
	}
	if byID["io.airmedy.bad"].Status != domain.PluginStatusFailed {
		t.Fatalf("bad status = %q, want failed", byID["io.airmedy.bad"].Status)
	}
	if !strings.Contains(byID["io.airmedy.bad"].LastError, "handshake") {
		t.Fatalf("bad LastError = %q, want the start failure", byID["io.airmedy.bad"].LastError)
	}
}

// TestSetEnabledPersistsAndStarts covers the toggle path: the flag is written
// before anything is launched so a crash during start does not lose the choice.
func TestSetEnabledPersistsAndStarts(t *testing.T) {
	service, factory, settings := newTestServiceWithSettings(
		t, loaderWith(providerManifest("io.airmedy.owner", "wy")), Config{Root: "/plugins"}, nil)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if err := service.SetEnabled(ctx, "io.airmedy.owner", true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if ids := settings.enabledIDs(); len(ids) != 1 || ids[0] != "io.airmedy.owner" {
		t.Fatalf("persisted = %v, want the plugin enabled", ids)
	}
	if factory.created("io.airmedy.owner") == nil {
		t.Fatal("enabling a plugin must start it")
	}
	if _, ok := service.HostForProvider(ctx, "wy"); !ok {
		t.Fatal("an enabled owner must be routable")
	}

	if err := service.SetEnabled(ctx, "io.airmedy.owner", false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if !factory.created("io.airmedy.owner").isClosed() {
		t.Fatal("disabling a plugin must stop it")
	}
	if infos := service.List(); infos[0].Running {
		t.Fatal("a disabled plugin must report not running")
	}
	if _, ok := service.HostForProvider(ctx, "wy"); ok {
		t.Fatal("a disabled owner must stop being routable")
	}
}

// TestSetEnabledUnknownPlugin keeps a stale UI toggle from writing a row.
func TestSetEnabledUnknownPlugin(t *testing.T) {
	service, _, _ := newTestServiceWithSettings(t, &fakeLoader{}, Config{Root: "/plugins"}, nil)
	if err := service.SetEnabled(context.Background(), "io.airmedy.ghost", true); !errors.Is(err, domain.ErrPluginNotFound) {
		t.Fatalf("SetEnabled(unknown) = %v, want ErrPluginNotFound", err)
	}
}

// TestSetEnabledWithoutStore explains the failure instead of panicking when no
// settings repository is bound.
func TestSetEnabledWithoutStore(t *testing.T) {
	service := NewService(&fakeLoader{}, (&fakeFactory{hosts: map[string]*fakeHost{}}).start, nil, testLogger(), Config{})
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	err := service.SetEnabled(context.Background(), "io.airmedy.any", true)
	if !errors.Is(err, domain.ErrPluginSettingsUnavailable) {
		t.Fatalf("SetEnabled = %v, want ErrPluginSettingsUnavailable", err)
	}
}

// TestStatusReportsLifecycle covers the three states the UI renders.
func TestStatusReportsLifecycle(t *testing.T) {
	service, factory := newTestService(t, loaderWith(providerManifest("io.airmedy.owner", "wy")), Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	status, reason, err := service.Status("io.airmedy.owner")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != domain.PluginStatusIdle || reason != "" {
		t.Fatalf("idle status = %q/%q", status, reason)
	}

	if err := service.Start(ctx, "io.airmedy.owner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if status, _, _ = service.Status("io.airmedy.owner"); status != domain.PluginStatusRunning {
		t.Fatalf("status = %q, want running", status)
	}

	if err := service.Stop(ctx, "io.airmedy.owner"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if status, _, _ = service.Status("io.airmedy.owner"); status != domain.PluginStatusIdle {
		t.Fatalf("status = %q, want idle after Stop", status)
	}

	if _, _, err := service.Status("io.airmedy.ghost"); !errors.Is(err, domain.ErrPluginNotFound) {
		t.Fatalf("Status(unknown) = %v, want ErrPluginNotFound", err)
	}
	_ = factory
}

// TestStatusReportsStartFailure keeps the reason available for the UI's
// expandable error row.
func TestStatusReportsStartFailure(t *testing.T) {
	service, factory := newTestService(t, loaderWith(providerManifest("io.airmedy.broken", "wy")), Config{Root: "/plugins"})
	factory.err = errors.New("handshake refused")
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if err := service.Start(ctx, "io.airmedy.broken"); err == nil {
		t.Fatal("Start should fail")
	}
	status, reason, err := service.Status("io.airmedy.broken")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != domain.PluginStatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	if !strings.Contains(reason, "handshake") {
		t.Fatalf("reason = %q, want the start failure", reason)
	}
}

// TestRunningHostsOnlyReturnsRunningPlugins is the coarse "what is up" view: a
// stopped plugin is not in it.
func TestRunningHostsOnlyReturnsRunningPlugins(t *testing.T) {
	running := providerManifest("io.airmedy.running", "wy")
	stopped := providerManifest("io.airmedy.stopped", "kg")

	service, _ := newTestService(t, loaderWith(running, stopped), Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.Start(ctx, "io.airmedy.running"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	hosts := service.RunningHosts(ctx)
	if len(hosts) != 1 {
		t.Fatalf("RunningHosts = %d hosts, want just the running plugin", len(hosts))
	}
	if hosts[0].Manifest().ID != "io.airmedy.running" {
		t.Fatalf("RunningHosts[0] = %q", hosts[0].Manifest().ID)
	}
}

// TestEnableFlagsDefaultOff is the "default off" rule at the registry level: no
// rows means nothing runs, even right after Scan.
func TestEnableFlagsDefaultOff(t *testing.T) {
	service, factory := newTestService(t, loaderWith(providerManifest("io.airmedy.owner", "wy")), Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := service.StartEnabled(ctx); err != nil {
		t.Fatalf("StartEnabled: %v", err)
	}
	if factory.startCount() != 0 {
		t.Fatal("a plugin with no settings row must not be auto-started")
	}
	if info := findInfo(t, service.List(), "io.airmedy.owner"); info.Enabled || info.EffectiveEnabled() {
		t.Fatal("a plugin with no settings row must report disabled")
	}
}

// TestLoadRejudgesAlreadyRegisteredPlugins: adding a manifest through Load can
// invalidate a plugin that was fine when it was registered alone.
func TestLoadRejudgesAlreadyRegisteredPlugins(t *testing.T) {
	first := providerManifest("io.airmedy.alpha", "wy")
	secondPath := "/plugins/beta/airmedy-plugin.json"
	// The second manifest is installed only after the first scan, which is the
	// situation Load exists for: a plugin copied in without an app restart.
	loader := loaderWith(first)

	service, _ := newTestService(t, loader, Config{Root: "/plugins"})
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if info := findInfo(t, service.List(), "io.airmedy.alpha"); info.Invalid {
		t.Fatalf("the first plugin starts out valid, got: %s", info.LastError)
	}

	loader.paths = append(loader.paths, secondPath)
	loader.manifests[secondPath] = providerManifest("io.airmedy.beta", "wy")
	if _, err := service.Load(ctx, secondPath); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info := findInfo(t, service.List(), "io.airmedy.alpha"); !info.Invalid {
		t.Fatal("Load must re-judge the plugins already registered")
	}
}

// findInfo returns the entry for id or fails the test.
func findInfo(t *testing.T, infos []PluginInfo, id string) PluginInfo {
	t.Helper()
	for _, info := range infos {
		if info.Manifest.ID == id {
			return info
		}
	}
	t.Fatalf("plugin %q missing from %+v", id, infos)
	return PluginInfo{}
}
