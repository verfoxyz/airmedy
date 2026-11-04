package plugin

import (
	"context"
	"testing"

	"airmedy/internal/domain"

	"go.uber.org/fx"
)

// TestModuleWiring drives the fx module with fake ports, so the graph main.go
// assembles is proven without building the Wails entry point (which needs the
// audio C libraries this machine does not have).
func TestModuleWiring(t *testing.T) {
	loader := &fakeLoader{
		paths:     []string{"/plugins/a/airmedy-plugin.json"},
		manifests: map[string]domain.PluginManifest{"/plugins/a/airmedy-plugin.json": testManifest("io.airmedy.a")},
	}
	factory := &fakeFactory{hosts: make(map[string]*fakeHost)}

	var service *Service
	app := fx.New(
		Module,
		fx.NopLogger,
		fx.Supply(Config{Root: "/plugins"}),
		fx.Supply(testLogger()),
		fx.Provide(func() domain.PluginLoader { return loader }),
		fx.Provide(func() domain.PluginHostFactory { return factory.start }),
		fx.Provide(func() domain.PluginSettingsRepository { return newFakeSettings() }),
		fx.Populate(&service),
	)

	startCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start module: %v", err)
	}

	// OnStart scans the root and registers what it finds.
	infos := service.List()
	if len(infos) != 1 || infos[0].Manifest.ID != "io.airmedy.a" {
		t.Fatalf("plugin not registered by module start: %+v", infos)
	}

	// A plugin started by the app is closed when the lifecycle stops.
	if err := service.Start(startCtx, "io.airmedy.a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("stop module: %v", err)
	}
	if !factory.created("io.airmedy.a").isClosed() {
		t.Fatal("module shutdown left the plugin running")
	}
}
