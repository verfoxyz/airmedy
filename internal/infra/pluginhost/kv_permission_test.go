package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"airmedy/internal/domain"
)

// These tests are the end-to-end half of `permissions.kv`: a real child process
// asks the host for storage and the answer depends on whether its manifest
// declared the permission. app/pluginkv covers the handler in isolation; this
// covers the path from a manifest on disk to a decision on the wire.

// gate builds the handler an application binds: it applies the manifest's decision
// the way app/pluginkv's permission table does, and serves storage otherwise.
func gate(declared bool, repo map[string]string) domain.PluginRequestHandler {
	return func(_ context.Context, pluginID, method string, params json.RawMessage) (any, error) {
		if !declared {
			return nil, domain.ErrPluginKVPermission
		}
		if method == domain.MethodStorageSet {
			return struct{}{}, nil
		}
		return domain.PluginStorageGetResult{Key: "token", Value: `{"v":1}`, Found: true}, nil
	}
}

// TestKVIsServedOnlyWhenTheManifestDeclaresIt is the whole permission contract
// in one test: the same helper binary is served with the declaration and refused
// (-32003) without it.
//
// The helper's kv-probe mode asks the host for a value mid-call and reports the
// refusal back through its own answer, so this asserts what a plugin would actually
// observe rather than what the host recorded.
func TestKVIsServedOnlyWhenTheManifestDeclaresIt(t *testing.T) {
	repo := map[string]string{}
	for _, declared := range []bool{true, false} {
		name := "no permission"
		if declared {
			name = "permission declared"
		}
		t.Run(name, func(t *testing.T) {
			host := startHelperHost(t, "kv-probe", WithRequestHandler(gate(declared, repo)))

			var result domain.PluginStorageGetResult
			err := host.Call(context.Background(), "kv-probe", nil, &result)
			if declared {
				if err != nil {
					t.Fatalf("a declared permission was refused: %v", err)
				}
				if !result.Found || result.Value != `{"v":1}` {
					t.Fatalf("plugin received %+v, want the stored value", result)
				}
				return
			}
			var rpcErr *domain.RPCError
			if !errors.As(err, &rpcErr) {
				t.Fatalf("refusal = %v, want the plugin to report the -32003 it received", err)
			}
			if code := domain.RPCErrorCode(err); code != domain.RPCErrorPermissionDenied {
				t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorPermissionDenied)
			}
		})
	}
}

// TestManifestPermissionSurvivesParsing is a guard on the loader: the permission
// has to survive parsing, or an enforced right would be impossible to grant.
//
// The lx plugin is the plugin that declares it today, so it is the file the test
// reads. A manifest that lost `kv` in parsing would silently refuse every storage
// request a plugin makes.
func TestManifestPermissionSurvivesParsing(t *testing.T) {
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	manifest, err := LoadManifest(filepath.Join(repoRoot, "plugins", "lx", domain.ManifestFileName))
	if err != nil {
		t.Fatalf("load the lx manifest: %v", err)
	}
	if !manifest.Permissions.KV {
		t.Fatal("the lx plugin's permissions.kv did not survive manifest parsing")
	}

	var parsed domain.PluginManifest
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if !parsed.Permissions.KV {
		t.Fatal("permissions.kv does not survive a JSON round-trip")
	}
}
