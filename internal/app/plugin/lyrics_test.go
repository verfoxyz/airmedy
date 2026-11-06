package plugin

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"airmedy/internal/domain"
)

// lyricReplyHost answers lyric.fetch with a canned result. It is a plugin host
// double for the translation contract only; routing itself is covered above.
type lyricReplyHost struct {
	manifest domain.PluginManifest
	reply    domain.PluginLyricResult
}

func (h *lyricReplyHost) Manifest() domain.PluginManifest { return h.manifest }
func (h *lyricReplyHost) Capabilities() []domain.Capability {
	return h.manifest.Capabilities
}
func (h *lyricReplyHost) Call(_ context.Context, _ string, _ any, result any) error {
	if target, ok := result.(*domain.PluginLyricResult); ok {
		*target = h.reply
	}
	return nil
}
func (h *lyricReplyHost) Close() error { return nil }

func TestLyricsAdapterStoresTheTranslationApart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(nil, nil, nil, logger, Config{})
	manifest := domain.PluginManifest{
		ID:           "wy",
		Capabilities: []domain.Capability{domain.CapabilityLyricFetch},
		Sources:      []string{"wy"},
	}
	if err := service.Register(manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	host := &lyricReplyHost{
		manifest: manifest,
		reply: domain.PluginLyricResult{
			Lyric:  "[00:01.00]晴天",
			TLyric: "[00:01.00]Clear sky",
			Source: "wy-plugin",
		},
	}
	service.hosts["wy"] = host
	service.entries["wy"].status = domain.PluginStatusRunning

	lyric, handled, err := NewLyricsAdapter(service, logger).FetchForTrack(context.Background(), &domain.TrackDTO{
		Track: domain.Track{ID: "t1", Provider: "wy", ExternalID: "1", Title: "晴天"},
	})
	if err != nil || !handled {
		t.Fatalf("FetchForTrack = %v, %v; want a handled answer", lyric, err)
	}
	if lyric.Content != "[00:01.00]晴天" || lyric.TranslationContent != "[00:01.00]Clear sky" {
		t.Fatalf("lyric = %+v; want original and translation stored separately", lyric)
	}
	if lyric.TranslationSource != "wy-plugin-translation" {
		t.Fatalf("TranslationSource = %q, want the translation origin", lyric.TranslationSource)
	}
}

func TestLyricsAdapterLeavesTheTranslationEmptyWhenNoneIsReturned(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(nil, nil, nil, logger, Config{})
	manifest := domain.PluginManifest{
		ID:           "wy",
		Capabilities: []domain.Capability{domain.CapabilityLyricFetch},
		Sources:      []string{"wy"},
	}
	if err := service.Register(manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	host := &lyricReplyHost{
		manifest: manifest,
		reply:    domain.PluginLyricResult{Lyric: "[00:01.00]晴天", Source: "wy-plugin"},
	}
	service.hosts["wy"] = host
	service.entries["wy"].status = domain.PluginStatusRunning

	lyric, handled, err := NewLyricsAdapter(service, logger).FetchForTrack(context.Background(), &domain.TrackDTO{
		Track: domain.Track{ID: "t1", Provider: "wy", ExternalID: "1", Title: "晴天"},
	})
	if err != nil || !handled {
		t.Fatalf("FetchForTrack = %v, %v; want a handled answer", lyric, err)
	}
	if lyric.TranslationContent != "" || lyric.TranslationSource != "" {
		t.Fatalf("lyric = %+v; want an empty translation", lyric)
	}
}

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
