// Package pluginhost launches Airmedy plugins and speaks the v1 plugin protocol
// with them.
//
// A plugin is a child process declared by an airmedy-plugin.json manifest. The
// host talks to it over stdin/stdout, one JSON-RPC 2.0 object per line, and
// treats stderr as the plugin's log stream. Loading runs a handshake:
//
//	host → initialize {hostVersion, apiVersion, locale}
//	plugin → {apiVersion, capabilities}
//
// The concrete API version the plugin answers with must satisfy the range in its
// own manifest and stay inside the host's major/minor promise (1.x is additive
// only). A mismatch is a hard failure: the process is reclaimed instead of
// running under a contract it does not honour.
//
// Lifecycle rules worth remembering:
//
//   - Close escalates shutdown → stdin EOF → terminate → kill, never waiting
//     longer than the configured grace per step.
//   - A crash fails in-flight calls with -32000. The host does not relaunch
//     behind the caller's back: the next Call (or Start) relaunches, up to
//     MaxRestarts times; after that the plugin is ErrPluginUnavailable.
//   - Unknown capabilities and unknown notifications are tolerated and logged;
//     the ecosystem contains forks with extra sources, and rejecting a manifest
//     over one unfamiliar string would lock out working plugins.
//
// infra/pluginhost implements the domain ports PluginLoader, PluginHost and
// PluginHostFactory. The app registry (internal/app/plugin) consumes only the
// ports, so no app package imports this one.
package pluginhost

import "airmedy/internal/domain"

// Compile-time proof that the adapters still satisfy the domain ports. main.go
// binds these onto the ports, and it cannot be compiled on a machine without the
// audio toolchain, so the assertion belongs here where it always runs.
var (
	_ domain.PluginHost        = (*Host)(nil)
	_ domain.PluginLoader      = (*Loader)(nil)
	_ domain.PluginHostFactory = (&Factory{}).Start
)
