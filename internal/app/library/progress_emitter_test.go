package library

import (
	"testing"

	"airmedy/internal/domain"
)

// TestProgressEmitterThrottles: a burst of per-item updates must not become a
// burst of webview events, but the final 100% mark must always reach the UI.
func TestProgressEmitterThrottles(t *testing.T) {
	events := make([]domain.SyncProgress, 0)
	emit := newProgressEmitterWith(true, func(progress domain.SyncProgress) {
		events = append(events, progress)
	})

	for current := 1; current <= 1000; current++ {
		emit(current, 1000, "track")
	}

	if len(events) > 12 {
		t.Fatalf("emitted %d events for 1000 items, want the throttle to keep it <= 12", len(events))
	}
	if len(events) == 0 || events[0].Current != 1 {
		t.Fatalf("first event = %+v, want current 1", events)
	}
	if events[len(events)-1].Current != 1000 || events[len(events)-1].Total != 1000 {
		t.Fatalf("last throttled event = %+v, want 1000/1000", events[len(events)-1])
	}

	// The loops explicitly send the final value after finishing; it must pass
	// even though it is not the first update.
	emit(1000, 1000, "done")
	if events[len(events)-1].Current != 1000 || events[len(events)-1].Path != "done" {
		t.Fatalf("explicit final event = %+v, want 1000/1000 with the final path", events[len(events)-1])
	}
}

// TestProgressEmitterBackgroundIsSilent: automatic syncs must not open the
// progress dialog.
func TestProgressEmitterBackgroundIsSilent(t *testing.T) {
	calls := 0
	emit := newProgressEmitterWith(false, func(domain.SyncProgress) { calls++ })
	emit(1, 10, "a")
	emit(5, 10, "b")
	emit(10, 10, "c")
	if calls != 0 {
		t.Fatalf("background emitter sent %d events, want 0", calls)
	}
}
