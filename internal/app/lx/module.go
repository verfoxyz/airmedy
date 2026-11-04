package lx

import "go.uber.org/fx"

// Module wires the lx source list service.
//
// The service provides LaunchEnv; main.go folds it into plugin.Config.LaunchEnv
// so a change in the list reaches the plugin on the next start without the
// registry knowing what the list means. Its validator and registry ports are
// bound in main.go, next to the other infra adapters, because app must not
// import infra.
var Module = fx.Module("lx", fx.Provide(NewService))
