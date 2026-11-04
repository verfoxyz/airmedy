package pluginhost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHelperProcess is not a normal test: when the host-under-test launches this
// same binary as a plugin, GO_WANT_HELPER_PROCESS=1 is set and the function
// speaks the line-delimited JSON-RPC protocol on stdio. PLUGIN_HELPER_MODE picks
// the behaviour a test needs (silent, incompatible, crash, ...).
//
// This keeps the end-to-end tests free of any external runtime such as Node, so
// they run in CI and on a machine that has only the Go toolchain.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	mode := os.Getenv("PLUGIN_HELPER_MODE")

	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			handleHelperRequest(encoder, mode, trimmed)
		}
		if err != nil {
			// EOF: the host closed our stdin, so exit cleanly. os.Exit keeps
			// the testing framework from printing PASS into the JSON stream.
			os.Exit(0)
		}
	}
}

func handleHelperRequest(encoder *json.Encoder, mode string, line []byte) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		return
	}

	reply := func(result any) {
		if len(request.ID) == 0 {
			return
		}
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  result,
		})
	}
	replyError := func(code int, message string) {
		if len(request.ID) == 0 {
			return
		}
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error":   map[string]any{"code": code, "message": message},
		})
	}
	// replyErrorWithKind answers with the structured kind a real plugin sends
	// when it declines a request: -32000 plus data.kind.
	replyErrorWithKind := func(code int, message, kind string) {
		if len(request.ID) == 0 {
			return
		}
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error": map[string]any{
				"code":    code,
				"message": message,
				"data":    map[string]any{"kind": kind},
			},
		})
	}

	switch request.Method {
	case "lifecycle.unload":
		// A notification: record the reason and do not answer. The file path
		// arrives through the plugin's own environment so the test can point it
		// at a temp directory.
		recordUnloadReason(request.Params)
	case "initialize":
		if mode == "strict-initialize" {
			// Verify the handshake shape from the v1 contract before answering.
			var params struct {
				HostVersion string `json:"hostVersion"`
				APIVersion  string `json:"apiVersion"`
				Locale      string `json:"locale"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil {
				replyError(-32602, "invalid initialize params")
				return
			}
			if params.HostVersion == "" || params.APIVersion != "1.0" || params.Locale != "zh-CN" {
				replyError(-32602, "unexpected initialize params")
				return
			}
		}
		switch mode {
		case "silent":
			// Never answer: the host must time out and reap the process.
			return
		case "incompatible":
			reply(map[string]any{"apiVersion": "2.0", "capabilities": []string{"auth"}})
		case "newer-minor":
			reply(map[string]any{"apiVersion": "1.9", "capabilities": []string{"auth"}})
		case "no-api-version":
			reply(map[string]any{"capabilities": []string{"auth"}})
		case "strict-initialize":
			reply(map[string]any{"apiVersion": "1.0", "capabilities": []string{"auth"}})
		default:
			// Deliberately includes capabilities the host does not know and the
			// manifest does not declare, to prove tolerant mode is preserved.
			reply(map[string]any{
				"apiVersion":   "1.0",
				"capabilities": []string{"auth", "stream.resolve", "git", "hires"},
			})
		}
	case "ping":
		if mode == "crash-on-ping" {
			os.Exit(1)
		}
		if mode == "sleep-on-ping" {
			time.Sleep(2 * time.Second)
		}
		reply(map[string]bool{"ok": true})
	case "echo":
		reply(request.Params)
	case "stderr":
		fmt.Fprintln(os.Stderr, "helper stderr line")
		reply(map[string]bool{"ok": true})
	case "decline":
		// The app routes by manifest declaration, so a plugin never declines a
		// provider; this case now exercises "unknown kind stays unclassified".
		replyErrorWithKind(-32000, "helper declined", "not_a_kind")
	case "fail":
		replyError(-32002, "helper refused")
	case "kv-probe":
		// The plugin -> host direction: ask the host for a value and answer the
		// original call with what came back. This only works if the host reads
		// inbound requests while a call is in flight on the same session.
		replyWithHostRequest(encoder, request.ID)
	case "kv-probe-denied":
		replyWithHostRequest(encoder, request.ID, "storage.everything")
	case "record-unload":
		// The host sends lifecycle.unload as a notification before shutdown.
		// Record the reason to a file the test can read, so the assertion is
		// about what the plugin actually received, not about host bookkeeping.
		reply(map[string]bool{"ok": true})
	case "shutdown":
		reply(map[string]bool{"ok": true})
		os.Exit(0)
	default:
		replyError(-32601, "method not found")
	}
}

// helperManifestPath writes a manifest that launches this test binary as a
// plugin in the requested mode and returns the manifest path.
func helperManifestPath(t *testing.T, mode string) string {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	manifest := map[string]any{
		"id":         "io.airmedy.helper",
		"name":       "Helper Plugin",
		"version":    "1.0.0",
		"apiVersion": "^1.0",
		"runtime": map[string]any{
			"type":    "process",
			"command": executable,
			"args":    []string{"-test.run=TestHelperProcess", "--"},
			"env": map[string]string{
				"GO_WANT_HELPER_PROCESS": "1",
				"PLUGIN_HELPER_MODE":     mode,
			},
		},
		"capabilities": []string{"auth"},
	}

	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode helper manifest: %v", err)
	}
	path := writeManifestFile(t, encoded)
	return path
}

// writeManifestFile writes raw manifest bytes into a fresh temp directory.
func writeManifestFile(t *testing.T, encoded []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "airmedy-plugin.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write helper manifest: %v", err)
	}
	return path
}

// manifestJSON renders a manifest body with the given field overrides. Tests use
// it to keep each case focused on the one field under test.
func manifestJSON(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	base := map[string]any{
		"id":         "io.airmedy.test",
		"name":       "Test",
		"version":    "1.2.0",
		"apiVersion": "^1.0",
		"runtime": map[string]any{
			"type":    "process",
			"command": "node",
			"args":    []string{"index.js"},
		},
		"capabilities": []string{"auth", "stream.resolve"},
	}
	for key, value := range overrides {
		if value == nil {
			delete(base, key)
			continue
		}
		base[key] = value
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return encoded
}

// assertErrorIs fails when err does not wrap want.
func assertErrorIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("expected error wrapping %q, got %v", want, err)
	}
}

// replyWithHostRequest asks the host for a value over the same session, then
// answers the plugin's own call with what the host returned.
//
// This is the only inbound direction in the protocol: the plugin writes a line
// with its own id, the host answers it, and the plugin keeps the original
// request open until the answer arrives. A host that only reads responses
// deadlocks here, which is what this helper detects.
func replyWithHostRequest(encoder *json.Encoder, id json.RawMessage, method ...string) {
	target := "storage.get"
	if len(method) > 0 {
		target = method[0]
	}

	// The host echoes our params back into the answer, so send a key it must
	// return verbatim for the test to have something to assert.
	_ = encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      "kv-1",
		"method":  target,
		"params":  map[string]any{"key": "token"},
	})

	// Read until the host's answer arrives: the host may still be finishing
	// other work, so skip anything that is not a response to kv-1.
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var message struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(trimmed, &message) == nil && string(message.ID) == `"kv-1"` {
				switch {
				case message.Error != nil:
					_ = encoder.Encode(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"error": map[string]any{
							"code":    message.Error.Code,
							"message": message.Error.Message,
						},
					})
				case json.Valid(message.Result):
					_ = encoder.Encode(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"result":  message.Result,
					})
				default:
					_ = encoder.Encode(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"result":  map[string]any{"ok": false},
					})
				}
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// recordUnloadReason writes the reason carried by a lifecycle.unload notification
// to the file named by PLUGIN_UNLOAD_RECORD. A missing path is a no-op, so the
// helper stays harmless in the modes that do not care.
//
// It reports what the *plugin* received, which is the only thing the notification
// contract promises: the file appears if and only if the host actually sent the
// message.
func recordUnloadReason(params json.RawMessage) {
	path := os.Getenv("PLUGIN_UNLOAD_RECORD")
	if path == "" {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(body.Reason), 0o644)
}
