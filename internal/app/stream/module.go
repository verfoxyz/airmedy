package stream

import (
	"context"
	"log/slog"

	"airmedy/internal/domain"

	"go.uber.org/fx"
)

// Module wires plugin-backed stream resolution and the local playback cache.
//
// A track's provider is owned by the one enabled plugin that declared it under
// "providers"; NewPluginResolver looks that owner up through the registry port
// and is the single domain.StreamResolver the download service receives.
// NewCache is the local proxy FFmpeg opens while a remote track plays; it
// resolves through that same resolver, so it never talks to a provider itself.
var Module = fx.Module("stream",
	fx.Provide(
		NewPluginResolver,
		func(resolver *PluginResolver) domain.StreamResolver { return resolver },
		NewCache,
	),
	// Artist aliases let cross-source matching reuse a cached stream under the
	// other spelling of the same artist. Optional for graphs without the store.
	fx.Invoke(fx.Annotate(
		func(cache *Cache, aliases domain.ArtistAliasRepository) {
			cache.SetArtistAliases(aliases)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	fx.Invoke(func(lifecycle fx.Lifecycle, cache *Cache, logger *slog.Logger) {
		lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				if err := cache.Start(ctx); err != nil {
					// A dead proxy must not abort app startup: local playback is
					// unaffected, and the error is surfaced when a stream opens.
					logger.Warn("Stream cache unavailable", "error", err)
				}
				return nil
			},
			OnStop: func(ctx context.Context) error {
				return cache.Stop(ctx)
			},
		})
	}),
)
