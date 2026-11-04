// Command echo is the example plugin: one leaf plugin that implements
// stream.resolve itself.
//
// It exists to show the plugin contract without the old host/sub-plugin split:
//
//   - the app launches this process and does not care how it is implemented;
//   - the manifest declares the capability AND the providers it serves, which is
//     the whole routing table — the app never asks a plugin "is this yours?";
//   - stream.resolve answers a URL for a declared provider and fails with a
//     structured kind for anything else.
//
// Build it into an executable next to its manifest:
//
//	go build -o echo ./examples/plugins/echo
//
// The manifest's runtime.command points at ./echo, which is resolved relative to
// the manifest directory.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// hostAPIVersion is the version this example speaks. A real plugin would read it
// from its own constants; the manifest's ^1.0 range accepts it.
const hostAPIVersion = "1.0"

// fixedURL is what a resolve returns. A real plugin would ask a service.
const fixedURL = "https://example.com/echo/audio.mp3"

// rpcRequest mirrors the JSON-RPC envelope the app sends.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcError is the error object from the v1 contract. A stream failure carries a
// structured kind in data; the message is never parsed by the app.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			handleLine(encoder, line)
		}
		if err != nil {
			// The app closed stdin: exit cleanly.
			return
		}
	}
}

// handleLine answers one request.
func handleLine(encoder *json.Encoder, line []byte) {
	var request rpcRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return
	}

	switch request.Method {
	case "initialize":
		reply(encoder, request.ID, map[string]any{
			"apiVersion":   hostAPIVersion,
			"capabilities": []string{"stream.resolve"},
		})

	case "stream.resolve":
		var params struct {
			Source string `json:"source"`
			ID     string `json:"id"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			replyError(encoder, request.ID, rpcError{Code: -32602, Message: "invalid params"})
			return
		}

		if params.Source != "echo.test" {
			// The app routed by declaration, so this should be unreachable. It
			// still fails with a kind rather than a bare string, because retry
			// policy depends on the structured field.
			replyError(encoder, request.ID, rpcError{
				Code:    -32603,
				Message: fmt.Sprintf("this plugin does not serve %q", params.Source),
				Data:    json.RawMessage(`{"kind":"unavailable"}`),
			})
			return
		}
		reply(encoder, request.ID, map[string]any{"url": fixedURL})

	case "shutdown":
		reply(encoder, request.ID, map[string]any{"ok": true})
		os.Exit(0)

	default:
		replyError(encoder, request.ID, rpcError{Code: -32601, Message: "method not found"})
	}
}

// reply writes a successful response.
func reply(encoder *json.Encoder, id json.RawMessage, result any) {
	if len(id) == 0 {
		return
	}
	_ = encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

// replyError writes an error response.
func replyError(encoder *json.Encoder, id json.RawMessage, rpcErr rpcError) {
	if len(id) == 0 {
		return
	}
	_ = encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   rpcErr,
	})
}
