package lease

import (
	"testing"
	"time"
)

func TestSelfFenceUsesMonotonicElapsedTime(t *testing.T) {
	now := time.Now()
	manager := &Manager{config: Config{
		SelfFenceAfter: 10 * time.Second,
		Now:            func() time.Time { return now },
	}}
	handle := &Handle{lastRenew: now}
	now = now.Add(9 * time.Second)
	if manager.SelfFenced(handle) {
		t.Fatal("self-fenced before safety window")
	}
	now = now.Add(time.Second)
	if !manager.SelfFenced(handle) {
		t.Fatal("did not self-fence at safety window")
	}
}
