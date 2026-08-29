package dispatcher

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/runnercap"
)

// RunState is what the reconcile sweep knows about a labeled pod's run.
type RunState int

// Run states for the orphan sweep. The deletion rule is deliberately
// fail-CLOSED toward cleanup: terminal AND unknown both delete (dispatcher
// §5 — "any labeled pod whose run is terminal or unknown is deleted"); only a
// positively-live run keeps its pod.
const (
	// RunStateUnknown means the run cannot be resolved (default zero value).
	RunStateUnknown RunState = iota
	// RunStateLive means the run is in flight and its attempt may still own
	// the pod.
	RunStateLive
	// RunStateTerminal means the run has closed.
	RunStateTerminal
)

// RunStates resolves a run ID (as recorded in the pod's LabelRun) to its
// state. Implementations answer from the daemon's run tracking; an
// unresolvable run answers RunStateUnknown, which the sweep deletes.
type RunStates interface {
	// RunState reports the state of the run a pod is labeled with.
	RunState(ctx context.Context, runLabel string) RunState
}

// SweepOrphans is the restart reconcile half of orphan cleanup (dispatcher
// §5, constraint (a)): cross-namespace ownerReferences are NOT used (k8s GC
// silently deletes a dependent whose namespaced owner lives in another
// namespace — silent-delete-reads-as-eviction), so on restart the dispatcher
// lists every pod it labeled and deletes any whose run is terminal or
// unknown. activeDeadlineSeconds is the always-on backstop between restarts;
// together the design is per-attempt-leak-BOUNDED, not zero-leak, which is
// the accepted v1 posture.
//
// Returns the names of the pods it deleted.
func (d *Dispatcher) SweepOrphans(ctx context.Context, runs RunStates) ([]string, error) {
	if runs == nil {
		return nil, fmt.Errorf("dispatcher: orphan sweep requires a RunStates resolver")
	}
	pods, err := d.pods.ListPods(ctx, d.cfg.Namespace, sweepSelector())
	if err != nil {
		return nil, fmt.Errorf("dispatcher: list labeled stage pods for orphan sweep: %w", err)
	}
	var deleted []string
	var errs []error
	for i := range pods {
		pod := &pods[i]
		if runs.RunState(ctx, pod.Labels[LabelRun]) == RunStateLive {
			continue
		}
		// Fail CLOSED toward cleanup: one pod's delete error must not strand
		// the rest of the batch. Accumulate and keep deleting; the aggregated
		// error is returned after the loop, and `deleted` reflects every pod
		// actually removed. (The sweep also re-runs on every restart, so a
		// pod that errors here is retried, not lost.)
		if err := d.pods.DeletePod(ctx, pod.Namespace, pod.Name); err != nil {
			errs = append(errs, fmt.Errorf("dispatcher: delete orphaned stage pod %s/%s: %w", pod.Namespace, pod.Name, err))
			continue
		}
		deleted = append(deleted, pod.Name)
	}
	return deleted, errors.Join(errs...)
}

// sweepSelector selects exactly the pods this dispatcher stamps: its own
// managed-by marker plus the stage role.
func sweepSelector() map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		// The role label is the shared runnercap constant, so the sweep and
		// the stamp cannot drift.
		runnercap.LabelRole: runnercap.RoleStage,
	}
}
