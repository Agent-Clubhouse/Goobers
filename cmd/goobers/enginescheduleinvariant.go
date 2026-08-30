package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
)

// enginescheduleinvariant.go is the daemon's boot assertion for decision 005's
// "Temporal Schedules are never the trigger source" invariant (#3877,
// finding 002's RESUME-ON-RESTART section, leg (a)).
//
// # What the invariant is
//
// On an engine-enabled daemon instance every engine run is started by THIS
// process: a cron, webhook or trigger-plane fire lands on the local scheduler,
// which dispatches through engine.TemporalStarter (#3876), which starts the
// workflow with WorkflowID == RunID. That equality is what lets every guard in
// enginerunguards.go work by describing a run's own id.
//
// A Temporal Schedule breaks it. The Schedule mechanism mints its OWN workflow
// id per fire, and internal/engine's RunScheduled rewrites the run's RunID to
// a hash of that id — so `guards.await`/`guards.cancel` describing the run id
// get NotFound for a live run, and every one of them has to be resolved
// through the open-workflow inverse instead. That inverse exists (#3877) and
// is correct, but it is bounded by a cache TTL and a page cap sized for the
// EXCEPTIONAL case. Making it the normal case is a capacity decision nobody
// took.
//
// # Why it is a regression guard, not a fix
//
// Nothing in cmd/goobers constructs an engine.ScheduleReconciler today, so
// this check passes trivially on every instance. That is the point: it is
// cheap now, and it converts a future re-wiring of the schedule path from a
// silent capacity cliff into a boot-time refusal that names the schedules.

// engineScheduleLister is what the boot check enumerates through. The daemon
// passes its shared Temporal client's WorkflowService; tests pass a fake, so
// the invariant is provable without a Temporal server.
type engineScheduleLister interface {
	ListSchedules(ctx context.Context, in *workflowservice.ListSchedulesRequest, opts ...grpc.CallOption) (*workflowservice.ListSchedulesResponse, error)
}

// errEngineScheduleCheckUnknown wraps a check that could not FIND OUT, as
// opposed to one that found a violation. An unreachable frontend proves
// nothing either way, and refusing to boot on it would turn a transient
// Temporal outage into a daemon outage — so the two are kept apart at the
// type level rather than by reading the message.
var errEngineScheduleCheckUnknown = errors.New("engine schedule reconciliation invariant could not be checked")

// assertNoEngineScheduleReconciliation fails when an engine-enabled instance's
// Temporal namespace contains Goobers-owned Schedules. A listing failure is
// wrapped in errEngineScheduleCheckUnknown; a schedule that IS present is a
// violation, and it is loud.
func assertNoEngineScheduleReconciliation(ctx context.Context, lister engineScheduleLister, namespace string) error {
	if lister == nil {
		return nil
	}
	schedules, err := engine.ListGoobersSchedules(ctx, lister, namespace)
	if err != nil {
		return fmt.Errorf("%w: %w", errEngineScheduleCheckUnknown, err)
	}
	if len(schedules) == 0 {
		return nil
	}
	return fmt.Errorf(
		"engine-enabled instance has %d Goobers-owned Temporal Schedule(s) in namespace %s (%s): "+
			"decision 005 requires this daemon's own scheduler to be the only trigger source for engine runs, because a "+
			"Schedule fire rewrites the run's id (RunScheduled) and every re-attach and cancel then depends on the "+
			"bounded open-workflow scan; delete the schedule(s), or run this instance with `engine:` unset",
		len(schedules), namespace, strings.Join(schedules, ", "))
}

// checkEngineScheduleInvariant runs the boot check for the daemon's shared
// client, journalling a violation. It reports whether the daemon may start:
// only a positively-observed violation refuses, and an instance with no
// engine configured is vacuously fine — no run there can be engine-driven.
func checkEngineScheduleInvariant(ctx context.Context, e *daemonEngineClient, log *journal.InstanceLog) (error, bool) {
	if e == nil || e.Temporal() == nil {
		return nil, true
	}
	err := assertNoEngineScheduleReconciliation(ctx, e.Temporal().WorkflowService(), e.Namespace())
	switch {
	case err == nil:
		return nil, true
	case errors.Is(err, errEngineScheduleCheckUnknown):
		return err, true
	}
	if log != nil {
		_ = log.Append(journal.Event{
			Type:   journal.EventError,
			Reason: "Temporal Schedules are configured on an engine-enabled instance",
			Error: &journal.ErrorDetail{
				Code:    "engine_schedules_configured",
				Message: err.Error(),
			},
		})
	}
	return err, false
}
