//go:build integration

package runner

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Drive the real branch executor after reopening the journal. The fixture
// installs a branch-local held checkout directly; public writable-parallel
// admission remains gated until workspace forks and fan-in are qualified. The
// serial fixture still reaches its expected join-routing refusal after the
// resumed stage succeeds; this proves recovery, not public parallel admission.
func TestIntegrationParallelChildWaitRestoresWithoutInterruptedClosure(t *testing.T) {
	testdep.Require(t, "git")
	for _, continued := range []bool{false, true} {
		name := "parked"
		if continued {
			name = "continued-before-invocation"
		}
		t.Run(name, func(t *testing.T) {
			r, run, frame, handoff, fork := prepareChildWaitRuntime(t)
			r.maxSteps = 100
			par := seedChildRecoveryParallel(t, run)
			frame.jr = &branchJournal{run: run, branch: 1}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := r.runTask(ctx, frame, 1, 1, "", "keep this branch guidance", nil, false, nil)
				done <- err
			}()
			select {
			case <-handoff.waiting:
			case err := <-done:
				t.Fatal("branch did not park", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			cancel()
			if err := <-done; !errors.Is(err, errChildWaitDrain) {
				t.Fatal("shutdown lost child wait", err)
			}
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			if continued {
				publishRecoveredBranchContinuation(t, &frame, events)
				handoff.reacquired = true
			}
			dir := run.Dir()
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, _, err := journal.Recover(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = recovered.Close() })
			events, err = reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			frame.in.parallelChild, err = r.parallelChildOwner(frame.in, par, events)
			if err != nil {
				t.Fatal(err)
			}
			frame.in.parallelSlot, _ = newParallelBranchSlots(1).tryAcquire()
			defer frame.in.parallelSlot.release()
			handoff.waiting = make(chan struct{})
			close(handoff.complete)
			registrar, _ := journal.DefaultScrubber()
			history := newParallelBranchEventIndex(events, par.spec.Name).events(1)
			result := r.runParallelBranch(t.Context(), recovered, par, frame.in, par.branchSnapshot(0), nil, "", apiv1.ResultEnvelope{}, nil, "", registrar, history, &atomic.Int64{})
			if result.err == nil || !strings.Contains(result.err.Error(), `completed the run instead of routing to "@join"`) || result.paused {
				t.Fatal("branch did not reach the expected serial-fixture routing boundary", result.err, result.status)
			}
			handoff.mu.Lock()
			envs := append([]apiv1.InvocationEnvelope(nil), handoff.envs...)
			handoff.mu.Unlock()
			if len(envs) != 2 || envs[1].InstructionAddendum != "keep this branch guidance" || envs[0].ChildWorkflowOrigin.StageOccurrence != envs[1].ChildWorkflowOrigin.StageOccurrence {
				t.Fatal("branch restart lost context or invocation identity", envs)
			}
			events, err = reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			succeeded := false
			for _, event := range events {
				if event.Branch == 1 && event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultSuccess) {
					succeeded = true
				}
				if event.Branch == 1 && event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultFailure) {
					t.Fatal("child wait charged an interrupted failure", event)
				}
			}
			if !succeeded {
				t.Fatal("recovered child continuation did not finish its stage")
			}
			fork.assertParentUnchanged(t)
		})
	}
}

func seedChildRecoveryParallel(t *testing.T, run *journal.Run) *parallelExec {
	t.Helper()
	par := newParallelExec(apiv1.Parallel{Name: "fan", Branches: []apiv1.Branch{{Name: "a", Start: "work"}, {Name: "b", Start: "other"}}})
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: par.spec.Name, Completeness: par.completeness()}); err != nil {
		t.Fatal(err)
	}
	branch, cursors := par.startBranch(0)
	run.SetBranchCursors(cursors)
	if err := run.Append(journal.Event{Type: journal.EventBranchStarted, Branch: 1, Parallel: par.spec.Name, BranchName: branch.name, Stage: branch.start}); err != nil {
		t.Fatal(err)
	}
	if err := settleConcurrentBranch(run, par, par.spec.Name, parallelBranchResult{index: 1, status: journal.BranchSucceeded}); err != nil {
		t.Fatal(err)
	}
	return par
}

func publishRecoveredBranchContinuation(t *testing.T, frame *taskFrame, events []journal.Event) {
	t.Helper()
	history := newParallelBranchEventIndex(events, "fan").events(1)
	record, marker, err := pendingChildWait(history)
	if err != nil || record == nil {
		t.Fatal("missing branch wait", err)
	}
	pointer, err := recordChildCompletion(frame, marker.Attempt, marker.AttemptClass, *record, ChildHandoffCompletion{State: "completed", Summary: "child complete"})
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: frame.t.Name, Attempt: marker.Attempt, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": record.Request.RequestID, "context": pointer}}); err != nil {
		t.Fatal(err)
	}
}
