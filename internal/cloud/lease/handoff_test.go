package lease

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSelfFencedHandleNeverHandsOffEarly: Run releases voluntarily only for a
// handle that is not self-fenced, whichever way its renewal loop exited. When
// cancellation wins the race, the loop returns nil though the handle has
// already passed its self-fence window; releasing then would hand authority
// over before the database TTL.
func TestSelfFencedHandleNeverHandsOffEarly(t *testing.T) {
	now := time.Now()
	manager := &Manager{config: Config{SelfFenceAfter: 10 * time.Second, Now: func() time.Time { return now }}}
	handle := &Handle{lastRenew: now}
	if err := manager.handoffRefusal(handle, nil, nil); err != nil {
		t.Fatalf("live handle refused handoff: %v", err)
	}
	if err := manager.handoffRefusal(handle, nil, context.Canceled); err != nil {
		t.Fatalf("cancelled live handle refused handoff: %v", err)
	}
	if err := manager.handoffRefusal(handle, ErrWorkStillRunning, nil); !errors.Is(err, ErrWorkStillRunning) {
		t.Fatalf("still-running work handed off: %v", err)
	}
	now = now.Add(10 * time.Second)
	if err := manager.handoffRefusal(handle, nil, nil); !errors.Is(err, ErrSelfFenced) {
		t.Fatalf("self-fenced handle with a clean renewal exit handed off early: %v", err)
	}
}
