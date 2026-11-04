package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"airmedy/internal/domain"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRegistry is the provider table the resolver looks a route up in:
// provider id → the plugin that declared it.
type fakeRegistry struct {
	byProvider map[string]domain.PluginHost
	// asked records every provider lookup, so a test can prove the resolver
	// consulted the table instead of polling plugins.
	asked []string
}

func (r *fakeRegistry) HostForProvider(ctx context.Context, provider string) (domain.PluginHost, bool) {
	if err := ctx.Err(); err != nil {
		return nil, false
	}
	r.asked = append(r.asked, provider)
	host, ok := r.byProvider[provider]
	if !ok || host == nil {
		return nil, false
	}
	return host, true
}

// fakeHost answers one plugin call from a canned result or error, recording what
// the host sent so the wire contract can be asserted.
type fakeHost struct {
	id         string
	mu         sync.Mutex
	calls      int
	lastMethod string
	lastParams domain.PluginStreamRequest
	result     domain.PluginStreamResult
	err        error
	closed     bool
}

func (h *fakeHost) Manifest() domain.PluginManifest { return domain.PluginManifest{ID: h.id} }

func (h *fakeHost) Capabilities() []domain.Capability {
	return []domain.Capability{domain.CapabilityStreamResolve}
}

func (h *fakeHost) Call(_ context.Context, method string, params, result any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.lastMethod = method

	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &h.lastParams); err != nil {
		return err
	}
	if h.err != nil {
		return h.err
	}
	encoded, err := json.Marshal(h.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

func (h *fakeHost) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

func (h *fakeHost) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *fakeHost) method() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastMethod
}

func (h *fakeHost) params() domain.PluginStreamRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastParams
}

func pluginTrack(provider string) *domain.TrackDTO {
	return &domain.TrackDTO{
		Track: domain.Track{
			ID:             "track-1",
			Provider:       provider,
			ExternalID:     "509781655",
			Title:          "晴天",
			Duration:       269,
			RawArtistNames: "周杰伦",
		},
		Album: &domain.Album{Title: "叶惠美"},
	}
}

func TestPluginResolverMapsRequestAndResult(t *testing.T) {
	host := &fakeHost{result: domain.PluginStreamResult{
		URL:       "http://cdn.example/a.mp3",
		Headers:   map[string]string{"Referer": "https://music.163.com/"},
		ExpiresAt: "2026-11-02T15:00:00Z",
	}}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"io.airmedy.wy": host}}
	resolver := NewPluginResolver(registry, testLogger())

	track := pluginTrack("io.airmedy.wy")
	track.Artists = []*domain.Artist{{Name: "周杰伦"}}
	ref, err := resolver.Resolve(context.Background(), track, "320k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if host.method() != domain.MethodStreamResolve {
		t.Fatalf("method = %q, want %q", host.method(), domain.MethodStreamResolve)
	}
	params := host.params()
	if params.Source != "io.airmedy.wy" || params.ID != "509781655" || params.Quality != "320k" {
		t.Fatalf("params = %+v", params)
	}
	if params.Metadata.Title != "晴天" || params.Metadata.Duration != 269 || params.Metadata.Album != "叶惠美" {
		t.Fatalf("metadata = %+v", params.Metadata)
	}
	if len(params.Metadata.Artists) != 1 || params.Metadata.Artists[0] != "周杰伦" {
		t.Fatalf("artists = %v", params.Metadata.Artists)
	}
	if ref.URL != "http://cdn.example/a.mp3" {
		t.Fatalf("url = %q", ref.URL)
	}
	if ref.Headers["Referer"] != "https://music.163.com/" {
		t.Fatalf("headers = %v", ref.Headers)
	}
	if ref.Expires.IsZero() {
		t.Fatal("expiresAt was not parsed")
	}
}

