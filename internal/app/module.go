package app

import (
	"airmedy/internal/app/analysis"
	"airmedy/internal/app/analytics"
	"airmedy/internal/app/appsettings"
	"airmedy/internal/app/catalog"
	"airmedy/internal/app/config"
	"airmedy/internal/app/download"
	"airmedy/internal/app/eq"
	"airmedy/internal/app/i18n"
	"airmedy/internal/app/lastfm"
	"airmedy/internal/app/library"
	"airmedy/internal/app/lyrics"
	"airmedy/internal/app/mobilesync"
	"airmedy/internal/app/moodradio"
	"airmedy/internal/app/normalization"
	"airmedy/internal/app/pairing"
	"airmedy/internal/app/player"
	"airmedy/internal/app/playlist"
	"airmedy/internal/app/remoteserver"
	"airmedy/internal/app/romanization"
	"airmedy/internal/app/stream"
	"airmedy/internal/app/updater"
	"airmedy/internal/domain"
	"airmedy/internal/infra/artwork"
	"airmedy/internal/infra/audio"
	"airmedy/internal/infra/bleve"
	"airmedy/internal/infra/covercache"
	keyringinfra "airmedy/internal/infra/keyring"
	"airmedy/internal/infra/logging"
	lyricsinfra "airmedy/internal/infra/lyrics"
	mdnsinfra "airmedy/internal/infra/mdns"
	"airmedy/internal/infra/metadata"
	mqttinfra "airmedy/internal/infra/mqtt"
	"airmedy/internal/infra/notification"
	"airmedy/internal/infra/power"
	romanizationinfra "airmedy/internal/infra/romanization"
	"airmedy/internal/infra/sqlite"
	"airmedy/internal/infra/wails"
	"context"
	"log/slog"

	"go.uber.org/fx"

	"github.com/wailsapp/wails/v3/pkg/application"
)

