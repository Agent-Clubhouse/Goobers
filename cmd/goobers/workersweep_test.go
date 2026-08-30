package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
)

// fakeSweepDescriber answers DescribeWorkflowExecution from a table keyed by
// workflow id. An id absent from the table answers NotFound; an id in
// unavailable answers a transport failure, the "Temporal is unreachable" case.
type fakeSweepDescriber struct {
	status      map[string]enumspb.WorkflowExecutionStatus
	unavailable map[string]bool
	asked       []string
}

func (f *fakeSweepDescriber) DescribeWorkflowExecution(_ context.Context, workflowID, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.asked = append(f.asked, workflowID)
	if f.unavailable[workflowID] {
		return nil, serviceerror.NewUnavailable("frontend is cycling")
	}
	status, ok := f.status[workflowID]
	if !ok {
		return nil, serviceerror.NewNotFound("no such workflow")
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}, nil
}

// sweepAttempt is the RUNNER-DRIVEN shape: DispatchOne owns the pod, so the
// stamped driver happens to equal <run>/<stage>/<attempt>.
func sweepAttempt() dispatcher.PodAttempt {
	return dispatcher.PodAttempt{
		Pod: "gbn-open-pr-run1-a2", Namespace: "gaggle-e2e",
		OwningWorkflowID: engine.DispatchOneWorkflowID("run-1", "open-pr", 2),
		RunID:            "run-1", Stage: "open-pr", Attempt: 2,
	}
}

// scheduledSweepAttempt is the shape review 3 caught the first cut deleting
// alive, and the reason the driver is stamped rather than composed.
//
// ClaimScheduled starts the run's child workflow as claimID+"-run" and
// RunScheduled then rewrites the run's RunID to RunID(claimID), a sha256
// prefix (internal/engine/liveness.go: "a hash describe can never find").
// The pod therefore carries a run id that is NOT a workflow id, its stage is
// dispatched by the run's own walk so no DispatchOne execution exists either,
// and every id composable from this attempt answers NotFound.
func scheduledSweepAttempt() dispatcher.PodAttempt {
	claimID := "goobers-e2e-nightly-2026-08-29T03:00:00Z"
	return dispatcher.PodAttempt{
		Pod: "gbn-open-pr-e9791a88-a1", Namespace: "gaggle-e2e",
		OwningWorkflowID: claimID + "-run",
		RunID:            engine.RunID(claimID), Stage: "open-pr", Attempt: 1,
	}
}

// A pod whose driving workflow is still Running is ADOPTED: the worker
// restarted underneath a live stage, and the pod is still writing. This is the
// case decision 003 named when it rejected the fail-closed-toward-deletion
// sweep.
func TestTemporalRunStatesAdoptsRunningAttempt(t *testing.T) {
	for name, attempt := range map[string]dispatcher.PodAttempt{
		"runner-driven (DispatchOne)": sweepAttempt(),
		"engine walk (scheduled run)": scheduledSweepAttempt(),
	} {
		t.Run(name, func(t *testing.T) {
			describer := &fakeSweepDescriber{status: map[string]enumspb.WorkflowExecutionStatus{
				attempt.OwningWorkflowID: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			}}
			if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateLive {
				t.Fatalf("RunState = %v, want RunStateLive — a Running attempt must be adopted, not disposed", got)
			}
		})
	}
}

