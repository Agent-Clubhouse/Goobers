package workerhost

import "time"

// The SDK cancels activities only at the end of its drain. Keep the host and
// authenticated client alive while child dispatch records bounded cleanup and
// explicit completion. Ordinary abandoned activities do not extend shutdown.
func (t *activityTracker) waitForChildSettlement(timeout time.Duration) {
	if t.children.Load() == 0 {
		return
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for t.children.Load() > 0 {
		select {
		case <-deadline.C:
			return
		case <-poll.C:
		}
	}
}