var Module = fx.Module("app",
	fx.Provide(
		config.NewConfig,
		i18n.NewService,
		func(lc fx.Lifecycle, c *config.Config, logger *slog.Logger) (*sqlite.DB, error) {
			db, err := sqlite.NewDB(c.DBPath(), logger)
			if err != nil {
				return nil, err
			}
			lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					return db.Close()
				},
			})
			return db, nil
		},
		func(lc fx.Lifecycle, c *config.Config) (domain.SearchService, error) {
			search, err := bleve.NewBleveSearchService(c.IndexPath())
			if err != nil {
				return nil, err
			}
			lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					return search.Close()
				},
			})
			return search, nil
		},
		func(c *config.Config) (domain.ArtworkCache, error) {
			return artwork.NewDiskArtworkCache(c.ArtworkCachePath())
		},
		func(db *sqlite.DB, c *config.Config, logger *slog.Logger) (*covercache.Cache, error) {
			return covercache.New(covercache.Config{
				Dir:    c.CoverCachePath(),
				DB:     db.DB.DB,
				Logger: logger,
			})
		},
		func(cache *covercache.Cache) domain.CatalogCoverCache { return cache },
		func(cache *covercache.Cache) domain.CoverIdentityAssets { return cache },
		func(cache *covercache.Cache) domain.CoverPinner { return cache },
		func(cache *covercache.Cache) player.StreamCoverProvider { return cache },
		func() domain.MetadataExtractor { return metadata.NewTagLibExtractor() },
		func() domain.MetadataWriter { return metadata.NewTagLibWriter() },
		audio.NewPlayer,
		func() domain.LoudnessAnalyzer { return audio.NewLoudnessAnalyzer() },
		func() domain.MediaDurationProbe { return audio.NewDurationProbe() },
		library.NewLibraryService,
		wails.NewLibraryService,
		wails.NewPlayerService,
		wails.NewSearchService,
		wails.NewPlaylistService,
		wails.NewLyricsService,
		func(lc fx.Lifecycle) *romanization.Service {
			service := romanization.New(romanizationinfra.New())
			lc.Append(fx.Hook{OnStop: service.Close})
			return service
		},
		wails.NewEQService,
		wails.NewNormalizationService,
		wails.NewAnalysisService,
		wails.NewAnalyticsService,
		wails.NewLastFmService,
		wails.NewWindowService,
		wails.NewSettingsService,
		wails.NewRemoteServerService,
		wails.NewMobilePairingService,
		wails.NewMobileLibrarySyncService,
		wails.NewUpdaterService,
		wails.NewMoodRadioService,
		wails.NewDownloadService,
		wails.NewPluginService,
		wails.NewLxSourcesService,
		wails.NewCatalogService,
		func(logger *slog.Logger) *updater.Service {
			return updater.NewService(config.Version, logger)
		},
		func() *wails.GreetService { return &wails.GreetService{} },
		func() domain.PairingKeyStore { return keyringinfra.NewPairingKeyStore() },
		// The download pipeline consumes ports; these adapters are the app-layer
		// services behind them. download.Module then builds the service.
		func(lib *library.LibraryService) download.LibraryImporter { return lib },
		func(pl *playlist.PlaylistService) download.PlaylistAdder { return pl },
		func(st *appsettings.SettingsService) download.SettingsReader { return st },
		// The search page's row badges ("in library", "downloading", "failed")
		// come from the download service: it owns both the remote identity
		// lookup and the queue. catalog.Module consumes this port and never
		// imports app/download.
		func(svc *download.Service) domain.CatalogRowStateResolver { return svc },
		// The search row's "save" action registers and queues through the
		// download service, flags favorites through the library, and adds
		// playlist membership through the playlist service. Each is a separate
		// narrow port so app/catalog imports none of them.
		func(svc *download.Service) catalog.RemoteTracks { return svc },
		func(svc *download.Service) catalog.LocalTracks { return svc },
		// Saving a row is decided in one place: the download coordinator promotes
		// a cached stream copy or queues a download.
		func(svc *download.Service) catalog.SavedTracks { return svc },
		func(cache *stream.Cache) download.CachedAudio { return cache },
		func(lib *library.LibraryService) download.LocalFileRemover { return lib },
		func(st *appsettings.SettingsService) catalog.SettingsReader { return st },
		func(lib *library.LibraryService) catalog.FavoritesWriter { return lib },
		func(pl *playlist.PlaylistService) catalog.PlaylistMembership { return pl },
		func(logger *slog.Logger) domain.PairingBroker { return mqttinfra.NewPairingBroker(logger) },
		func(logger *slog.Logger) domain.PairingAdvertiser { return mdnsinfra.NewPairingAdvertiser(logger) },
	),
	sqlite.Module,
	logging.Module,
	lyricsinfra.Module,
	player.Module,
	power.Module,
	notification.Module,
	playlist.Module,
	lyrics.Module,
	eq.Module,
	normalization.Module,
	moodradio.Module,
	lastfm.Module,
	appsettings.Module,
	remoteserver.Module,
	pairing.Module,
	mobilesync.Module,
	analysis.Module,
	analytics.Module,
	download.Module,
	catalog.Module,
	// Plugin lyrics are optional: plugin.Module is a sibling module and may be
	// absent in tests. When present, the adapter gets the first lyric attempt
	// ahead of the built-in providers.
	fx.Invoke(fx.Annotate(
		func(ls *lyrics.LyricsService, fetcher domain.PluginLyricFetcher) {
			ls.SetPluginLyricsFetcher(fetcher)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	fx.Invoke(func(lc fx.Lifecycle, db *sqlite.DB, search domain.SearchService, lib *library.LibraryService, playerSvc *player.PlayerService, eqSvc *eq.EQService, lastfmSvc *lastfm.LastFmService, analysisSvc *analysis.AnalysisService, settingsSvc *appsettings.SettingsService, playlistSvc *playlist.PlaylistService, pairingSvc *pairing.Service, mobileSyncSvc *mobilesync.Service, downloadSvc *download.Service, cache *stream.Cache, logger *slog.Logger) {
		// Deleting a streamed row must also drop its cached audio and release the
		// decoder that holds the loopback URL. Wired here so app/library imports
		// neither app/stream nor app/player.
		lib.SetStreamCacheDiscarder(cache)
		lib.SetPlaybackRemover(playerSvc)
		// Promotion must not move a file the decoder still points at, and a
		// promoted row must not be streamed again. Both are narrow ports so
		// neither stream nor download imports app/player.
		downloadSvc.SetPlayingTrack(playerSvc)
		downloadSvc.SetCacheCleanupPlayer(playerSvc)
		cache.SetLocalIdentityChecker(downloadSvc)
		// Promotion moves the cache file, so it waits for playback to release it;
		// a closing session is the fastest signal, the worker's timer is the
		// backstop. Wire direction: stream notifies download, never the reverse.
		cache.OnSessionClosed(func(_, _ string) {
			downloadSvc.WakeCacheMaintenance()
		})
		var offTrackMerged func()
		pairingSvc.AddDeviceConnectionListener(func(deviceID string, connected bool) {
			if connected {
				return
			}
			if err := mobileSyncSvc.CancelIfActive(context.Background(), deviceID); err != nil {
				logger.Error("cancel mobile library sync for offline device", "device_id", deviceID, "error", err)
			}
		})
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				// A merge deletes the remote row. The player may still have it
				// loaded (the download case, or the promotion that runs after the
				// session closed), so it must remap before the next write or load.
				if app := application.Get(); app != nil && app.Event != nil {
					offTrackMerged = app.Event.On("track:merged", func(event *application.CustomEvent) {
						payload, ok := event.Data.(map[string]string)
						if !ok {
							return
						}
						playerSvc.RemapTrack(context.Background(), payload["from_track_id"], payload["to_track_id"])
					})
				}
				// Wire metadata changes to player state; favorites use their own listener.
				lib.AddTrackUpdateListener(func(track *domain.TrackDTO) {
					playerSvc.SyncTrack(track)
				})

				// Last.fm love/unlove only on a genuine favorite toggle — not on
				// every track update. AddTrackUpdateListener also fires on import
				// and metadata edits, which would spam track.unlove for every
				// freshly imported non-favorite track.
				lib.AddFavoriteChangeListener(func(track *domain.TrackDTO) {
					playerSvc.SyncFavorite(track)
					lastfmSvc.SetLoveStatus(track, track.IsFavorite)
					// A saved remote track becomes permanent: complete the stream
					// cache and promote it (or re-download at a higher quality).
					if track.IsFavorite {
						if _, err := downloadSvc.OnRemoteTrackSaved(context.Background(), domain.RemoteTrackSaved{
							Track:    track,
							Explicit: true,
						}); err != nil {
							slog.Warn("promote saved favorite", "track_id", track.ID, "error", err)
						}
					}
				})

				// Wire import -> analysis enqueue (low priority) and playback state ->
				// analysis throttle, both centrally here so neither package imports
				// the other (avoids a library<->analysis or player<->analysis cycle;
				// mirrors the track-update wiring above).
				lib.AddAnalysisListener(func(trackID string) {
					analysisSvc.Enqueue(trackID, false)
				})
				lib.AddSyncFinishedListener(func() {
					analysisSvc.TriggerPercentileRecompute()
				})
				lib.AddTrackDeletedListener(func(trackIDs []string) {
					analysisSvc.Dequeue(trackIDs)
					analysisSvc.NotifyTracksDeleted(trackIDs)
				})
				playerSvc.AddStatusListener(func(status domain.PlayerStatus) {
					analysisSvc.SetThrottled(status.PlaybackState == domain.PlaybackStatePlaying)
				})
				playerSvc.AddTrackLoadListener(func(track *domain.TrackDTO) {
					analysisSvc.Enqueue(track.ID, true) // on-play priority boost
				})

				// Reschedule the library's periodic sync when the interval
				// setting changes (ignore saves that don't touch it, so an
				// unrelated settings edit doesn't force an immediate rescan).
				lastSyncInterval := lib.CurrentSyncInterval()
				settingsSvc.AddChangeListener(func(settings *domain.AppSettings) {
					if settings.LibrarySyncInterval != lastSyncInterval {
						lastSyncInterval = settings.LibrarySyncInterval
						lib.RescheduleSync()
					}
				})

				// Apply the queue size cap live so a lowered limit trims the
				// running queue immediately (in addition to the startup load
				// in PlayerService.restoreState).
				settingsSvc.AddChangeListener(func(settings *domain.AppSettings) {
					playerSvc.SetMaxQueueSize(domain.ResolveMaxQueueSize(settings.MaxQueueSize))
				})

				// Apply crossfade duration changes live (initial value is loaded
				// in PlayerService.restoreState).
				settingsSvc.AddChangeListener(func(settings *domain.AppSettings) {
					playerSvc.SetCrossfadeSeconds(domain.ClampCrossfadeSeconds(settings.CrossfadeSeconds))
				})

				// Apply macOS automatic-track notification preference live.
				settingsSvc.AddChangeListener(func(settings *domain.AppSettings) {
					playerSvc.SetAutoAdvanceNotificationsEnabled(settings.AutoAdvanceNotificationsEnabled)
				})

				// Adding an online track to a playlist is one save event: the
				// coordinator promotes a cached copy, otherwise the auto-download
				// setting decides. Wired here so the playlist package never imports
				// download.
				playlistSvc.SetAutoDownload(func(ctx context.Context, track *domain.TrackDTO, playlistID string) {
					if _, err := downloadSvc.OnRemoteTrackSaved(ctx, domain.RemoteTrackSaved{
						Track:      track,
						PlaylistID: playlistID,
					}); err != nil {
						slog.Warn("auto-download decision failed", "track_id", track.ID, "error", err)
					}
				})

				if err := playlistSvc.EnsureFavoritesPlaylist(ctx); err != nil {
					slog.Error("Failed to ensure favorites playlist", "error", err)
				}
				if err := eqSvc.SeedDefaults(ctx); err != nil {
					slog.Error("Failed to seed EQ defaults", "error", err)
				}
				if err := eqSvc.ApplyActiveProfile(ctx); err != nil {
					slog.Error("Failed to apply active EQ profile", "error", err)
				}
				if err := analysisSvc.Start(ctx); err != nil {
					slog.Error("Failed to start analysis service", "error", err)
				}
				return lib.Start(ctx)
			},
			OnStop: func(ctx context.Context) error {
				if offTrackMerged != nil {
					offTrackMerged()
					offTrackMerged = nil
				}
				if err := analysisSvc.Stop(ctx); err != nil {
					slog.Error("Failed to stop analysis service", "error", err)
				}
				if err := lib.Stop(ctx); err != nil {
					slog.Error("Failed to stop library service", "error", err)
				}
				return nil
			},
		})
	}),
)
