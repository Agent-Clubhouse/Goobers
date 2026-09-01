package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
)

// scheduleinvariant.go answers one question for the daemon's boot check
// (decision 005 D2, #3877): does this Temporal namespace contain Goobers-owned
// Schedules?
//
// Finding 002's "RESUME-ON-RESTART = RE-ATTACH ONLY" section closes the
// RunID-rewrite gap on two legs. The second leg is the open-workflow inverse
// (ResolveWorkflowID). The FIRST is an invariant: on an engine-enabled daemon
// instance, Temporal Schedules are never the trigger source — cron, webhook
// and trigger-plane fires reach the engine through the daemon's own scheduler
// (#3876), which starts every run with WorkflowID == RunID. Nothing in
// cmd/goobers constructs a ScheduleReconciler, so the invariant holds today
// and this is a REGRESSION GUARD, not a fix for a live problem.
//
// It is worth guarding because the failure it prevents is silent and
// asymmetric. A materialized Schedule makes RunScheduled the start path
// again, which makes every scheduled run's RunID a hash of a workflow id, and
// resolving those hashes back becomes the load-bearing path rather than the
// exceptional one — bounded by the scan's cache TTL and page cap, which are
// sized for the exceptional case. The daemon would keep working right up to
// the point where it silently could not.

const (
	// scheduleScanPageSize and maxScheduleScanPages bound the boot check the
	// way the open-workflow scan is bounded: a namespace with a pathological
	// number of schedules must not turn a boot diagnostic into an unbounded
	// enumeration. Past the cap the answer is an error (unknown), never a
	// short list read as "none".
	scheduleScanPageSize = 100
	maxScheduleScanPages = 10
	// scheduleScanTimeout bounds the whole enumeration, so a wedged frontend
	// delays a boot check rather than stalling it.
	scheduleScanTimeout = 15 * time.Second
)

// scheduleLister is the slice of the Temporal service the boot check needs.
// workflowservice.WorkflowServiceClient satisfies it; tests provide a fake.
type scheduleLister interface {
	ListSchedules(ctx context.Context, in *workflowservice.ListSchedulesRequest, opts ...grpc.CallOption) (*workflowservice.ListSchedulesResponse, error)
}

// ListGoobersSchedules returns the ids of every Temporal Schedule in the
// namespace that this tree could have created — the ones ScheduleID and
// scheduleOwnedPrefix mint, all of which carry scheduleIDPrefix.
//
// It deliberately does NOT filter to one instance id. The instance id a
// ScheduleSnapshot is reconciled under is not something the daemon has
// (nothing populates ScheduleSnapshot.InstanceID in this tree), and the
// invariant is about the namespace an engine-enabled daemon shares: a sibling
// instance materializing Schedules puts scheduled-shaped runs into the same
// visibility this daemon resolves against. Naming them all is what lets an
// operator see which one to remove.
//
// An enumeration that cannot complete is an error, never an empty list: "no
// schedules" is the answer the boot check treats as healthy, so it must never
// be inferred from a failed page.
func ListGoobersSchedules(ctx context.Context, service scheduleLister, namespace string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, scheduleScanTimeout)
	defer cancel()
	var (
		found     []string
		pageToken []byte
	)
	for page := 0; ; page++ {
		if page >= maxScheduleScanPages {
			return nil, fmt.Errorf("engine: schedule scan exceeded %d pages; schedule reconciliation state unknown", maxScheduleScanPages)
		}
		resp, err := service.ListSchedules(ctx, &workflowservice.ListSchedulesRequest{
			Namespace:       namespace,
			MaximumPageSize: scheduleScanPageSize,
			NextPageToken:   pageToken,
		})
		if err != nil {
			return nil, fmt.Errorf("engine: list Temporal schedules in %s: %w", namespace, err)
		}
		for _, entry := range resp.GetSchedules() {
			id := entry.GetScheduleId()
			if strings.HasPrefix(id, scheduleIDPrefix) {
				found = append(found, id)
			}
		}
		pageToken = resp.GetNextPageToken()
		if len(pageToken) == 0 {
			return found, nil
		}
	}
}
