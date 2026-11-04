package plugin

import (
	"context"
	"testing"

	"airmedy/internal/domain"
)

// TestProviderConflictSurvivesRescan: a conflict is recomputed on every scan, so
// a rescan must not quietly pick a winner or clear the reason.
func TestProviderConflictSurvivesRescan(t *testing.T) {
	loader := loaderWith(providerManifest("io.airmedy.alpha", "wy"), providerManifest("io.airmedy.beta", "wy"))

	service, factory, settings := newTestServiceWithSettings(
		t, loader, Config{Root: "/plugins"}, nil)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// A conflicted plugin must refuse to start even when the user asks for it.
	if err := settings.Set(ctx, "io.airmedy.alpha", true); err != nil {
		t.Fatalf("seed flag: %v", err)
	}
	if err := service.SetEnabled(ctx, "io.airmedy.alpha", true); err == nil {
		t.Fatal("enabling a conflicted plugin must be refused")
	}
	if factory.created("io.airmedy.alpha") != nil {
		t.Fatal("a conflicted plugin must never be launched")
	}
	if _, ok := service.HostForProvider(ctx, "wy"); ok {
		t.Fatal("a conflicted provider must have no owner")
	}

	// A rescan re-reports the already-registered ids as duplicates; that is the
	// registry's duplicate rule, not the subject here, so only the entries'
	// state is asserted.
	_, _ = service.Scan(ctx)
	for _, id := range []string{"io.airmedy.alpha", "io.airmedy.beta"} {
		info := findInfo(t, service.List(), id)
		if !info.Invalid {
			t.Fatalf("%s became valid after a rescan", id)
		}
	}
}

// TestSetEnabledTwiceDoesNotDoubleStart: the UI switch can be clicked twice.
func TestSetEnabledTwiceDoesNotDoubleStart(t *testing.T) {
	service, factory, _ := newTestServiceWithSettings(
		t, loaderWith(providerManifest("io.airmedy.owner", "wy")), Config{Root: "/plugins"}, nil)
	ctx := context.Background()
	if _, err := service.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := service.SetEnabled(ctx, "io.airmedy.owner", true); err != nil {
			t.Fatalf("SetEnabled %d: %v", i, err)
		}
	}
	if got := factory.startCount(); got != 1 {
		t.Fatalf("factory starts = %d, want 1 (Start is idempotent)", got)
	}
	if _, reason := mustStatus(t, service, "io.airmedy.owner"); reason != "" {
		t.Fatalf("reason = %q", reason)
	}
}

func mustStatus(t *testing.T, s *Service, id string) (domain.PluginStatus, string) {
	t.Helper()
	status, reason, err := s.Status(id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return status, reason
}
