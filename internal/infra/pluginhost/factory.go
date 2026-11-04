package pluginhost

import (
	"context"
	"log/slog"

	"airmedy/internal/domain"
)

// Factory is the host-side half of the domain.PluginHostFactory port: main.go
// binds it once and the app registry calls it per Start. Options given here apply
// to every plugin the app runs (logging, timings, locale, the plugin → host
// request handler); per-plugin overrides are not a v1 feature.
//
// The request handler is shared because it dispatches by plugin id itself: the
// transport passes the id of the plugin it is talking to, so one handler serves
// every process without leaking one plugin's keys to another.
type Factory struct {
	logger  *slog.Logger
	options []Option
}

// NewFactory returns a factory that launches plugin hosts with the given options.
// The logger is always attached, so a plugin's stderr and host events land in the
// application log with the plugin id as a field.
func NewFactory(logger *slog.Logger, options ...Option) *Factory {
	if logger == nil {
		logger = slog.Default()
	}
	return &Factory{logger: logger, options: options}
}

// Start implements domain.PluginHostFactory.
func (f *Factory) Start(ctx context.Context, manifest domain.PluginManifest) (domain.PluginHost, error) {
	options := make([]Option, 0, len(f.options)+1)
	options = append(options, WithLogger(f.logger))
	options = append(options, f.options...)
	host, err := StartManifest(ctx, manifest, options...)
	if err != nil {
		return nil, err
	}
	return host, nil
}

// Compile-time proof that this package implements the domain ports the app layer
// consumes. If a port changes, the build fails here instead of at the wiring in
// main.go, which cannot be built on a machine without the audio C libraries.
var (
	_ domain.PluginLoader      = (*Loader)(nil)
	_ domain.PluginHost        = (*Host)(nil)
	_ domain.PluginHostFactory = (*Factory)(nil).Start
)
