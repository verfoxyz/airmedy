package pluginhost

import (
	"context"
	"errors"
	"testing"
	"time"

	"airmedy/internal/domain"
)

// startHelperHost launches this binary as a plugin in the given helper mode.
func startHelperHost(t *testing.T, mode string, options ...Option) *Host {
	t.Helper()

	base := []Option{
		WithLogger(discardLogger()),
		WithHandshakeTimeout(2 * time.Second),
		WithShutdownGrace(2 * time.Second),
	}
	host, err := Start(context.Background(), helperManifestPath(t, mode), append(base, options...)...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = host.Close()
	})
	return host
}

// TestHostHandshakeAndPing is acceptance case 3 and 6: the handshake evidence
// is what Capabilities reports, and ping round-trips through the real protocol.
func TestHostHandshakeAndPing(t *testing.T) {
	host := startHelperHost(t, "")

	capabilities := host.Capabilities()
	want := []domain.Capability{
		domain.CapabilityAuth,
		domain.CapabilityStreamResolve,
		"git",   // unknown to the host, kept verbatim
		"hires", // same
	}
	if len(capabilities) != len(want) {
		t.Fatalf("Capabilities = %v, want %v", capabilities, want)
	}
	for i := range want {
		if capabilities[i] != want[i] {
			t.Fatalf("Capabilities = %v, want %v", capabilities, want)
		}
	}

	var ping struct {
		OK bool `json:"ok"`
	}
	if err := host.Call(context.Background(), "ping", nil, &ping); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !ping.OK {
		t.Fatalf("ping result = %+v, want ok true", ping)
	}
}

