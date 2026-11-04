package plugin

import (
	"context"
	"log/slog"

	"airmedy/internal/domain"

	"go.uber.org/fx"
)

// Module wires the plugin registry.
//
// Discovery runs on application start, followed by the automatic start of every
// plugin the user enabled. Neither step is fatal: no plugins installed, one
// broken manifest in a user directory, or a plugin that fails its handshake must
// not stop the player from opening. Each failure is recorded on the registry
// entry so the settings page can show it, and the rest of the plugins still run.
//
// Running plugin processes are closed on shutdown through the same lifecycle.
var Module = fx.Module("plugin",
	fx.Provide(
		NewService,
		// Host-owned values for the switches a plugin declares through describe.
		func(kv domain.PluginKVRepository) *SettingStore { return NewSettingStore(kv) },
		// The plugin-backed lyric route consumes the same registry the stream
		// router uses; app/lyrics sees only the port.
		func(service *Service, logger *slog.Logger) domain.PluginLyricFetcher {
			return NewLyricsAdapter(service, logger)
		},
	),
	fx.Invoke(func(lifecycle fx.Lifecycle, service *Service, logger *slog.Logger) {
		lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				if _, err := service.Scan(ctx); err != nil {
					logger.Warn("Failed to scan plugins", "error", err)
				}
				// Autostart the plugins the user enabled. StartEnabled applies
				// the running cap and records per-plugin failures instead of
				// aborting the rest.
				if err := service.StartEnabled(ctx); err != nil {
					logger.Warn("Some enabled plugins failed to start", "error", err)
				}
				return nil
			},
			OnStop: func(ctx context.Context) error {
				if err := service.Close(ctx); err != nil {
					logger.Warn("Failed to stop plugin service", "error", err)
				}
				return nil
			},
		})
	}),
)
