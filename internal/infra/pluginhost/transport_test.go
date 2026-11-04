package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePlugin is an in-memory peer for the transport: a pair of pipes wired the
// same way the process pipes are, plus a goroutine speaking the protocol.
type fakePlugin struct {
	transport *transport
	requests  *bufio.Reader
	responses io.Writer

	mu     sync.Mutex
	writer io.Closer
}

// newFakePlugin starts a peer whose handler answers each request. Returning a
// nil result and nil error replies with a JSON null result.
func newFakePlugin(t *testing.T, handler func(fp *fakePlugin, request rpcMessage) (any, *domain.RPCError)) *fakePlugin {
	t.Helper()
	return newFakePluginWithRequestHandler(t, handler, nil)
}

// newFakePluginWithRequestHandler is newFakePlugin with a bound plugin → host
// request handler, which is what the inbound-method tests need.
func newFakePluginWithRequestHandler(
	t *testing.T,
	handler func(fp *fakePlugin, request rpcMessage) (any, *domain.RPCError),
	requestHandler domain.PluginRequestHandler,
) *fakePlugin {
	t.Helper()

	hostReads, pluginWrites := io.Pipe()
	pluginReads, hostWrites := io.Pipe()

	transport := newTransport(hostReads, hostWrites, "test-plugin", discardLogger(), requestHandler)
	peer := &fakePlugin{
		transport: transport,
		requests:  bufio.NewReader(pluginReads),
		responses: pluginWrites,
		writer:    pluginWrites,
	}
	encoder := json.NewEncoder(pluginWrites)

	go func() {
		defer func() { _ = pluginWrites.Close() }()
		for {
			line, err := peer.requests.ReadBytes('\n')
			if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
				var request rpcMessage
				if jsonErr := json.Unmarshal([]byte(trimmed), &request); jsonErr == nil {
					result, rpcErr := handler(peer, request)
					if len(request.ID) > 0 {
						reply := map[string]any{"jsonrpc": "2.0", "id": request.ID}
						if rpcErr != nil {
							reply["error"] = rpcErr
						} else {
							reply["result"] = result
						}
						peer.mu.Lock()
						_ = encoder.Encode(reply)
						peer.mu.Unlock()
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		_ = pluginWrites.Close()
		_ = pluginReads.Close()
		_ = hostWrites.Close()
	})
	return peer
}

// crash simulates the plugin process dying: its stdout pipe reaches EOF.
func (fp *fakePlugin) crash() {
	_ = fp.writer.Close()
}

func (fp *fakePlugin) writeRaw(t *testing.T, raw string) {
	t.Helper()
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if _, err := io.WriteString(fp.responses, raw); err != nil {
		t.Fatalf("write raw response: %v", err)
	}
}

func TestTransportCall(t *testing.T) {
	peer := newFakePlugin(t, func(_ *fakePlugin, request rpcMessage) (any, *domain.RPCError) {
		if request.Method != "ping" {
			return nil, &domain.RPCError{Code: domain.RPCErrorMethodNotFound, Message: "no such method"}
		}
		return map[string]bool{"ok": true}, nil
	})

	var result struct {
		OK bool `json:"ok"`
	}
	if err := peer.transport.call(context.Background(), "ping", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v, want ok true", result)
	}
}

func TestTransportCallEncodesParams(t *testing.T) {
	peer := newFakePlugin(t, func(_ *fakePlugin, request rpcMessage) (any, *domain.RPCError) {
		return request.Params, nil
	})

	type params struct {
		Title string `json:"title"`
	}
	var echoed params
	if err := peer.transport.call(context.Background(), "echo", params{Title: "夜曲"}, &echoed); err != nil {
		t.Fatalf("call: %v", err)
	}
	if echoed.Title != "夜曲" {
		t.Fatalf("echoed = %+v", echoed)
	}
}

func TestTransportMapsPluginErrorCodes(t *testing.T) {
	peer := newFakePlugin(t, func(_ *fakePlugin, _ rpcMessage) (any, *domain.RPCError) {
		return nil, &domain.RPCError{Code: domain.RPCErrorMethodNotFound, Message: "nope"}
	})

	err := peer.transport.call(context.Background(), "missing", nil, nil)
	if !errors.Is(err, domain.ErrPluginMethodNotFound) {
		t.Fatalf("expected ErrPluginMethodNotFound, got %v", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorMethodNotFound {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorMethodNotFound)
	}
}

func TestTransportCallTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	peer := newFakePlugin(t, func(_ *fakePlugin, _ rpcMessage) (any, *domain.RPCError) {
		// Stay silent until the test is over, so the call can only end by its
		// own deadline.
		<-release
		return nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := peer.transport.call(ctx, "slow", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestTransportCrashFailsPendingCall(t *testing.T) {
	release := make(chan struct{})
	peer := newFakePlugin(t, func(_ *fakePlugin, _ rpcMessage) (any, *domain.RPCError) {
		<-release
		return nil, nil
	})

	done := make(chan error, 1)
	go func() { done <- peer.transport.call(context.Background(), "ping", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	peer.crash()
	close(release)

	select {
	case err := <-done:
		if !errors.Is(err, domain.ErrPluginCrashed) {
			t.Fatalf("expected ErrPluginCrashed, got %v", err)
		}
		if code := domain.RPCErrorCode(err); code != domain.RPCErrorCrashed {
			t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorCrashed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call did not fail after the plugin stream closed")
	}
}

// TestTransportToleratesNoiseFromPlugin pins the tolerant reader: one malformed
// line must not kill the session or lose the response that follows it.
func TestTransportToleratesNoiseFromPlugin(t *testing.T) {
	peer := newFakePlugin(t, func(fp *fakePlugin, _ rpcMessage) (any, *domain.RPCError) {
		fp.writeRaw(t, "this is not json\n")
		return map[string]bool{"ok": true}, nil
	})

	var result struct {
		OK bool `json:"ok"`
	}
	if err := peer.transport.call(context.Background(), "ping", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

// TestTransportAnswersPluginRequests checks the host does not leave a plugin
// request hanging: it answers -32601 rather than ignoring it.
func TestTransportAnswersPluginRequests(t *testing.T) {
	peer := newFakePlugin(t, func(fp *fakePlugin, request rpcMessage) (any, *domain.RPCError) {
		if request.Method != "trigger" {
			return nil, nil
		}
		fp.writeRaw(t, `{"jsonrpc":"2.0","id":"plugin-1","method":"ui/open"}`+"\n")
		line, err := fp.requests.ReadBytes('\n')
		if err != nil {
			t.Errorf("reading host answer: %v", err)
			return nil, nil
		}
		var answer rpcMessage
		if err := json.Unmarshal(line, &answer); err != nil {
			t.Errorf("decode host answer: %v", err)
			return nil, nil
		}
		if answer.Error == nil || answer.Error.Code != domain.RPCErrorMethodNotFound {
			t.Errorf("host answer = %+v, want -32601 error", answer)
		}
		if string(answer.ID) != `"plugin-1"` {
			t.Errorf("host answer id = %s, want \"plugin-1\"", answer.ID)
		}
		return map[string]bool{"ok": true}, nil
	})

	if err := peer.transport.call(context.Background(), "trigger", nil, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
}

func TestTransportConcurrentCallsCorrelate(t *testing.T) {
	peer := newFakePlugin(t, func(_ *fakePlugin, request rpcMessage) (any, *domain.RPCError) {
		return request.Params, nil
	})

	const calls = 16
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			payload := struct {
				Index int `json:"index"`
			}{Index: index}
			var echoed struct {
				Index int `json:"index"`
			}
			if err := peer.transport.call(context.Background(), "echo", payload, &echoed); err != nil {
				errs <- fmt.Errorf("call %d: %w", index, err)
				return
			}
			if echoed.Index != index {
				errs <- fmt.Errorf("call %d got response %d (ids crossed)", index, echoed.Index)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
