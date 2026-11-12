package lastfm

import (
	"context"
	"log/slog"

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
	),
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
	fx.Invoke(func(lc fx.Lifecycle, s *LastFmService, logger *slog.Logger) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
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
