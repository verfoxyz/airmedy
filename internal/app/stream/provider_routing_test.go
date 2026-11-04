package stream

import (
	"context"
	"errors"
	"strings"
	"testing"

	"airmedy/internal/domain"
)

// providerTable builds the registry as the routing table the app maintains:
// provider id → the one plugin that declared it.
func providerTable(routes map[string]*fakeHost) *fakeRegistry {
	registry := &fakeRegistry{byProvider: make(map[string]domain.PluginHost, len(routes))}
	for provider, host := range routes {
		registry.byProvider[provider] = host
	}
	return registry
}

// TestProviderRoutingCallsOnlyTheOwner is acceptance case 3: the table points at
// exactly one plugin, so every other running plugin is left untouched.
func TestProviderRoutingCallsOnlyTheOwner(t *testing.T) {
	owner := &fakeHost{id: "io.airmedy.ikun", result: domain.PluginStreamResult{URL: "http://cdn/ikun.mp3"}}
	other := &fakeHost{id: "io.airmedy.other", result: domain.PluginStreamResult{URL: "http://cdn/other.mp3"}}
	resolver := NewPluginResolver(providerTable(map[string]*fakeHost{
		"wy": owner,
		"kg": other,
	}), testLogger())

	ref, err := resolver.Resolve(context.Background(), pluginTrack("wy"), "320k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ref.URL != "http://cdn/ikun.mp3" {
		t.Fatalf("url = %q, want the declaring plugin's answer", ref.URL)
	}
	if owner.callCount() != 1 {
		t.Fatalf("owner called %d times, want 1", owner.callCount())
	}
	if other.callCount() != 0 {
		t.Fatalf("the plugin that does not declare the provider was called %d times, want 0", other.callCount())
	}
}

// TestProviderRoutingNormalisesTheLookup: a track row may carry " WY " while the
// manifest declares "wy"; both sides meet in the canonical form.
func TestProviderRoutingNormalisesTheLookup(t *testing.T) {
	owner := &fakeHost{id: "io.airmedy.ikun", result: domain.PluginStreamResult{URL: "http://cdn/ikun.mp3"}}
	registry := providerTable(map[string]*fakeHost{"wy": owner})
	resolver := NewPluginResolver(registry, testLogger())

	track := pluginTrack("  WY ")
	if _, err := resolver.Resolve(context.Background(), track, "320k"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(registry.asked) != 1 || registry.asked[0] != "wy" {
		t.Fatalf("registry asked = %v, want the normalised provider", registry.asked)
	}
	if got := owner.params().Source; got != "wy" {
		t.Fatalf("params.source = %q, want the normalised provider the plugin expects", got)
	}
}

// TestProviderRoutingMissIsPermanent is acceptance case 4: no declaration means
// nobody is called and the failure names the provider.
func TestProviderRoutingMissIsPermanent(t *testing.T) {
	owner := &fakeHost{id: "io.airmedy.ikun", result: domain.PluginStreamResult{URL: "http://cdn/ikun.mp3"}}
	resolver := NewPluginResolver(providerTable(map[string]*fakeHost{"wy": owner}), testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack("nobody.serves.this"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("error = %v, want ErrStreamUnavailable", err)
	}
	if !strings.Contains(err.Error(), "nobody.serves.this") {
		t.Fatalf("error should name the provider, got: %v", err)
	}
	if owner.callCount() != 0 {
		t.Fatalf("a plugin served a provider it never declared (%d calls)", owner.callCount())
	}
	if policy := domain.ClassifyDownloadError(err, 0, 5); policy.Retryable {
		t.Fatalf("nobody claiming the provider must not be retried: %+v", policy)
	}
}

// TestProviderRoutingFailureDoesNotFallThrough: the owner's failure is final. A
// second plugin serving another provider could have the same track id, and
// silently asking it would swap the source the user chose.
func TestProviderRoutingFailureDoesNotFallThrough(t *testing.T) {
	owner := &fakeHost{id: "io.airmedy.ikun", err: &domain.RPCError{
		Code: 4202, Message: "vip only", Data: []byte(`{"kind":"vip"}`),
	}}
	other := &fakeHost{id: "io.airmedy.other", result: domain.PluginStreamResult{URL: "http://cdn/other.mp3"}}
	resolver := NewPluginResolver(providerTable(map[string]*fakeHost{
		"wy": owner,
		"kg": other,
	}), testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack("wy"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("error = %v, want the VIP failure", err)
	}
	if other.callCount() != 0 {
		t.Fatal("a definitive failure must not fall through to another provider's plugin")
	}
}

// TestPluginWithoutProvidersIsNeverRouted is acceptance case 5 at the resolver
// level: a plugin that declares no provider simply never appears in the table,
// and that is not an error.
func TestPluginWithoutProvidersIsNeverRouted(t *testing.T) {
	lyrics := &fakeHost{id: "io.airmedy.lyrics"}
	registry := providerTable(map[string]*fakeHost{"wy": &fakeHost{
		id: "io.airmedy.ikun", result: domain.PluginStreamResult{URL: "http://cdn/ikun.mp3"},
	}})
	resolver := NewPluginResolver(registry, testLogger())

	// A lyrics-only plugin is running alongside the source plugin; resolving a
	// provider it does not declare neither calls it nor fails the request.
	if _, err := resolver.Resolve(context.Background(), pluginTrack("wy"), "320k"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lyrics.callCount() != 0 {
		t.Fatalf("the provider-less plugin was called %d times, want 0", lyrics.callCount())
	}
}

// TestProviderRoutingNeverReachesABuiltInResolver pins the end state: there is
// no built-in resolver, so a provider no plugin declares fails even when other
// providers have an owner.
func TestProviderRoutingNeverReachesABuiltInResolver(t *testing.T) {
	owner := &fakeHost{id: "io.airmedy.ikun", result: domain.PluginStreamResult{URL: "http://cdn/b.mp3"}}
	resolver := NewPluginResolver(providerTable(map[string]*fakeHost{"wy": owner}), testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack("kg"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("error = %v, want ErrStreamUnavailable for an undeclared provider", err)
	}
	if !strings.Contains(err.Error(), "kg") {
		t.Fatalf("error should name the provider, got: %v", err)
	}
	if owner.callCount() != 0 {
		t.Fatal("a plugin served a provider it never declared")
	}
}
