package player

import (
	"airmedy/internal/app/stream"

	"go.uber.org/fx"
)

// Module provides the player-related services to the application.
var Module = fx.Module("player",
	fx.Provide(
		NewQueueService,
		func(cache *stream.Cache) StreamCache { return cache },
		NewPlayerService,
	),
)
