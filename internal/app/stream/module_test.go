package stream

import (
	"context"
	"errors"
	"testing"

	"airmedy/internal/domain"

	"go.uber.org/fx"
)

// TestModuleProvidesTheDownloadResolver drives the fx graph main.go assembles:
// the plugin resolver is what the download service receives, a declared provider
// is resolved through its owning plugin, and an unknown provider fails with the
// explained permanent error.
func TestModuleProvidesTheDownloadResolver(t *testing.T) {
	owner := &fakeHost{id: "io.airmedy.wy", result: domain.PluginStreamResult{URL: "http://cdn/a.mp3"}}
	registry := providerTable(map[string]*fakeHost{"wy": owner})

	var resolver domain.StreamResolver
	app := fx.New(
		Module,
		fx.NopLogger,
		fx.Supply(testLogger()),
		fx.Provide(func() ProviderRegistry { return registry }),
		fx.Populate(&resolver),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("fx graph: %v", err)
	}

	ref, err := resolver.Resolve(context.Background(), pluginTrack("wy"), "320k")
	if err != nil {
		t.Fatalf("Resolve declared provider: %v", err)
	}
	if ref.URL != "http://cdn/a.mp3" || owner.callCount() != 1 {
		t.Fatalf("plugin route not used: ref=%+v calls=%d", ref, owner.callCount())
	}

	_, err = resolver.Resolve(context.Background(), pluginTrack("io.airmedy.missing"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("unknown provider = %v, want ErrStreamUnavailable", err)
	}
}
