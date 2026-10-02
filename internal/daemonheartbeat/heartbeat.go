// Package daemonheartbeat renders periodic daemon liveness and scheduler activity.
package daemonheartbeat

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/cpustat"
	"github.com/goobers/goobers/internal/platform/memstat"
)

type heartbeatActivity struct {
	triggers int
	started  int
	finished int
	skipped  int
}

func summarizeHeartbeat(events []journal.Event, afterSeq uint64) (heartbeatActivity, uint64) {
	activity := heartbeatActivity{}
	lastSeq := afterSeq
	for _, event := range events {
		if event.Seq <= afterSeq {
			continue
		}
		if event.Seq > lastSeq {
			lastSeq = event.Seq
		}
		switch event.Type {
		case journal.EventTriggerFired:
			activity.triggers++
		case journal.EventRunStarted:
			activity.started++
		case journal.EventRunFinished:
			activity.finished++
		case journal.EventTickSkipped:
			activity.skipped++
		}
	}
	return activity, lastSeq
}

// Emit prints a periodic liveness line carrying scheduler activity
// since startup and the daemon's current memory and CPU footprint.
//
// Neither resource clause is decoration. Without the memory one the
// operator-facing log cannot distinguish a leaking daemon from a pod whose
// memory cgroup is filling with page cache produced by the stages it runs — in
// #3949 the 47 minutes of heartbeats preceding an OOMKill were indistinguishable
// from healthy ones, and the kill was misread as a daemon leak while the
// daemon's own anonymous memory sat flat at 62 MiB.
//
// The CPU one answers the question that incident could not (#3963): a container
// pinned at its CPU quota is indistinguishable from a busy one in every
// point-in-time metric, and only nr_throttled separates "doing work" from
// "being stopped". The same pod was losing 79.5% of its CFS periods to
// throttling, visible nowhere but a shell inside it.
//
// Both reads are cheap (no stop-the-world, a few small file reads) and neither
// can fail, so it costs the heartbeat nothing to always carry them.
func Emit(
	ctx context.Context,
	stdout io.Writer,
	schedulerDir string,
	workflowCount func() int,
	tail *journal.InstanceLogTail,
	err error,
	interval time.Duration,
	pending *PendingUpdate,
	done chan<- struct{},
) {
	defer close(done)
	defer func() { _ = tail.Close() }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// The output writer can wake a shutdown caller before this loop
			// gets back to its select. Do not process a tick that was already
			// queued when cancellation won that race.
			if ctx.Err() != nil {
				return
			}
			if tail == nil {
				tail, err = journal.OpenInstanceLogTail(schedulerDir)
			}
			if err == nil {
				var events []journal.Event
				events, err = tail.Events()
				if err == nil {
					activity, _ := summarizeHeartbeat(events, 0)
					_, _ = fmt.Fprintf(stdout, "[%s] alive — %d workflow(s), %d trigger(s) fired, %d run(s) started, %d run(s) finished, %d tick(s) skipped; %s; %s%s\n",
						now.Format("15:04:05"), workflowCount(), activity.triggers, activity.started, activity.finished, activity.skipped, memstat.Read(), cpustat.Read(),
						pending.Clause())
					continue
				}
				_ = tail.Close()
				tail = nil
			}
			if err != nil {
				// Both resource clauses ride the degraded line too. A daemon
				// that has lost its journal tail is exactly when an operator
				// most needs to know whether it is also about to be OOM-killed,
				// or merely too throttled to make progress.
				_, _ = fmt.Fprintf(stdout, "[%s] alive — scheduler activity unavailable: %v; %s; %s%s\n",
					now.Format("15:04:05"), err, memstat.Read(), cpustat.Read(), pending.Clause())
				continue
			}
		}
	}
}
