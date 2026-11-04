package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"
)

// kvOrch is the host side bound onto the request handler in these tests. It
// records which plugin asked what, which is how isolation and the -32601
// fallback stay observable.
type kvOrch struct {
	mu       sync.Mutex
	pluginID string
	calls    []string
}

// Handle implements domain.PluginRequestHandler: storage.get answers a fixed
// value and everything else is "not implemented", so both the served and the
// refused paths are exercised through the real wire.
func (o *kvOrch) Handle(_ context.Context, pluginID, method string, _ json.RawMessage) (any, error) {
	o.mu.Lock()
	o.pluginID = pluginID
	o.calls = append(o.calls, method)
	o.mu.Unlock()

	switch method {
	case domain.MethodStorageGet:
		return domain.PluginStorageGetResult{Key: "token", Value: `{"v":1}`, Found: true}, nil
	default:
		return nil, fmt.Errorf("%w: %q", domain.ErrPluginRequestUnknown, method)
	}
}

func (o *kvOrch) seen() (string, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pluginID, append([]string(nil), o.calls...)
}

// TestHostAnswersAPluginRequestDuringACall is the contract of the inbound
// direction: a plugin asking for a stored value while the host is waiting on its
// answer must not deadlock, and the answer must arrive intact.
//
// The helper opens a call, sends its own request in the middle of it, and only
// replies when the host answers that request. A host that treats inbound lines
// as "always -32601" fails the assertions; one that only reads responses hangs
// and fails on the call timeout.
func TestHostAnswersAPluginRequestDuringACall(t *testing.T) {
	orchestrator := &kvOrch{}
	host := startHelperHost(t, "kv-probe", WithRequestHandler(orchestrator.Handle))

	var result domain.PluginStorageGetResult
	if err := host.Call(context.Background(), "kv-probe", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.Found || result.Value != `{"v":1}` {
		t.Fatalf("plugin received %+v, want the host's stored value", result)
	}

	pluginID, calls := orchestrator.seen()
	if pluginID != "io.airmedy.helper" {
		t.Fatalf("handler saw plugin id %q, want io.airmedy.helper", pluginID)
	}
	if len(calls) != 1 || calls[0] != domain.MethodStorageGet {
		t.Fatalf("handler calls = %v, want [storage.get]", calls)
	}
}

// TestUnhandledMethodStaysMethodNotFound: with a handler bound, a method it does
// not implement must still answer the protocol's -32601, which is the
// tolerant-mode promise from the v1 contract.
func TestUnhandledMethodStaysMethodNotFound(t *testing.T) {
	orchestrator := &kvOrch{}
	host := startHelperHost(t, "kv-probe-denied", WithRequestHandler(orchestrator.Handle))

	var result struct {
		OK bool `json:"ok"`
	}
	err := host.Call(context.Background(), "kv-probe-denied", nil, &result)
	if !errors.Is(err, domain.ErrPluginMethodNotFound) {
		t.Fatalf("call = %v, want ErrPluginMethodNotFound", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorMethodNotFound {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorMethodNotFound)
	}
	if _, calls := orchestrator.seen(); len(calls) != 1 || calls[0] != "storage.everything" {
		t.Fatalf("handler calls = %v, want [storage.everything]", calls)
	}
}

// TestNoHandlerKeepsTheV1Answer: a host without a bound handler behaves exactly
// as the pre-KV host did, so an older plugin sees no change.
func TestNoHandlerKeepsTheV1Answer(t *testing.T) {
	host := startHelperHost(t, "kv-probe")

	var result domain.PluginStorageGetResult
	err := host.Call(context.Background(), "kv-probe", nil, &result)
	if !errors.Is(err, domain.ErrPluginMethodNotFound) {
		t.Fatalf("call = %v, want ErrPluginMethodNotFound with no handler bound", err)
	}
}

// TestHandlerPanicDoesNotKillTheSession: the handler runs in the reader
// goroutine, so a panic there would otherwise end the plugin's session (and the
// user's playback) over one malformed request.
func TestHandlerPanicDoesNotKillTheSession(t *testing.T) {
	host := startHelperHost(t, "kv-probe", WithRequestHandler(func(context.Context, string, string, json.RawMessage) (any, error) {
		panic("handler bug")
	}))

	var result domain.PluginStorageGetResult
	started := time.Now()
	err := host.Call(context.Background(), "kv-probe", nil, &result)
	if err == nil {
		t.Fatal("call = nil, want the panic reported as a host failure")
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorInternal {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorInternal)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the panicking handler blocked the call for %s", elapsed)
	}

	// The session must still be usable afterwards: a later call is answered by
	// the plugin, not by a dead transport.
	var ping struct {
		OK bool `json:"ok"`
	}
	if err := host.Call(context.Background(), "ping", nil, &ping); err != nil {
		t.Fatalf("ping after a handler panic: %v", err)
	}
	if !ping.OK {
		t.Fatalf("ping = %+v, want ok", ping)
	}
}
