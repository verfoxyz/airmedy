package download

import (
	"context"
	"log/slog"

	"airmedy/internal/domain"

	"go.uber.org/fx"
)

// Module wires the download pipeline.
//
// The service starts with the application (fx lifecycle) so jobs left queued by
// a crash resume on launch, and stops on shutdown: Stop cancels an in-flight job
// and leaves its partial file and row for the next Start's recovery.
var Module = fx.Module("download",
	fx.Provide(
		// The adapters arrive as domain ports; the service asks for them under
		// its own narrow interfaces, so name them explicitly.
		func(ports Ports) AudioDownloader { return ports.Downloader },
		// The library and playlist services are concrete app types; taking them
		// as interface values keeps finalize's dependencies visible.
		func(ports Ports, lib LibraryImporter, playlists PlaylistAdder, logger *slog.Logger) *Finalizer {
			return NewFinalizer(ports.Tagger, ports.Validator, ports.Artwork, lib, playlists, logger)
		},
		// Lyrics are optional: the adapter is a no-op when the lyrics service is
		// absent, so a deployment without it still downloads audio.
		// The lyric services are concrete app types from other modules; binding
		// them onto these narrow ports here keeps this package free of an
		// app/lyrics or app/appsettings import.
		func(fetcher LyricsFetcher, settings LyricsSettingsReader, logger *slog.Logger) *DownloadLyrics {
			return NewDownloadLyrics(fetcher, settings, logger)
		},
		func(f *Finalizer) LibraryIngest { return f },
		NewMerger,
		NewService,
	),
	// The lyric writer is attached after construction instead of provided twice:
	// *Service already has a provider, and a second one would be an fx conflict.
	// This runs before the lifecycle's OnStart, so no job can observe a service
	// without its writer.
	fx.Invoke(func(svc *Service, lyrics *DownloadLyrics) {
		svc.SetLyricsWriter(lyrics)
	}),
	fx.Invoke(func(svc *Service, remover LocalFileRemover) {
		svc.SetLocalFileRemover(remover)
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *Service, logger *slog.Logger) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				if err := svc.Start(ctx); err != nil {
					// A download queue that cannot start must not take the
					// player down with it.
					logger.Error("Failed to start download service", "error", err)
				}
				return nil
			},
			OnStop: func(ctx context.Context) error {
				if err := svc.Stop(ctx); err != nil {
					logger.Error("Failed to stop download service", "error", err)
				}
				return nil
			},
		})
	}),
)

// BindPorts adapts the concrete adapters supplied in main.go (the HTTP
// downloader, the taglib-backed tagger and the cover fetcher) onto the domain
// ports the pipeline consumes. It lives here rather than in main.go so the
// wiring stays next to the code that depends on it.
func BindPorts(
	dl domain.StreamDownloader,
	validator domain.FileValidator,
	properties domain.AudioPropertiesReader,
	tagger domain.TrackTagger,
	artwork domain.ArtworkFetcher,
) Ports {
	return Ports{
		Downloader: dl,
		Validator:  validator,
		Properties: properties,
		Tagger:     tagger,
		Artwork:    artwork,
	}
}

// Ports bundles the adapter ports so fx can hand them to every provider that
// needs one without a long parameter list per provider.
type Ports struct {
	Downloader domain.StreamDownloader
	Validator  domain.FileValidator
	Properties domain.AudioPropertiesReader
	Tagger     domain.TrackTagger
	Artwork    domain.ArtworkFetcher
}

// Downloader returns the byte mover.
func (p Ports) AudioDownloader() domain.StreamDownloader { return p.Downloader }
