package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/runnercap"
)

// RunState is what the reconcile sweep learned about a labeled pod's attempt.
//
// The zero value is deliberately the one that keeps the pod. Decision 003
// rejected option C partly because "SweepOrphans (zero callers today;
// fail-closed toward deletion)" would collide with the worker's own pods, and
// the graft it adopted instead states the rule the other way round: an attempt
// whose workflow is still Running is ADOPTED — left alone — and only a
// POSITIVELY settled attempt is disposed. Every remaining case (Temporal
// unreachable, the answer ambiguous, the pod unaddressable) leaves the pod and
// lets the always-on activeDeadlineSeconds stamp reclaim it. Deleting a pod
// whose attempt might still be executing destroys in-flight work and, for a
// mutating stage, does it invisibly; leaving one costs at most one stage
// timeout of cluster capacity.
type RunState int

const (
	// RunStateIndeterminate means the sweep could not establish the attempt's
	// state — an unreachable engine above all. The pod is LEFT. It is the zero
	// value so that a resolver which forgets a case, or a state added later
	// and not yet handled, fails toward leaving rather than toward deleting.
	RunStateIndeterminate RunState = iota
	// RunStateLive means the attempt's workflow is still executing: the pod is
	// adopted by this sweep and left to finish.
	RunStateLive
	// RunStateTerminal means the sweep POSITIVELY established that no workflow
	// is executing this attempt — Completed, Failed, or no such execution at
	// all. The pod is disposed.
	RunStateTerminal
)

// PodAttempt is the attempt identity read back off one labeled pod: the
// verbatim run ID and stage from the dispatcher's identity annotations plus
// the attempt ordinal from its label. A resolver composes the engine's
// per-attempt workflow id from these (engine.DispatchOneWorkflowID), which is
// why they must be the exact dispatch-time strings and not the sanitized
// label values.
type PodAttempt struct {
	// Pod and Namespace name the pod being considered, for diagnostics.
	Pod       string
	Namespace string
	// RunID, Stage and Attempt are the attempt's verbatim identity. Attempt is
	// an int to match Attempt.Number, the field it is read back from.
	RunID   string
	Stage   string
	Attempt int
}

// RunStates resolves one labeled pod's attempt to its state. Implementations
// answer from the engine (Temporal DescribeWorkflowExecution); anything they
// could not determine must answer RunStateIndeterminate, which keeps the pod.
type RunStates interface {
	// RunState reports the state of the attempt a pod was created for.
	RunState(ctx context.Context, attempt PodAttempt) RunState
}

// SweepOrphans is the restart reconcile half of orphan cleanup (dispatcher
// §5, constraint (a)): cross-namespace ownerReferences are NOT used (k8s GC
// silently deletes a dependent whose namespaced owner lives in another
// namespace — silent-delete-reads-as-eviction), so on restart the dispatcher
// lists the pods IT labeled and disposes the ones whose attempt is settled.
// activeDeadlineSeconds is the always-on backstop between restarts; together
// the design is per-attempt-leak-BOUNDED, not zero-leak, which is the accepted
// v1 posture.
//
// Two scopes narrow what a sweep can reach, and both are load-bearing:
//
//   - by OWNER (LabelOwner): decision 003 wires this on the worker, and a
//     cluster runs more than one. Without the owner scope, worker B's restart
//     lists worker A's live stage pods, and a resolver that cannot see A's
//     attempts disposes them.
//   - by STATE: only RunStateTerminal deletes. See RunState.
//
// Returns the names of the pods it disposed. A pod that could not be addressed
// or deleted is left, and named in the aggregated error.
func (d *Dispatcher) SweepOrphans(ctx context.Context, runs RunStates) ([]string, error) {
	if runs == nil {
		return nil, errors.New("dispatcher: orphan sweep requires a RunStates resolver")
	}
	owner := d.cfg.ownerLabel()
	if owner == "" {
		return nil, errors.New("dispatcher: orphan sweep requires Config.Owner — an unscoped sweep would dispose other workers' stage pods")
	}
	pods, err := d.pods.ListPods(ctx, d.cfg.Namespace, sweepSelector(owner))
	if err != nil {
		return nil, fmt.Errorf("dispatcher: list labeled stage pods for orphan sweep: %w", err)
	}
	var deleted []string
	var errs []error
	for i := range pods {
		pod := &pods[i]
		attempt, ok := podAttempt(pod)
		if !ok {
			// Selected but not addressable: no identity annotations, so no
			// workflow id can be composed and no resolver can answer. Leave it
			// (the deadline stamp reclaims it) and say so — a pod in this state
			// means something stamped LabelOwner without going through
			// stampIdentityAnnotations, which is a bug in this package.
			errs = append(errs, fmt.Errorf(
				"dispatcher: stage pod %s/%s carries no attempt identity (%s/%s); left in place",
				pod.Namespace, pod.Name, AnnotationRunID, AnnotationStage))
			continue
		}
		if runs.RunState(ctx, attempt) != RunStateTerminal {
			continue
		}
		// One pod's delete error must not strand the rest of the batch.
		// Accumulate and keep going; the aggregated error is returned after the
		// loop, and `deleted` reflects every pod actually removed. (The sweep
		// also re-runs on every restart, so a pod that errors here is retried,
		// not lost.)
		if err := d.pods.DeletePod(ctx, pod.Namespace, pod.Name); err != nil {
			errs = append(errs, fmt.Errorf("dispatcher: delete orphaned stage pod %s/%s: %w", pod.Namespace, pod.Name, err))
			continue
		}
		deleted = append(deleted, pod.Name)
	}
	return deleted, errors.Join(errs...)
}

// podAttempt reads one pod's attempt identity back off its metadata. The
// verbatim run and stage come from the annotations (the labels are sanitized
// and cannot address a workflow); the ordinal comes from LabelAttempt, which
// is already exact. Any missing or unparseable piece makes the pod
// unaddressable, and an unaddressable pod is never disposed.
func podAttempt(pod *corev1.Pod) (PodAttempt, bool) {
	runID := pod.Annotations[AnnotationRunID]
	stage := pod.Annotations[AnnotationStage]
	if runID == "" || stage == "" {
		return PodAttempt{}, false
	}
	number, err := strconv.Atoi(pod.Labels[LabelAttempt])
	if err != nil {
		return PodAttempt{}, false
	}
	return PodAttempt{
		Pod:       pod.Name,
		Namespace: pod.Namespace,
		RunID:     runID,
		Stage:     stage,
		Attempt:   number,
	}, true
}

// sweepSelector selects exactly the pods THIS dispatcher stamps: its own
// managed-by marker, the stage role, and its owner.
func sweepSelector(owner string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		// The role label is the shared runnercap constant, so the sweep and
		// the stamp cannot drift.
		runnercap.LabelRole: runnercap.RoleStage,
		LabelOwner:          owner,
	}
}