// The regression this rewrite exists for, stated as its own test rather than
// left implicit in the one above: a LIVE scheduled run's stage pod.
//
// The describer knows only the run's real workflow. Every id the attempt's
// run/stage/attempt can compose is absent from the table and answers NotFound,
// so the resolver this replaced counted two NotFounds, called the attempt
// settled, and had SweepOrphans delete a pod whose open-pr stage was still
// executing. Asking the stamped driver is the whole difference.
func TestTemporalRunStatesAdoptsLiveScheduledRunNoComposedIDCanFind(t *testing.T) {
	attempt := scheduledSweepAttempt()
	describer := &fakeSweepDescriber{status: map[string]enumspb.WorkflowExecutionStatus{
		attempt.OwningWorkflowID: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
	}}
	// Guard the fixture: if either composed id ever became findable, this test
	// would pass without the driver being consulted at all.
	for _, composed := range []string{
		engine.DispatchOneWorkflowID(attempt.RunID, attempt.Stage, int32(attempt.Attempt)),
		attempt.RunID,
	} {
		if _, found := describer.status[composed]; found {
			t.Fatalf("composed id %q is findable in the fixture — the scheduled shape is precisely that it is not", composed)
		}
	}
	if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateLive {
		t.Fatalf("RunState = %v for a LIVE scheduled run's stage pod, want RunStateLive — disposing it deletes an executing mutating stage, invisibly", got)
	}
	if len(describer.asked) != 1 || describer.asked[0] != attempt.OwningWorkflowID {
		t.Fatalf("resolver asked %v, want exactly the stamped driver %q — every other id is a guess", describer.asked, attempt.OwningWorkflowID)
	}
}

// The driving workflow settled — closed, or no such execution — is the only
// disposal.
func TestTemporalRunStatesDisposesSettledAttempt(t *testing.T) {
	for name, status := range map[string]enumspb.WorkflowExecutionStatus{
		"completed": enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		"failed":    enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		"timed out": enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
	} {
		t.Run(name, func(t *testing.T) {
			attempt := sweepAttempt()
			describer := &fakeSweepDescriber{status: map[string]enumspb.WorkflowExecutionStatus{
				attempt.OwningWorkflowID: status,
			}}
			if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateTerminal {
				t.Fatalf("RunState = %v, want RunStateTerminal for a %s attempt", got, name)
			}
		})
	}
	// NotFound settles ONLY because the id asked is the pod's stamped driver —
	// the execution whose activity created it. That is a real answer about this
	// attempt: nothing is driving it.
	//
	// It is emphatically not the old "no such workflow" subtest, which fed an
	// EMPTY table so that every COMPOSED id answered NotFound and asserted
	// disposal — encoding the scheduled-run bug as intended behaviour. Here the
	// table is empty for the driver itself, and no other id is consulted.
	t.Run("no such workflow under the stamped driver", func(t *testing.T) {
		attempt := sweepAttempt()
		describer := &fakeSweepDescriber{}
		if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateTerminal {
			t.Fatalf("RunState = %v, want RunStateTerminal when the driving execution does not exist", got)
		}
		if len(describer.asked) != 1 || describer.asked[0] != attempt.OwningWorkflowID {
			t.Fatalf("resolver asked %v, want exactly [%q]", describer.asked, attempt.OwningWorkflowID)
		}
	})
}

// The rule the whole graft exists for: an unreachable engine LEAVES the pod.
// Never delete on uncertainty — the pod's activeDeadlineSeconds reclaims it.
func TestTemporalRunStatesLeavesPodWhenTemporalUnreachable(t *testing.T) {
	attempt := sweepAttempt()
	describer := &fakeSweepDescriber{unavailable: map[string]bool{attempt.OwningWorkflowID: true}}
	if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateIndeterminate {
		t.Fatalf("RunState = %v, want RunStateIndeterminate — an unreachable engine must never authorise a delete", got)
	}
}

// No client at all is the same uncertainty, not a licence to delete.
func TestTemporalRunStatesLeavesPodWithoutClient(t *testing.T) {
	if got := (temporalRunStates{}).RunState(context.Background(), sweepAttempt()); got != dispatcher.RunStateIndeterminate {
		t.Fatalf("RunState = %v, want RunStateIndeterminate with no client", got)
	}
}

// An attempt with no stamped driver is never disposed and is never described.
// dispatcher.podAttempt already refuses such a pod, so this is the second of
// two guards: describing "" would earn a NotFound that authorises a delete
// backed by no evidence whatever.
func TestTemporalRunStatesLeavesPodWithoutStampedDriver(t *testing.T) {
	attempt := sweepAttempt()
	attempt.OwningWorkflowID = ""
	describer := &fakeSweepDescriber{}
	if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateIndeterminate {
		t.Fatalf("RunState = %v, want RunStateIndeterminate for an attempt naming no driver", got)
	}
	if len(describer.asked) != 0 {
		t.Fatalf("resolver described %v for an attempt with no driver; there is no id to ask about", describer.asked)
	}
}

