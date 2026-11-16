package app

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestDispatchTrackMergedReturnsImmediately: the Wails event listener must not
// run the merge work on the event loop. The callback returns before remap
// finishes, and the refresh still runs once remap is done.
func TestDispatchTrackMergedReturnsImmediately(t *testing.T) {
	release := make(chan struct{})
	refreshed := make(chan string, 1)

	dispatchTrackMerged(
		map[string]string{"from_track_id": "remote-1", "to_track_id": "local-1"},
		func(context.Context, string, string) { <-release },
		func(_ context.Context, toTrackID string) { refreshed <- toTrackID },
	)

	select {
	case <-refreshed:
		t.Fatal("the recommendation refresh ran before the remap finished")
	default:
	}

	close(release)
	select {
	case got := <-refreshed:
		if got != "local-1" {
			t.Fatalf("refresh track id = %q, want local-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the dispatched work never ran")
	}
}

// TestDispatchTrackMergedIgnoresIncompletePayload: a missing id must not start
// a goroutine that remaps half an identity.
func TestDispatchTrackMergedIgnoresIncompletePayload(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	dispatchTrackMerged(
		map[string]string{"from_track_id": "remote-1"},
		func(context.Context, string, string) { mu.Lock(); calls++; mu.Unlock() },
		func(context.Context, string) { mu.Lock(); calls++; mu.Unlock() },
	)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("incomplete payload started %d callbacks", calls)
	}
}
