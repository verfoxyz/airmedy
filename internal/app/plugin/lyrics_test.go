package plugin

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"airmedy/internal/domain"
)

func TestHasLyricTimeline(t *testing.T) {
	if !hasLyricTimeline("[00:01.00]晴天\n[00:02.50]故事") {
		t.Fatal("a standard LRC timeline must be accepted")
	}
	if hasLyricTimeline("晴天\n故事的小黄花") {
		t.Fatal("plain text must be rejected")
	}
	if !hasLyricTimeline("[01:02:03]legacy label") {
		t.Fatal("a 1-3 digit minute label must be accepted")
	}
}

func TestHostForLyricSourceRoutesByManifestSources(t *testing.T) {
	service := NewService(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})
	manifest := domain.PluginManifest{
		ID:           "wy",
		Capabilities: []domain.Capability{domain.CapabilityLyricFetch},
		Sources:      []string{"wy"},
	}
	if err := service.Register(manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	host := &fakeHost{manifest: manifest, capabilities: manifest.Capabilities}
	service.hosts["wy"] = host
	service.entries["wy"].status = domain.PluginStatusRunning

	found, ok := service.HostForLyricSource(context.Background(), "WY")
	if !ok || found != host {
		t.Fatalf("HostForLyricSource(wy) = %v, %v", found, ok)
	}

	// A provider the plugin only searches but does not declare under sources must
	// not receive lyric requests.
	if _, ok := service.HostForLyricSource(context.Background(), "kg"); ok {
		t.Fatal("an undeclared provider was routed")
	}

	// A stopped plugin is not routable.
	service.hosts = map[string]domain.PluginHost{}
	if _, ok := service.HostForLyricSource(context.Background(), "wy"); ok {
		t.Fatal("a stopped plugin was routed")
	}
}
