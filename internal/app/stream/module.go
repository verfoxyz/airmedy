package stream

import (
	"airmedy/internal/domain"

	"go.uber.org/fx"
)

// Module wires plugin-backed stream resolution.
//
// A track's provider is owned by the one enabled plugin that declared it under
// "providers"; NewPluginResolver looks that owner up through the registry port
// and is the single domain.StreamResolver the download service receives.
var Module = fx.Module("stream",
	fx.Provide(
		NewPluginResolver,
		func(resolver *PluginResolver) domain.StreamResolver { return resolver },
	),
)
