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
		fx.Annotate(
			func(sources *PluginSources, settings PluginSettingReader) *SourceSettingReader {
				return NewSourceSettingReader(sources, settings)
			},
			// Same optional store; without it the multi-source cap falls back to
			// one page instead of failing the search.
			fx.ParamTags(``, `optional:"true"`),
		),
		// The three ports the service consumes, bound from the adapters above.
		func(sources *PluginSources) domain.CatalogSearchSourceRegistry { return sources },
		func(searcher *PluginSearcher) domain.CatalogSearcher { return searcher },
		func(reader *SourceSettingReader) domain.CatalogSourceSettingReader { return reader },
		func(
			sources *PluginSources,
			searcher domain.CatalogSearcher,
			resolver ResolverAvailability,
			states domain.CatalogRowStateResolver,
			library RemoteTracks,
			localTracks LocalTracks,
			localMatcher LocalTrackMatcher,
			favorites FavoritesWriter,
			playlists PlaylistMembership,
			settings SettingsReader,
			sourceSettings domain.CatalogSourceSettingReader,
			covers domain.CatalogCoverCache,
			coverAssets domain.CoverIdentityAssets,
			logger *slog.Logger,
		) *Service {
			return NewService(Deps{
				Sources:        sources,
				Searcher:       searcher,
				Resolver:       resolver,
				States:         states,
				Library:        library,
				LocalTracks:    localTracks,
				LocalMatcher:   localMatcher,
				Favorites:      favorites,
				Playlists:      playlists,
				Settings:       settings,
				SourceSettings: sourceSettings,
				Covers:         covers,
				CoverAssets:    coverAssets,
				Logger:         logger,
			})
		},
	),
	// Alias expansion is optional: without the store matching still works with the
	// literal artist sets.
	fx.Invoke(fx.Annotate(
		func(svc *Service, aliases domain.ArtistAliasRepository) {
			svc.SetArtistAliases(aliases)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
)
