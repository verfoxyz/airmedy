package lastfm

import (
	"context"
	"log/slog"

	"airmedy/internal/app/catalog"
	"airmedy/internal/app/player"
	"airmedy/internal/domain"

	"go.uber.org/fx"
)

var Module = fx.Module("lastfm",
	fx.Provide(
		NewLastFmService,
		// The recommender consumes the read-only surface through its own port so
		// the aggregation rules are testable with a fake client.
		func(s *LastFmService) TrackSource { return s },
		NewRecommender,
		// Radio drives the player queue and registers matched rows as streamable
		// tracks; both surfaces stay behind the narrow ports in radio.go.
		func(queue *player.PlayerService) RadioQueue { return queue },
		func(svc *catalog.Service) StreamableResolver { return svc },
		NewRadio,
	),
	fx.Invoke(func(radio *Radio, queue RadioQueue) {
		radio.WireListeners(queue)
	}),
	// A remote row that cannot be loaded is a broken source, not a broken song:
	// the recommendation page retries the same identity against another provider.
	fx.Invoke(func(playerSvc *player.PlayerService, recommender *Recommender) {
		playerSvc.AddPlaybackFailureListener(func(track *domain.TrackDTO, _ error) {
			if track == nil {
				return
			}
			recommender.HandlePlaybackFailure(track.Provider, track.ExternalID)
		})
	}),
	// A matched row whose source reports no artwork falls back to the cached
	// cover for its identity, so the page can show a real image or a placeholder
	// instead of a broken URL.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, assets domain.CoverIdentityAssets) {
			recommender.SetCoverAssets(assets)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// The two recommendation lists survive a restart; without the store the
	// recommender still builds them in memory.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, store domain.LastFmRecommendationRepository) {
			recommender.SetRecommendationStore(store)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// Local listening signals rank the seeds for an imported library that has
	// no scrobbles. Optional: without the store the seeds are Last.fm-only.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, seeds domain.RecommendationSeedRepository) {
			recommender.SetSeedRepository(seeds)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// The shared match cache outlives one list and one session, so the same
	// identity is matched once. Optional: without it the in-memory caches still
	// apply, they just do not survive a restart.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, cache domain.LastFmMatchCacheRepository) {
			recommender.SetMatchCache(cache)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// Dislikes are local-only: they hide a song or artist from discovery and
	// radio without touching Last.fm or the library. Optional: without the store
	// nothing is excluded.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, dislikes domain.LastFmDislikeRepository) {
			recommender.SetDislikeStore(dislikes)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// A dislike also drops the recommendation rows, candidate entries and match
	// cache of the identity. Optional: without the purger the dislike still
	// filters future rows.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, purger domain.LastFmDislikeTracePurger) {
			recommender.SetDislikeTracePurger(purger)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// With the purge setting on, a dislike also deletes the local audio. The
	// library owns that path; the port keeps app/lastfm out of it.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, purger DislikeLocalPurger) {
			recommender.SetDislikeLocalPurger(purger)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// The purge toggle lives in app settings. Optional: without it purging is
	// off.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, settings DislikeSettingsReader) {
			recommender.SetDislikeSettings(settings)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	// A merge moves the remote identity onto the local survivor, so the
	// recommender needs the survivor row to refresh rows that still say "from
	// <provider>". Optional: without it the lists refresh on their next load.
	fx.Invoke(fx.Annotate(
		func(recommender *Recommender, tracks domain.TrackRepository) {
			recommender.SetTrackLookup(tracks)
		},
		fx.ParamTags(``, `optional:"true"`),
	)),
	fx.Invoke(func(lc fx.Lifecycle, s *LastFmService, recommender *Recommender, logger *slog.Logger) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				// Drop expired and overflow matches once per session; the write
				// path also prunes, so a running app never grows unbounded.
				recommender.PruneMatchCache(ctx)
				// Retry scrobbles from previous sessions before new playback starts.
				s.StartScrobbleQueue()
				logger.Info("Last.fm service started")
				return nil
			},
			OnStop: func(ctx context.Context) error {
				s.StopScrobbleQueue()
				return nil
			},
		})
	}),
)
