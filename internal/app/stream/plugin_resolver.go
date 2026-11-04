// Package stream turns a remote track into a directly playable URL through the
// enabled plugin that owns the track's provider.
//
// A plugin declares the `stream.resolve` capability and the providers it serves,
// and the host calls it with the platform id pair, because real audio-source
// scripts only accept ids, not search metadata (see plugins/lx/README.md).
// There is no built-in resolver: a provider that no enabled plugin declares is
// unplayable.
//
// Routing is a lookup, not a poll: the manifest says which plugin owns a
// provider, so exactly one plugin is called and no plugin is ever asked "is this
// yours?". A provider nobody declares fails with the provider named.
//
// This package depends on domain only: the plugin registry is reached through
// the narrow ProviderRegistry port, so app never imports app/plugin or infra.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// ProviderRegistry is the slice of the plugin registry the resolver needs: the
// running plugin that declared a provider.
//
// A provider is a manifest declaration, not a plugin id, so the answer is looked
// up rather than inferred. *plugin.Service satisfies this port.
type ProviderRegistry interface {
	// HostForProvider returns the running plugin that declared provider, or
	// false when no plugin claims it (or the one that does is not running).
	HostForProvider(ctx context.Context, provider string) (domain.PluginHost, bool)
}

// PluginResolver resolves a track through the plugin that declared its provider.
//
// It never caches a URL: stream URLs are short-lived and the download service
// re-resolves before every attempt (the StreamRef contract says so).
type PluginResolver struct {
	registry ProviderRegistry
	logger   *slog.Logger
}

// NewPluginResolver returns a resolver over registry.
func NewPluginResolver(registry ProviderRegistry, logger *slog.Logger) *PluginResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &PluginResolver{registry: registry, logger: logger}
}

// Resolve implements domain.StreamResolver.
//
// The provider is normalised the same way the manifest is, so " WY " in a track
// row and "wy" in a manifest are the same route. Exactly one plugin is called;
// there is no fallback to a second one, because that would silently swap the
// source the user chose.
func (r *PluginResolver) Resolve(ctx context.Context, track *domain.TrackDTO, quality string) (domain.StreamRef, error) {
	if track == nil {
		return domain.StreamRef{}, fmt.Errorf("resolve stream: nil track")
	}
	if r.registry == nil {
		return domain.StreamRef{}, fmt.Errorf("%w: no plugin registry", domain.ErrStreamUnavailable)
	}

	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" {
		return domain.StreamRef{}, fmt.Errorf(
			"%w: track %s has no provider (remote tracks must carry the provider id a plugin declares)",
			domain.ErrStreamUnavailable, track.ID)
	}
	if strings.TrimSpace(track.ExternalID) == "" {
		return domain.StreamRef{}, fmt.Errorf(
			"%w: provider %q track %s has no external id", domain.ErrStreamUnavailable, provider, track.ID)
	}

	host, ok := r.registry.HostForProvider(ctx, provider)
	if !ok {
		return domain.StreamRef{}, fmt.Errorf(
			"%w: no enabled plugin declares provider %q (install one that lists %q under \"providers\" and turn it on)",
			domain.ErrStreamUnavailable, provider, provider)
	}

	return r.resolveWith(ctx, host, track, quality)
}

// resolveWith performs the one declared plugin's attempt and classifies its
// answer.
func (r *PluginResolver) resolveWith(
	ctx context.Context, host domain.PluginHost, track *domain.TrackDTO, quality string,
) (domain.StreamRef, error) {
	var result domain.PluginStreamResult
	err := host.Call(ctx, domain.MethodStreamResolve, buildStreamRequest(track, quality), &result)
	if err != nil {
		return domain.StreamRef{}, mapStreamResolveError(pluginID(host), err)
	}
	streamURL := strings.TrimSpace(result.URL)
	if streamURL == "" {
		return domain.StreamRef{}, fmt.Errorf("%w: plugin %q returned an empty url for track %s",
			domain.ErrStreamUnavailable, pluginID(host), track.ID)
	}

	ref := domain.StreamRef{
		URL:     streamURL,
		Headers: result.Headers,
	}
	if result.ExpiresAt != "" {
		expires, parseErr := time.Parse(time.RFC3339, result.ExpiresAt)
		if parseErr != nil {
			// Advisory only: a bad hint must not turn a usable URL into a
			// failed download.
			r.logger.Debug("Plugin returned an unparsable stream expiry",
				"plugin", pluginID(host), "expiresAt", result.ExpiresAt, "error", parseErr)
		} else {
			ref.Expires = expires
		}
	}
	return ref, nil
}

// pluginID returns the manifest id of a host, or "" when it has none.
func pluginID(host domain.PluginHost) string {
	if host == nil {
		return ""
	}
	return host.Manifest().ID
}

