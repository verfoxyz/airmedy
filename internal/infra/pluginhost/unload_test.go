package pluginhost

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"airmedy/internal/domain"
)

// TestUnloadSendsTheReasonToThePlugin is the lifecycle half of the first-batch
// contract: the plugin learns *why* it is being reclaimed, through a
// notification, before the shutdown request.
//
// The assertion is made on the plugin side (a file the helper writes) rather than
// on the host's bookkeeping, because what matters is that the message crossed the
// wire.
func TestUnloadSendsTheReasonToThePlugin(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "reason.txt")
	host := startHelperHost(t, "record-unload", WithEnv("PLUGIN_UNLOAD_RECORD="+recordPath))

	if err := host.Unload(domain.PluginUnloadReasonReload); err != nil {
		t.Fatalf("Unload: %v", err)
	}

	recorded, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("the plugin recorded no unload reason: %v (did the host send the notification?)", err)
	}
	if got := string(recorded); got != string(domain.PluginUnloadReasonReload) {
		t.Fatalf("plugin recorded reason %q, want %q", got, domain.PluginUnloadReasonReload)
	}

	if reason := host.CloseReason(); reason != domain.PluginUnloadReasonReload {
		t.Fatalf("CloseReason = %q, want reload", reason)
	}
}

// TestUnloadIsIdempotentAndCloseKeepsTheFirstReason: the registry may Unload after
// a Close (a racing shutdown), and the second call must not rewrite the reason or
// re-run the escalation.
func TestUnloadIsIdempotentAndCloseKeepsTheFirstReason(t *testing.T) {
	host := startHelperHost(t, "")
	if err := host.Unload(domain.PluginUnloadReasonShutdown); err != nil {
		t.Fatalf("first Unload: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close after Unload: %v", err)
	}
	if reason := host.CloseReason(); reason != domain.PluginUnloadReasonShutdown {
		t.Fatalf("CloseReason = %q, want shutdown to survive the second call", reason)
	}
}

// TestCloseAloneStillWorks: a host closed without a reason gets the "disabled"
// default rather than an empty value, so a caller that only has Close still
// sends something meaningful on the wire.
func TestCloseAloneStillWorks(t *testing.T) {
	host := startHelperHost(t, "")
	if err := host.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if reason := host.CloseReason(); reason != domain.PluginUnloadReasonDisabled {
		t.Fatalf("CloseReason = %q, want disabled", reason)
	}
}

// TestUnloadDoesNotWaitOnASilentPlugin: the notification is best effort, so a
// plugin that never leaves (here: one that ignores its stdin and keeps running)
// must not turn Close into a hang. The escalation ladder still ends the process.
func TestUnloadDoesNotWaitOnASilentPlugin(t *testing.T) {
	host := startHelperHost(t, "", WithShutdownGrace(500*time.Millisecond))
	started := time.Now()
	if err := host.Unload(domain.PluginUnloadReasonShutdown); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("Unload took %s, want it bounded by the shutdown grace", elapsed)
	}
}