// recordingSweeper captures the resolver the wiring hands to SweepOrphans.
type recordingSweeper struct {
	disposed []string
	err      error
	states   dispatcher.RunStates
}

func (r *recordingSweeper) SweepOrphans(_ context.Context, states dispatcher.RunStates) ([]string, error) {
	r.states = states
	return r.disposed, r.err
}

// The boot wiring: the worker sweeps with a Temporal-backed resolver and
// reports what it disposed.
func TestSweepWorkerStageOrphansReportsDisposal(t *testing.T) {
	sweeper := &recordingSweeper{disposed: []string{"gbn-open-pr-run1-a2"}}
	withFakeSweepDial(t, &fakeSweepDescriber{})
	var stdout, stderr bytes.Buffer
	sweepWorkerStageOrphans(sweeper, "127.0.0.1:7233", "default", &stdout, &stderr)
	states, ok := sweeper.states.(temporalRunStates)
	if !ok {
		t.Fatalf("sweep ran with resolver %T, want temporalRunStates — a sweep with any other basis is not asking the engine", sweeper.states)
	}
	// The resolver must be HOLDING the dialed client. A clientless
	// temporalRunStates is the right type and answers Indeterminate to
	// everything, which looks safe and quietly means the sweep never reclaims
	// anything.
	if states.client == nil {
		t.Fatal("the sweep's resolver holds no Temporal client — it would answer Indeterminate for every pod and reclaim nothing")
	}
	if !strings.Contains(stdout.String(), "gbn-open-pr-run1-a2") {
		t.Fatalf("stdout %q does not name the disposed pod", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr %q", stderr.String())
	}
}

// Hygiene must never keep a worker from serving: a dial failure is reported
// and the worker proceeds, and so is a sweep error.
func TestSweepWorkerStageOrphansIsNeverFatal(t *testing.T) {
	t.Run("dial fails", func(t *testing.T) {
		previous := dialWorkerSweepTemporal
		dialWorkerSweepTemporal = func(string, string) (client.Client, error) {
			return nil, errors.New("connection refused")
		}
		t.Cleanup(func() { dialWorkerSweepTemporal = previous })
		var stdout, stderr bytes.Buffer
		sweepWorkerStageOrphans(&recordingSweeper{}, "127.0.0.1:7233", "default", &stdout, &stderr)
		if !strings.Contains(stderr.String(), "orphan sweep skipped") {
			t.Fatalf("stderr %q does not report the skipped sweep", stderr.String())
		}
	})
	t.Run("sweep errors", func(t *testing.T) {
		withFakeSweepDial(t, &fakeSweepDescriber{})
		var stdout, stderr bytes.Buffer
		sweepWorkerStageOrphans(&recordingSweeper{err: errors.New("apiserver conflict")}, "127.0.0.1:7233", "default", &stdout, &stderr)
		if !strings.Contains(stderr.String(), "apiserver conflict") {
			t.Fatalf("stderr %q does not report the sweep failure", stderr.String())
		}
	})
	t.Run("no dispatcher", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		sweepWorkerStageOrphans(nil, "127.0.0.1:7233", "default", &stdout, &stderr)
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("a worker with no dispatcher must not sweep at all; stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})
}

// sweepStubClient is a client.Client that answers only the call the sweep
// makes and inherits panics for everything else, so a sweep that starts using
// the connection for something new has to say so here.
type sweepStubClient struct {
	client.Client
	describer *fakeSweepDescriber
}

func (c *sweepStubClient) DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return c.describer.DescribeWorkflowExecution(ctx, workflowID, runID)
}

func (c *sweepStubClient) Close() {}

// withFakeSweepDial substitutes a Temporal client whose describe surface is
// the given fake and whose Close is a no-op.
func withFakeSweepDial(t *testing.T, describer *fakeSweepDescriber) {
	t.Helper()
	previous := dialWorkerSweepTemporal
	dialWorkerSweepTemporal = func(string, string) (client.Client, error) {
		return &sweepStubClient{describer: describer}, nil
	}
	t.Cleanup(func() { dialWorkerSweepTemporal = previous })
}
