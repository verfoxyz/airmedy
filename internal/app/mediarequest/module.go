package mediarequest

import (
	"context"
	"log/slog"

	"go.uber.org/fx"
)

// Module runs the media request worker and the save adapter every entry point
// shares.
//
// Every save entry point only upserts a request, and the worker advances it
// through the download service's executor (promoting a cached copy in place or
// queueing a download). The old promotion table and stream-cache flags are
// migrated into media_requests by SQL, not by a Go backfill.
var Module = fx.Module("mediarequest",
	fx.Provide(NewService, NewWorker, NewSaver),
	fx.Invoke(func(lc fx.Lifecycle, worker *Worker, logger *slog.Logger) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				if err := worker.Start(ctx); err != nil {
					// A mirror that cannot start must not keep the app from opening.
					logger.Error("Failed to start the media request worker", "error", err)
				}
				return nil
			},
			OnStop: func(ctx context.Context) error {
				if err := worker.Stop(ctx); err != nil {
					logger.Error("Failed to stop the media request worker", "error", err)
				}
				return nil
			},
		})
	}),
)
