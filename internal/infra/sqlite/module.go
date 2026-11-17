package sqlite

import (
	"airmedy/internal/domain"

	"go.uber.org/fx"
)

var Module = fx.Module("sqlite",
	fx.Provide(
		func(db *DB) domain.TrackRepository { return NewTrackRepository(db) },
		func(db *DB) domain.TrackArtworkStore { return newTrackRepository(db) },
		func(db *DB, tracks domain.TrackRepository) domain.TrackQueryRepository {
			return NewTrackQueryRepository(db, tracks)
		},
		func(db *DB) domain.AlbumRepository { return NewAlbumRepository(db) },
		func(db *DB) domain.ArtistRepository { return NewArtistRepository(db) },
		func(db *DB) domain.ArtistAliasRepository { return NewArtistAliasRepository(db) },
		func(db *DB) domain.GenreRepository { return NewGenreRepository(db) },
		func(db *DB) domain.ComposerRepository { return NewComposerRepository(db) },
		func(db *DB) domain.PlaylistRepository { return NewPlaylistRepository(db) },
		func(db *DB) domain.LyricRepository { return NewLyricRepository(db) },
		func(db *DB) domain.MobileSyncLyricCacheRepository { return NewMobileSyncLyricCacheRepository(db) },
		func(db *DB) domain.WatchedFolderRepository { return NewWatchedFolderRepository(db) },
		func(db *DB) domain.EQRepository { return NewEQRepository(db) },
		func(db *DB) domain.PlayerStateRepository { return NewPlayerStateRepository(db) },
		func(db *DB) domain.SettingsRepository { return NewSettingsRepository(db) },
		func(db *DB) domain.AnalysisRepository { return NewAnalysisRepository(db) },
		func(db *DB) domain.MiniPlayerStateRepository { return NewMiniPlayerStateRepository(db) },
		func(db *DB) domain.LibrarySyncStateRepository { return NewLibrarySyncStateRepository(db) },
		func(db *DB) domain.ListeningRepository { return NewListeningRepository(db) },
		func(db *DB) domain.PairingIdentityRepository { return NewPairingIdentityRepository(db) },
		func(db *DB) domain.TrustedMobileDeviceRepository { return NewTrustedMobileDeviceRepository(db) },
		func(db *DB) domain.MobileLibrarySyncPlanRepository { return NewMobileLibrarySyncPlanRepository(db) },
		func(db *DB) domain.PlaylistMutationLedger { return NewPlaylistMutationLedger(db) },
		func(db *DB) domain.PlaylistMutationLWW { return NewPlaylistMutationLWW(db) },
		func(db *DB) domain.FavoriteMutationLedger { return NewFavoriteMutationLedger(db) },
		func(db *DB) domain.FavoriteMutationLWW { return NewFavoriteMutationLWW(db) },
		func(db *DB) domain.PlaylistArtworkStagingRepository { return NewPlaylistArtworkStagingRepository(db) },
		func(db *DB) domain.DownloadJobRepository { return NewDownloadJobRepository(db) },
		func(db *DB) domain.StreamCacheRepository { return NewStreamCacheRepository(db) },
		func(db *DB) domain.PendingScrobbleRepository { return NewLastFmPendingScrobbleRepository(db) },
		func(db *DB) domain.LastFmRecommendationRepository {
			return NewLastFmRecommendationRepository(db)
		},
		func(db *DB) domain.LastFmMatchCacheRepository {
			return NewLastFmMatchCacheRepository(db)
		},
		func(db *DB) domain.LastFmDislikeRepository {
			return NewLastFmDislikeRepository(db)
		},
		func(db *DB) domain.RecommendationSeedRepository {
			return NewRecommendationSeedRepository(db)
		},
		func(db *DB) domain.StreamCacheTaskRepository { return NewStreamCacheTaskRepository(db) },
		func(db *DB) domain.MediaRequestRepository { return NewMediaRequestRepository(db) },
		func(db *DB) domain.PluginSettingsRepository { return NewPluginSettingsRepository(db) },
		func(db *DB) domain.CatalogSourceOrderRepository {
			return NewCatalogSourceOrderRepository(db)
		},
		func(db *DB) domain.PluginKVRepository { return NewPluginKVRepository(db) },
		func(db *DB) domain.LxSourceRepository { return NewLxSourceRepository(db) },
		func(db *DB, tracks domain.TrackRepository) domain.DownloadMergeRepository {
			return NewDownloadMergeRepository(db, tracks)
		},
		func(db *DB) domain.TxManager { return NewTxManager(db) },
	),
)
