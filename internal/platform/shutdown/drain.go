package shutdown

import "time"

// DrainWithEscalation runs the platform-agnostic downstream of every shutdown
// trigger. It invokes drain — which should return once in-flight work has
// finished checkpointing — and, if drain has not returned within grace, invokes
// escalate once (the hard-kill fallback, e.g. proc.Tree.Kill on a stuck stage's
// process group) and keeps waiting for drain to return. It reports whether drain
// finished before the grace deadline.
//
// This is the single shared shutdown sequence the cross-platform design
// mandates: every trigger — unix SIGINT/SIGTERM, windows Ctrl+C/Ctrl+Break, or a
// Windows SCM STOP via Notifier.RequestStop — funnels here, so drain-then-kill
// policy lives in exactly one place rather than being reimplemented per trigger.
//
// escalate may be nil, in which case an overrunning drain is simply waited out.
// drain must eventually return; escalate exists to make that happen when a
// graceful drain wedges.
func DrainWithEscalation(grace time.Duration, drain func(), escalate func()) (drainedInTime bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		drain()
	}()

	select {
	case <-done:
		return true
	case <-time.After(grace):
	}

	if escalate != nil {
		escalate()
	}
	<-done
	return false
}