// buildStreamRequest maps the track onto the frozen stream.resolve params.
func buildStreamRequest(track *domain.TrackDTO, quality string) domain.PluginStreamRequest {
	return domain.PluginStreamRequest{
		Source:  domain.NormalizeProvider(track.Provider),
		ID:      track.ExternalID,
		Quality: quality,
		Metadata: domain.PluginStreamMetadata{
			Title:    track.Title,
			Artists:  trackArtists(track),
			Album:    trackAlbum(track),
			Duration: track.Duration,
		},
	}
}

// trackArtists prefers the populated DTO relation and falls back to the raw
// tag string. A plugin that resolves by id ignores this, but a search-based
// one needs a usable artist list.
func trackArtists(track *domain.TrackDTO) []string {
	artists := make([]string, 0, len(track.Artists))
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			artists = append(artists, artist.Name)
		}
	}
	if len(artists) > 0 {
		return artists
	}
	if raw := strings.TrimSpace(track.RawArtistNames); raw != "" {
		for _, name := range strings.Split(raw, ";") {
			if name = strings.TrimSpace(name); name != "" {
				artists = append(artists, name)
			}
		}
	}
	return artists
}

// trackAlbum returns the album title when the DTO carries the relation.
func trackAlbum(track *domain.TrackDTO) string {
	if track.Album == nil {
		return ""
	}
	return track.Album.Title
}

// mapStreamResolveError turns a failed plugin call into the sentinel the
// download service's retry policy understands.
//
// Mapping is structural: only the JSON-RPC error's `data.kind` counts. The
// message is never parsed for words like "429" or "vip", because wording is
// free text and retry policy must not depend on it (task contract §3.3).
func mapStreamResolveError(pluginID string, err error) error {
	var rpcErr *domain.RPCError
	if errors.As(err, &rpcErr) {
		switch pluginErrorKind(rpcErr) {
		case domain.StreamErrorRateLimited:
			return fmt.Errorf("%w: plugin %q: %s",
				domain.ErrStreamRateLimited, pluginID, rpcErr.Message)
		case domain.StreamErrorUnavailable, domain.StreamErrorVIP:
			return fmt.Errorf("%w: plugin %q: %s",
				domain.ErrStreamUnavailable, pluginID, rpcErr.Message)
		case domain.StreamErrorAuth:
			return fmt.Errorf("%w: plugin %q: %s",
				domain.ErrStreamAuth, pluginID, rpcErr.Message)
		}
	}

	if errors.Is(err, domain.ErrPluginCrashed) {
		// The download service treats an unclassified error as retryable, and
		// the host relaunches the plugin on the next call; pass the crash
		// through with its context intact.
		return err
	}

	if errors.Is(err, domain.ErrPluginMethodNotFound) {
		// The plugin declares the provider but has no stream capability.
		// Retrying cannot grow a method, so fail permanently with an
		// explanation instead of burning the retry budget.
		return fmt.Errorf("%w: plugin %q does not implement %s",
			domain.ErrStreamUnavailable, pluginID, domain.MethodStreamResolve)
	}

	return fmt.Errorf("plugin %q %s: %w", pluginID, domain.MethodStreamResolve, err)
}

// pluginErrorKind decodes the structured kind from an error's data object. A
// missing or malformed object yields "", which keeps the failure retryable.
func pluginErrorKind(rpcErr *domain.RPCError) string {
	if len(rpcErr.Data) == 0 {
		return ""
	}
	var data domain.PluginErrorData
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		return ""
	}
	return domain.NormalizeProvider(data.Kind)
}

// Resolvable is implemented by resolvers that can answer "would this provider
// have an owner?" without performing a download.
//
// It is a separate interface rather than a method on domain.StreamResolver: the
// download pipeline only ever calls Resolve, and widening that port would force
// every implementation (including test fakes) to grow a method it does not need.
// Catalog asks the same question through its own narrow availability port.
type Resolvable interface {
	CanResolve(provider string) bool
}

// CanResolve reports whether some running plugin declared provider.
//
// It is the same lookup Resolve performs, minus the call: the catalog uses it to
// disable a search result whose download nobody could serve, so a source cannot
// look actionable and then fail after the click.
func (r *PluginResolver) CanResolve(provider string) bool {
	if r.registry == nil {
		return false
	}
	provider = domain.NormalizeProvider(provider)
	if provider == "" {
		return false
	}
	_, ok := r.registry.HostForProvider(context.Background(), provider)
	return ok
}

// Compile-time proof that the resolver satisfies the port the download service
// consumes and the availability question catalog asks. main.go binds it onto
// both; this assertion lives here so a port change fails a buildable package.
var (
	_ domain.StreamResolver = (*PluginResolver)(nil)
	_ Resolvable            = (*PluginResolver)(nil)
)