// TestHostIncompatibleAPIVersion is acceptance case 4: the plugin is rejected
// and reclaimed instead of running under a contract it does not honour.
func TestHostIncompatibleAPIVersion(t *testing.T) {
	_, err := Start(context.Background(), helperManifestPath(t, "incompatible"),
		WithLogger(discardLogger()),
		WithHandshakeTimeout(2*time.Second),
	)
	if !errors.Is(err, domain.ErrPluginIncompatibleAPIVersion) {
		t.Fatalf("expected ErrPluginIncompatibleAPIVersion, got %v", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorIncompatibleVersion {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorIncompatibleVersion)
	}
}

// TestHostNewerMinorRejected covers the additive-only rule: a 1.x plugin that
// claims a newer minor than the host speaks cannot be loaded either.
func TestHostNewerMinorRejected(t *testing.T) {
	_, err := Start(context.Background(), helperManifestPath(t, "newer-minor"),
		WithLogger(discardLogger()),
		WithHandshakeTimeout(2*time.Second),
	)
	if !errors.Is(err, domain.ErrPluginIncompatibleAPIVersion) {
		t.Fatalf("expected ErrPluginIncompatibleAPIVersion, got %v", err)
	}
}

// TestHostHandshakeTimeout is acceptance case 5, with the timeout shortened by
// the injectable option instead of waiting the contract's 10s.
func TestHostHandshakeTimeout(t *testing.T) {
	started := time.Now()
	_, err := Start(context.Background(), helperManifestPath(t, "silent"),
		WithLogger(discardLogger()),
		WithHandshakeTimeout(150*time.Millisecond),
		WithShutdownGrace(time.Second),
	)
	elapsed := time.Since(started)
	if !errors.Is(err, domain.ErrPluginHandshakeTimeout) {
		t.Fatalf("expected ErrPluginHandshakeTimeout, got %v", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorHandshakeTimeout {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorHandshakeTimeout)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("handshake timeout took %s, want well under 5s", elapsed)
	}
}

// TestHostCrashReportsCrashCode is acceptance case 7: a plugin that dies during
// a call yields -32000 and the host is left in a clean, restartable state.
func TestHostCrashReportsCrashCode(t *testing.T) {
	host := startHelperHost(t, "crash-on-ping")

	err := host.Call(context.Background(), "ping", nil, nil)
	if !errors.Is(err, domain.ErrPluginCrashed) {
		t.Fatalf("expected ErrPluginCrashed, got %v", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorCrashed {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorCrashed)
	}
	if host.Running() {
		t.Fatal("host still reports running after the plugin exited")
	}

	host.mu.Lock()
	proc := host.proc
	transport := host.transport
	host.mu.Unlock()
	select {
	case <-proc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("process done channel is still open after the crash was reported")
	}
	select {
	case <-transport.done:
	default:
		t.Fatal("transport session is still open after the crash was reported")
	}
}

// TestHostRestartBudget pins the "-32000 is retryable, at most 3 relaunches"
// rule. The budget is lowered so the test does not wait through crash cycles.
func TestHostRestartBudget(t *testing.T) {
	host := startHelperHost(t, "crash-on-ping", WithMaxRestarts(1))

	// First call crashes the plugin; the second relaunches it and crashes again.
	for attempt := 1; attempt <= 2; attempt++ {
		err := host.Call(context.Background(), "ping", nil, nil)
		if !errors.Is(err, domain.ErrPluginCrashed) {
			t.Fatalf("attempt %d: expected ErrPluginCrashed, got %v", attempt, err)
		}
	}
	// Third call: the single relaunch is spent, so the plugin is unavailable.
	err := host.Call(context.Background(), "ping", nil, nil)
	if !errors.Is(err, domain.ErrPluginUnavailable) {
		t.Fatalf("expected ErrPluginUnavailable after the restart budget, got %v", err)
	}
}

// TestHostGracefulClose is acceptance case 8: the plugin gets the shutdown
// request and exits on its own before the grace period expires.
func TestHostGracefulClose(t *testing.T) {
	host := startHelperHost(t, "")

	if err := host.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if host.Running() {
		t.Fatal("host still running after Close")
	}

	host.mu.Lock()
	proc := host.proc
	transport := host.transport
	host.mu.Unlock()

	select {
	case <-proc.done:
	default:
		t.Fatal("process did not exit on Close")
	}
	if err := proc.exitError(); err != nil {
		t.Fatalf("plugin exited with %v, want a clean exit after shutdown", err)
	}
	select {
	case <-transport.done:
	default:
		t.Fatal("transport session still open after Close")
	}
	if err := host.Call(context.Background(), "ping", nil, nil); !errors.Is(err, domain.ErrPluginHostClosed) {
		t.Fatalf("call after Close = %v, want ErrPluginHostClosed", err)
	}
	// Close is idempotent.
	if err := host.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestHostCallTimeout checks the default per-call deadline is applied even when
// the caller passes a context without one.
func TestHostCallTimeout(t *testing.T) {
	host := startHelperHost(t, "sleep-on-ping",
		WithCallTimeout(100*time.Millisecond),
		WithShutdownGrace(200*time.Millisecond))

	started := time.Now()
	err := host.Call(context.Background(), "ping", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("call timeout took %s", elapsed)
	}
	if !host.Running() {
		t.Fatal("a call timeout must not be treated as a crash")
	}
}

// TestHostStderrIsForwarded proves stderr is treated as plugin logs and does not
// corrupt the JSON-RPC stream on stdout.
func TestHostStderrIsForwarded(t *testing.T) {
	host := startHelperHost(t, "")

	var result struct {
		OK bool `json:"ok"`
	}
	if err := host.Call(context.Background(), "stderr", nil, &result); err != nil {
		t.Fatalf("call after stderr write: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

// TestHostInitializeParams checks the handshake carries the version and locale
// the contract specifies; the helper rejects the call if any field is wrong.
func TestHostInitializeParams(t *testing.T) {
	host := startHelperHost(t, "strict-initialize", WithLocale("zh-CN"))

	var pong struct {
		OK bool `json:"ok"`
	}
	if err := host.Call(context.Background(), "ping", nil, &pong); err != nil {
		t.Fatalf("ping after strict handshake: %v", err)
	}
	if !pong.OK {
		t.Fatalf("ping = %+v", pong)
	}
}

// TestHostManifestSurvivesStart checks the host exposes the parsed manifest.
func TestHostManifestSurvivesStart(t *testing.T) {
	host := startHelperHost(t, "")
	manifest := host.Manifest()
	if manifest.ID != "io.airmedy.helper" {
		t.Fatalf("manifest ID = %q", manifest.ID)
	}
	if manifest.Dir == "" {
		t.Fatal("manifest dir is empty")
	}
}

// TestHostSurvivesStructuredFailure pins the other half of the -32000 story. The
// code is shared between "the plugin-side call failed" and "the plugin is gone",
// so a well-formed error reply must not mark the host stopped: a plugin that
// failed one resolve is still healthy and must serve the next request without a
// relaunch.
func TestHostSurvivesStructuredFailure(t *testing.T) {
	host := startHelperHost(t, "decline")

	err := host.Call(context.Background(), "decline", nil, nil)
	if err == nil {
		t.Fatal("expected the structured failure to surface as an error")
	}
	if !errors.Is(err, domain.ErrPluginCrashed) {
		t.Fatalf("expected the -32000 reply to still classify as ErrPluginCrashed, got %v", err)
	}
	if !host.Running() {
		t.Fatal("a structured failure marked the host stopped; the next request would relaunch a healthy plugin")
	}

	var out map[string]bool
	if err := host.Call(context.Background(), "echo", nil, &out); err != nil {
		t.Fatalf("host did not serve the next call after a structured failure: %v", err)
	}
}
