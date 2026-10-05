//go:build integration

package runner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type laneCapacityFixture struct {
	handoff  *childHandoffFixture
	suspends atomic.Int32
	resumes  atomic.Int32
}

func (f *laneCapacityFixture) SuspendChildParent(context.Context, string) (ChildParentSuspension, error) {
	f.suspends.Add(1)
	return f, nil
}
func (f *laneCapacityFixture) Resume(context.Context) error {
	f.resumes.Add(1)
	f.handoff.mu.Lock()
	f.handoff.reacquired = true
	f.handoff.mu.Unlock()
	return nil
}

// Actual invocation cancellation/join, durable wait, worktree adoption and
// continuation run through runTask. The queued sibling only exercises lane
// ownership here; writable branch fork/fan-in is qualified separately.
func TestIntegrationParallelChildWaitReleasesLaneAndRetainsOccurrence(t *testing.T) {
	for _, recoverWait := range []bool{false, true} {
		name := "live"
		if recoverWait {
			name = "recover"
		}
		t.Run(name, func(t *testing.T) {
			r, run, frame, handoff, fork := prepareChildWaitRuntime(t)
			capacity := &laneCapacityFixture{handoff: handoff}
			r.cfg.ChildParentCapacity = capacity
			p := apiv1.Parallel{Name: "fan", MaxConcurrentBranches: 1, Branches: []apiv1.Branch{{Name: "a", Start: "work"}, {Name: "b", Start: "other"}}}
			par := newParallelExec(p)
			if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: p.Name, Completeness: par.completeness()}); err != nil {
				t.Fatal(err)
			}
			aggregate, err := newParallelChildCapacity(frame.in.RunID, capacity, []int{1, 2}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			runtime := &parallelChildRuntime{capacity: aggregate, lanes: make(chan struct{}, 1), branches: map[int]*parallelChildLane{}}
			for _, branch := range []int{1, 2} {
				runtime.branches[branch] = &parallelChildLane{runtime: runtime, branch: branch}
			}
			recorder := &branchJournal{run: run, branch: 1, setMachineState: func(string) {}}
			frame.jr = recorder
			frame.ex = newExecutors(r.cfg, recorder, journal.NewRegistryScrubber())
			frame.in.parallelChild = runtime.branches[1]
			if _, err := startConcurrentBranch(run, par, p, 0); err != nil {
				t.Fatal(err)
			}
			if err := frame.in.parallelChild.begin(t.Context()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer frame.in.parallelChild.release()
				_, _, err := r.runTask(ctx, frame, 1, 1, "", "", nil, false, nil)
				done <- err
			}()
			sibling := make(chan parallelBranchResult, 1)
			go func() {
				sibling <- runtime.runBranch(ctx, run, par, p, 1, func(branch branchState, _ *parallelChildLane) parallelBranchResult {
					return parallelBranchResult{index: 1, status: journal.BranchSucceeded}
				})
			}()
			select {
			case <-handoff.waiting:
			case err := <-done:
				t.Fatalf("ended before wait: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("parent did not wait")
			}
			clockReader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if expired, err := r.parallelBranchExpired(t.Context(), clockReader, par.branchSnapshot(0), 300, time.Now().Add(24*time.Hour)); err != nil || expired {
				t.Fatal("parked branch consumed execution deadline", expired, err)
			}
			var other parallelBranchResult
			select {
			case other = <-sibling:
			case <-time.After(10 * time.Second):
				t.Fatal("waiting branch consumed queued sibling's lane")
			}
			if other.err != nil || capacity.suspends.Load() != 0 {
				t.Fatal("queued sibling lost whole-run permit", other.err, capacity.suspends.Load())
			}
			if err := finishConcurrentBranch(t.Context(), runtime, run, journal.Event{Type: journal.EventBranchFinished, Parallel: p.Name, Branch: 2, BranchStatus: journal.BranchSucceeded}); err != nil {
				t.Fatal(err)
			}
			if capacity.suspends.Load() != 1 {
				t.Fatal("last runnable sibling did not suspend run")
			}
			if recoverWait {
				cancel()
				if err := <-done; !errors.Is(err, errChildWaitDrain) {
					t.Fatal("shutdown lost durable wait", err)
				}
				reader, _ := journal.OpenReadOnly(run.Dir())
				events, _ := reader.Events()
				history := newParallelBranchEventIndex(events, p.Name).events(1)
				restored, ok := recoverChildTaskContext(history, "work")
				if !ok || restored.childWaitErr != nil || restored.childWait == nil {
					t.Fatal("branch wait recovery failed", restored)
				}
				aggregate, err = newParallelChildCapacity(frame.in.RunID, capacity, []int{1, 2}, []int{1}, []int{2})
				if err != nil {
					t.Fatal(err)
				}
				runtime.capacity = aggregate
				frame.in.parallelChild = &parallelChildLane{runtime: runtime, branch: 1, parked: true}
				frame.childWaitResume, frame.childWaitAttempt, frame.childWaitClass = restored.childWait, restored.attempt, restored.class
				handoff.waiting = make(chan struct{})
				ctx, cancel = context.WithCancel(t.Context())
				defer cancel()
				go func() {
					defer frame.in.parallelChild.release()
					_, _, err := r.runTask(ctx, frame, 1, 2, restored.class, "", nil, false, &resumeRetryAccounting{policyAttempts: restored.childWait.PolicyAttempts, infrastructureFailures: restored.childWait.InfrastructureFailures, replacementConsumesPolicy: true})
					done <- err
				}()
				select {
				case <-handoff.waiting:
				case err := <-done:
					t.Fatalf("recovery ended before wait: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("recovery did not wait")
				}
			}
			close(handoff.complete)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("parent did not continue")
			}
			if capacity.resumes.Load() != 1 {
				t.Fatal("continuation did not reacquire exactly once")
			}
			handoff.mu.Lock()
			envs := append([]apiv1.InvocationEnvelope(nil), handoff.envs...)
			handoff.mu.Unlock()
			if len(envs) != 2 || envs[0].ChildWorkflowOrigin.StageOccurrence != envs[1].ChildWorkflowOrigin.StageOccurrence || envs[0].ChildWorkflowOrigin.AttemptID == envs[1].ChildWorkflowOrigin.AttemptID {
				t.Fatal("branch continuation changed logical occurrence", envs)
			}
			reader, _ := journal.OpenReadOnly(run.Dir())
			events, _ := reader.Events()
			projection, err := journal.ProjectChildWaits(events)
			if err != nil || len(projection.Waits) != 0 {
				t.Fatal("continuation failed to close branch wait", err)
			}
			for _, event := range events {
				if event.Type == journal.EventError || event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultFailure) {
					t.Fatal("child wait consumed failure allowance", event)
				}
			}
			fork.assertParentUnchanged(t)
		})
	}
}
