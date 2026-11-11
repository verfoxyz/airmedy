package lastfm

import (
	"context"
	"log/slog"

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
	fx.Invoke(func(lc fx.Lifecycle, s *LastFmService, logger *slog.Logger) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				logger.Info("Last.fm service started")
				return nil
			},
		})
	}),
)
