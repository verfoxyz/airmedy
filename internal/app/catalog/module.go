package catalog

import (
	"log/slog"

	"airmedy/internal/domain"

	"go.uber.org/fx"
)

// Module wires online catalogue search.
//
// The plugin registry arrives from app/plugin and the stream resolver from
// app/stream; both are consumed through the narrow ports this package declares,
// so app/catalog never imports either one.
//
// The resolver port is bound in main.go from *stream.PluginResolver, because
// catalog only needs the availability question and must not import the stream
// package itself.
var Module = fx.Module("catalog",
	fx.Provide(
		NewPluginSources,
		fx.Annotate(
			func(sources *PluginSources, settings PluginSettingReader, logger *slog.Logger) *PluginSearcher {
				return NewPluginSearcher(sources, settings, logger)
			},
			// The setting store lives in plugin.Module; a graph without plugins
			// keeps the default (covers requested).
			fx.ParamTags(``, `optional:"true"`, ``),
		),
		// The two ports the service consumes, bound from the adapters above.
		func(sources *PluginSources) domain.CatalogSearchSourceRegistry { return sources },
		func(searcher *PluginSearcher) domain.CatalogSearcher { return searcher },
		func(
			sources *PluginSources,
			searcher domain.CatalogSearcher,
			resolver ResolverAvailability,
			states domain.CatalogRowStateResolver,
			library RemoteTracks,
			localTracks LocalTracks,
			favorites FavoritesWriter,
			playlists PlaylistMembership,
			settings SettingsReader,
			logger *slog.Logger,
		) *Service {
			return NewService(Deps{
				Sources:     sources,
				Searcher:    searcher,
				Resolver:    resolver,
				States:      states,
				Library:     library,
				LocalTracks: localTracks,
				Favorites:   favorites,
				Playlists:   playlists,
				Settings:    settings,
				Logger:      logger,
			})
		},
	),
)
