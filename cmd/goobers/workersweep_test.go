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

func sweepAttempt() dispatcher.PodAttempt {
	return dispatcher.PodAttempt{
		Pod: "gbn-open-pr-run1-a2", Namespace: "gaggle-e2e",
		RunID: "run-1", Stage: "open-pr", Attempt: 2,
	}
}

// A pod whose per-attempt DispatchOne workflow is still Running is ADOPTED:
// the worker restarted underneath a live stage, and the pod is still writing.
// This is the case decision 003 named when it rejected the fail-closed-toward-
// deletion sweep.
func TestTemporalRunStatesAdoptsRunningAttempt(t *testing.T) {
	attempt := sweepAttempt()
	id := engine.DispatchOneWorkflowID(attempt.RunID, attempt.Stage, int32(attempt.Attempt))
	describer := &fakeSweepDescriber{status: map[string]enumspb.WorkflowExecutionStatus{
		id: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
	}}
	if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateLive {
		t.Fatalf("RunState = %v, want RunStateLive — a Running attempt must be adopted, not disposed", got)
	}
}

// The engine-start shape: no DispatchOne workflow exists, but the run's own
// Run workflow is executing and its walk owns this pod. Also adopted.
func TestTemporalRunStatesAdoptsRunningEngineRun(t *testing.T) {
	attempt := sweepAttempt()
	describer := &fakeSweepDescriber{status: map[string]enumspb.WorkflowExecutionStatus{
		attempt.RunID: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
	}}
	if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateLive {
		t.Fatalf("RunState = %v, want RunStateLive for a live engine-driven run", got)
	}
}

// Both identities settled — closed, or never existed — is the only disposal.
func TestTemporalRunStatesDisposesSettledAttempt(t *testing.T) {
	for name, status := range map[string]enumspb.WorkflowExecutionStatus{
		"completed": enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		"failed":    enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		"timed out": enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
	} {
		t.Run(name, func(t *testing.T) {
			attempt := sweepAttempt()
			id := engine.DispatchOneWorkflowID(attempt.RunID, attempt.Stage, int32(attempt.Attempt))
			describer := &fakeSweepDescriber{status: map[string]enumspb.WorkflowExecutionStatus{
				id: status, attempt.RunID: status,
			}}
			if got := (temporalRunStates{client: describer}).RunState(context.Background(), attempt); got != dispatcher.RunStateTerminal {
				t.Fatalf("RunState = %v, want RunStateTerminal for a %s attempt", got, name)
			}
		})
	}
	t.Run("no such workflow", func(t *testing.T) {
		// Empty table: every describe answers NotFound, which is an ANSWER —
		// nothing on the engine is addressable as this attempt.
		if got := (temporalRunStates{client: &fakeSweepDescriber{}}).RunState(context.Background(), sweepAttempt()); got != dispatcher.RunStateTerminal {
			t.Fatalf("RunState = %v, want RunStateTerminal when no workflow exists under either id", got)
		}
	})
}

// The rule the whole graft exists for: an unreachable engine LEAVES the pod.
// Never delete on uncertainty — the pod's activeDeadlineSeconds reclaims it.
func TestTemporalRunStatesLeavesPodWhenTemporalUnreachable(t *testing.T) {
	attempt := sweepAttempt()
	id := engine.DispatchOneWorkflowID(attempt.RunID, attempt.Stage, int32(attempt.Attempt))
	// The per-attempt id is unreachable; the run id answers NotFound. A sweep
	// that counted NotFound alone would dispose a pod whose real owner it never
	// managed to ask about.
	describer := &fakeSweepDescriber{unavailable: map[string]bool{id: true}}
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
