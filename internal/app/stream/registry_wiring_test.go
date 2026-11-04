package stream

import (
	"testing"

	"airmedy/internal/app/plugin"
)

// Compile-time proof of the binding main.go performs: the real plugin registry
// is the provider table PluginResolver routes through. It lives in this test
// because app/stream itself must not depend on app/plugin.
var _ ProviderRegistry = (*plugin.Service)(nil)

func TestPluginServiceSatisfiesProviderRegistry(t *testing.T) {
	// The assertion above is the test; this keeps the file non-empty for
	// tooling that expects at least one test.
	t.Log("plugin.Service implements the ProviderRegistry port")
}
