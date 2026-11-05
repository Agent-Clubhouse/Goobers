package terminalreport

import (
	"sync"
	"time"
)

const (
	reportCallTimeout     = 30 * time.Second
	reportLeaseSkewMargin = 30 * time.Second
	reportLeaseDuration   = reportCallTimeout + reportLeaseSkewMargin
	replayLockFileName    = "replay.lock"
)

// Keep the target-scoped in-process guard across distinct queue directories.
// The outbox replay flock is the cross-process authority for one queue.
var reportFlights sync.Map
var replaySkips sync.Map

func enterFlight(key string) (func(), bool) {
	if _, loaded := reportFlights.LoadOrStore(key, struct{}{}); loaded {
		return nil, false
	}
	return func() { reportFlights.Delete(key) }, true
}

type replaySkipCounter struct {
	mu    sync.Mutex
	count int
}

func replaySkip(dir string) int {
	value, _ := replaySkips.LoadOrStore(dir, &replaySkipCounter{})
	counter := value.(*replaySkipCounter)
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counter.count++
	return counter.count
}

func resetReplaySkips(dir string) {
	replaySkips.Delete(dir)
}
