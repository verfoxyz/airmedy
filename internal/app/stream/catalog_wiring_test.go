package stream

import (
	"testing"

	"airmedy/internal/app/catalog"
	"airmedy/internal/app/plugin"
)

// Compile-time proofs of the bindings main.go performs. They live in a stream
// test because neither app package may import the other in production code.

// The catalog service asks the resolver whether a provider can be downloaded at
// all; the resolver is the only type that knows which providers plugins declared.
var _ catalog.ResolverAvailability = (*PluginResolver)(nil)

// The plugin registry is the source table catalog search renders buttons from,
// and the thing it calls to search one source.
var _ catalog.PluginRegistry = (*plugin.Service)(nil)

func TestCatalogWiringContracts(t *testing.T) {
	// The assertions above are the test; these calls keep the file honest about
	// what the two ports are used for.
	resolver := NewPluginResolver(nil, testLogger())
	if resolver.CanResolve("wy") {
		t.Fatal("a resolver with no plugin registry must not claim it can resolve wy")
	}
	if resolver.CanResolve("") {
		t.Fatal("an empty provider must never be resolvable")
	}

	t.Log("PluginResolver implements catalog.ResolverAvailability; plugin.Service implements catalog.PluginRegistry")
}