// TestPluginResolverFallsBackToRawArtists covers tracks loaded without the
// relation populated: the metadata hint must still be usable.
func TestPluginResolverFallsBackToRawArtists(t *testing.T) {
	host := &fakeHost{result: domain.PluginStreamResult{URL: "http://cdn/a.mp3"}}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
	resolver := NewPluginResolver(registry, testLogger())

	track := pluginTrack("p")
	track.RawArtistNames = "周杰伦; 方文山"
	if _, err := resolver.Resolve(context.Background(), track, "320k"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	artists := host.params().Metadata.Artists
	if len(artists) != 2 || artists[0] != "周杰伦" || artists[1] != "方文山" {
		t.Fatalf("artists = %v", artists)
	}
}

// TestPluginResolverUnknownProvider is acceptance case 4: a provider nobody
// declares means a permanent, explained failure that names the provider.
func TestPluginResolverUnknownProvider(t *testing.T) {
	registry := &fakeRegistry{}
	resolver := NewPluginResolver(registry, testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack("io.airmedy.missing"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("expected ErrStreamUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "io.airmedy.missing") {
		t.Fatalf("error should name the provider, got: %v", err)
	}
	if !strings.Contains(err.Error(), "providers") {
		t.Fatalf("error should tell the user how to fix it, got: %v", err)
	}
	if policy := domain.ClassifyDownloadError(err, 0, 5); policy.Retryable {
		t.Fatalf("unknown provider must not be retried: %+v", policy)
	}
	// The lookup is what failed; no plugin call was attempted.
	if len(registry.asked) != 1 || registry.asked[0] != "io.airmedy.missing" {
		t.Fatalf("registry asked = %v, want one lookup for the provider", registry.asked)
	}
}

// TestPluginResolverEmptyProviderIsReadable: a remote track without a provider
// is unidentifiable, so it must fail before touching the registry and explain
// what is missing.
func TestPluginResolverEmptyProviderIsReadable(t *testing.T) {
	registry := &fakeRegistry{}
	resolver := NewPluginResolver(registry, testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack(""), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("expected ErrStreamUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("error should say the track has no provider, got: %v", err)
	}
	if resolver.CanResolve("") {
		t.Fatal("an empty provider must never be resolvable")
	}
	if len(registry.asked) != 0 {
		t.Fatalf("registry asked = %v, want no lookup for an empty provider", registry.asked)
	}
}

// TestPluginResolverMapsStructuredErrors is acceptance case 5 and the rest of
// the error table: only data.kind is consulted, never the message text.
func TestPluginResolverMapsStructuredErrors(t *testing.T) {
	cases := []struct {
		name      string
		rpcError  *domain.RPCError
		want      error
		retryable bool
	}{
		{
			name:      "rate limited",
			rpcError:  &domain.RPCError{Code: 4200, Message: "slow down", Data: json.RawMessage(`{"kind":"rate_limited"}`)},
			want:      domain.ErrStreamRateLimited,
			retryable: true,
		},
		{
			name:      "unavailable",
			rpcError:  &domain.RPCError{Code: 4201, Message: "copyright", Data: json.RawMessage(`{"kind":"unavailable"}`)},
			want:      domain.ErrStreamUnavailable,
			retryable: false,
		},
		{
			name:      "vip",
			rpcError:  &domain.RPCError{Code: 4202, Message: "vip only", Data: json.RawMessage(`{"kind":"vip"}`)},
			want:      domain.ErrStreamUnavailable,
			retryable: false,
		},
		{
			name:      "auth",
			rpcError:  &domain.RPCError{Code: 4203, Message: "forbidden", Data: json.RawMessage(`{"kind":"auth"}`)},
			want:      domain.ErrStreamAuth,
			retryable: false,
		},
		{
			name:      "unknown kind",
			rpcError:  &domain.RPCError{Code: 4204, Message: "who knows", Data: json.RawMessage(`{"kind":"weird"}`)},
			want:      nil,
			retryable: true,
		},
		{
			name:      "wording only is not parsed",
			rpcError:  &domain.RPCError{Code: 4205, Message: "429 too many requests vip"},
			want:      nil,
			retryable: true,
		},
		{
			name:      "malformed data",
			rpcError:  &domain.RPCError{Code: 4206, Message: "bad", Data: json.RawMessage(`"rate_limited"`)},
			want:      nil,
			retryable: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeHost{err: tc.rpcError}
			registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
			resolver := NewPluginResolver(registry, testLogger())

			_, err := resolver.Resolve(context.Background(), pluginTrack("p"), "320k")
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want sentinel %v", err, tc.want)
			}
			if tc.want == nil && errors.Is(err, domain.ErrStreamRateLimited) {
				t.Fatalf("error must stay unclassified, got %v", err)
			}
			if policy := domain.ClassifyDownloadError(err, 0, 5); policy.Retryable != tc.retryable {
				t.Fatalf("retryable = %v, want %v (%+v)", policy.Retryable, tc.retryable, policy)
			}
		})
	}
}

// TestPluginResolverPassesCrashesThrough is acceptance case 4: a -32000 must not
// be swallowed, and the download service retries it.
func TestPluginResolverPassesCrashesThrough(t *testing.T) {
	crash := &domain.RPCError{Code: domain.RPCErrorCrashed, Message: "plugin exited"}
	host := &fakeHost{err: crash}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
	resolver := NewPluginResolver(registry, testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack("p"), "320k")
	if !errors.Is(err, domain.ErrPluginCrashed) {
		t.Fatalf("expected ErrPluginCrashed to survive, got %v", err)
	}
	if domain.RPCErrorCode(err) != domain.RPCErrorCrashed {
		t.Fatalf("RPCErrorCode = %d, want %d", domain.RPCErrorCode(err), domain.RPCErrorCrashed)
	}
	if policy := domain.ClassifyDownloadError(err, 0, 5); !policy.Retryable {
		t.Fatalf("a crashed plugin must be retryable: %+v", policy)
	}
}

// TestPluginResolverDoesNotCacheURLs pins the stream contract: every attempt
// resolves again, because the URLs expire.
func TestPluginResolverDoesNotCacheURLs(t *testing.T) {
	host := &fakeHost{result: domain.PluginStreamResult{URL: "http://cdn/a.mp3"}}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
	resolver := NewPluginResolver(registry, testLogger())

	for i := 0; i < 3; i++ {
		if _, err := resolver.Resolve(context.Background(), pluginTrack("p"), "320k"); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
	}
	if calls := host.callCount(); calls != 3 {
		t.Fatalf("plugin called %d times, want 3 (no URL caching)", calls)
	}
}

// TestPluginResolverMapsMissingMethod keeps a plugin without the stream
// capability from burning the retry budget forever.
func TestPluginResolverMapsMissingMethod(t *testing.T) {
	host := &fakeHost{err: &domain.RPCError{Code: domain.RPCErrorMethodNotFound, Message: "no method"}}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
	resolver := NewPluginResolver(registry, testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack("p"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("expected ErrStreamUnavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "does not implement") == false {
		t.Fatalf("error should explain the missing method, got: %v", err)
	}
	if policy := domain.ClassifyDownloadError(err, 0, 5); policy.Retryable {
		t.Fatalf("a missing method must be permanent: %+v", policy)
	}
}

func TestPluginResolverRejectsMissingExternalID(t *testing.T) {
	host := &fakeHost{result: domain.PluginStreamResult{URL: "http://cdn/a.mp3"}}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
	resolver := NewPluginResolver(registry, testLogger())

	track := pluginTrack("p")
	track.ExternalID = ""
	_, err := resolver.Resolve(context.Background(), track, "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("expected ErrStreamUnavailable, got %v", err)
	}
	if calls := host.callCount(); calls != 0 {
		t.Fatalf("plugin was called %d times without an external id", calls)
	}
}

func TestPluginResolverRejectsEmptyURL(t *testing.T) {
	host := &fakeHost{result: domain.PluginStreamResult{URL: "   "}}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
	resolver := NewPluginResolver(registry, testLogger())

	_, err := resolver.Resolve(context.Background(), pluginTrack("p"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("expected ErrStreamUnavailable, got %v", err)
	}
}

// TestPluginResolverToleratesBadExpiry pins that expiresAt is advisory only.
func TestPluginResolverToleratesBadExpiry(t *testing.T) {
	host := &fakeHost{result: domain.PluginStreamResult{
		URL:       "http://cdn/a.mp3",
		ExpiresAt: "not-a-timestamp",
	}}
	registry := &fakeRegistry{byProvider: map[string]domain.PluginHost{"p": host}}
	resolver := NewPluginResolver(registry, testLogger())

	ref, err := resolver.Resolve(context.Background(), pluginTrack("p"), "320k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ref.URL != "http://cdn/a.mp3" {
		t.Fatalf("url = %q", ref.URL)
	}
	if !ref.Expires.IsZero() {
		t.Fatalf("Expires = %v, want zero", ref.Expires)
	}
}

func TestPluginResolverNilTrack(t *testing.T) {
	resolver := NewPluginResolver(&fakeRegistry{}, testLogger())
	if _, err := resolver.Resolve(context.Background(), nil, "320k"); err == nil {
		t.Fatal("expected an error for a nil track")
	}
}

func TestPluginResolverNilRegistry(t *testing.T) {
	resolver := NewPluginResolver(nil, testLogger())
	_, err := resolver.Resolve(context.Background(), pluginTrack("p"), "320k")
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("expected ErrStreamUnavailable, got %v", err)
	}
}
