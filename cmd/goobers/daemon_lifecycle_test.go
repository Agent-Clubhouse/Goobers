package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

// newStuckRun hand-constructs a run left non-terminal (task-checkpointed, no
// run.finished event) for the given workflow — the same "prior crash or
// unclean shutdown" fixture shape TestUpResumesInterruptedRun uses, factored
// out for the #135 tests that build their own Scheduler/runner directly
// rather than going through a full runUpContext.
func newStuckRun(t *testing.T, l instance.Layout, runID, workflowName string) {
	t.Helper()
	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	var gaggle, digest string
	found := false
	for i := range set.Workflows {
		if set.Workflows[i].Name == workflowName {
			m, err := workflow.Compile(workflow.Definition{Name: set.Workflows[i].Name, Version: 1, Spec: set.Workflows[i].Spec})
			if err != nil {
				t.Fatalf("compile fixture workflow: %v", err)
			}
			gaggle = set.Workflows[i].Spec.Gaggle
			digest = m.Digest()
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("workflow %q not found in fixture config", workflowName)
	}

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: workflowName, WorkflowVersion: 1,
		WorkflowDigest: digest, Gaggle: gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("hand-construct stuck run journal: %v", err)
	}
	jr.SetMachineState("local-ci")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestResumeReleasesReconciledSlotForFollowUpTrigger is issue #135's core
// fix (points 1 and 5): Reconcile seeds Conditions' active count from a
// stuck run so it's counted while genuinely being resumed, but that slot
// MUST come back down once the resume finishes — otherwise the workflow
// starves for the rest of the daemon's life (the exact bug: nothing used to
// call Release for a reconciled slot at all).
func TestResumeReleasesReconciledSlotForFollowUpTrigger(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	newStuckRun(t, l, "stuck-1", "default-implement")

	ctx := context.Background()
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		t.Fatal(err)
	}

	resumed, warned, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.RepoRefs, setup.InstanceLog, setup.RollupDB, sched.Release, &wg)
	if err != nil {
		t.Fatal(err)
	}
	if len(warned) != 0 {
		t.Fatalf("warned = %v, want none", warned)
	}
	if len(resumed) != 1 || resumed[0] != "stuck-1" {
		t.Fatalf("resumed = %v, want [stuck-1]", resumed)
	}
	wg.Wait() // let the resumed run's goroutine finish and Release its slot

	// Without issue #135's fix this Trigger would be wrongly rejected forever
	// (maxConcurrentRuns defaults to 1, and Reconcile's seeded slot for
	// stuck-1 would never have been released).
	runID, err := sched.Trigger(ctx, "default-implement", time.Now())
	if err != nil {
		t.Fatalf("Trigger rejected after the resumed run should have released its slot: %v", err)
	}
	if runID == "" {
		t.Fatal("expected a dispatched run id")
	}
	// dispatch's own goroutine calls trackedStarter.Start (and its wg.Add)
	// from within itself, so wg.Wait() right after Trigger returns has the
	// same tiny race window trackedStarter's own doc comment already
	// documents — poll the run's own journal instead of relying on wg here,
	// so the test doesn't tear down its TempDir while that goroutine (and
	// its worktree cleanup) is still in flight.
	waitForRunPhase(t, l.RunsDir(), runID, journal.PhaseCompleted)
}

// waitForRunPhase polls runID's journal until it reaches want, failing the
// test if it doesn't within a few seconds — used instead of wg.Wait() where
// trackedStarter's documented Add/Done race window would make wg.Wait()
// unreliable as a completion signal.
func waitForRunPhase(t *testing.T, runsDir, runID string, want journal.RunPhase) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if reader, err := journal.OpenRead(dir); err == nil {
			if st, err := reader.State(); err == nil && st.Phase == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach phase %s in time", runID, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestResumeJournalsActualPhaseNotHardcodedStatus is issue #135 point 4:
// resumeInterruptedRuns used to journal every resumed run's outcome as the
// literal string "resumed" regardless of its actual phase, breaking the
// status=phase convention dispatch's own goroutine follows for a fresh run.
func TestResumeJournalsActualPhaseNotHardcodedStatus(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	newStuckRun(t, l, "stuck-2", "default-implement")

	ctx := context.Background()
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, _, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.RepoRefs, setup.InstanceLog, setup.RollupDB, sched.Release, &wg); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var status string
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished && ev.RunID == "stuck-2" {
			status = ev.Status
		}
	}
	if status != string(journal.PhaseCompleted) {
		t.Fatalf("instance-log status = %q, want %q (the run's real outcome, not a hardcoded \"resumed\")", status, journal.PhaseCompleted)
	}
}

// TestUpSkipsUnresolvableWorkflowWithWarningNotFatal is issue #135 point 2's
// "unbootable daemon" fix: a stuck run referencing a workflow renamed or
// removed from config must not prevent `goobers up` from starting at all —
// it's skipped with a warning instead.
func TestUpSkipsUnresolvableWorkflowWithWarningNotFatal(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: "stale-1", Workflow: "no-such-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr.SetMachineState("whatever-state")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, &stdout, &stderr) }()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d (the daemon must still start despite the stale run), stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after ctx cancellation")
	}

	if !strings.Contains(stdout.String(), "warning: run stale-1") {
		t.Fatalf("stdout = %q, want a warning naming the unresolvable run", stdout.String())
	}
	if !strings.Contains(stdout.String(), "goobers run abort stale-1") {
		t.Fatalf("stdout = %q, want the warning to point at the abort recovery path", stdout.String())
	}
}

// TestRunAbortMarksRunTerminal and its siblings cover issue #135's
// `goobers run abort <run-id>` recovery path.
func TestRunAbortMarksRunTerminal(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: "stuck-3", Workflow: "no-such-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr.SetMachineState("whatever-state")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "run", "abort", "stuck-3", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "aborted run stuck-3") {
		t.Fatalf("stdout = %q", stdout)
	}

	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), "stuck-3"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want %q", st.Phase, journal.PhaseAborted)
	}
}

func TestRunAbortRejectsAlreadyTerminalRun(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: "done-1", Workflow: "no-such-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "run", "abort", "done-1", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "already terminal") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunAbortUnknownRun(t *testing.T) {
	root := initDeterministicDemo(t)
	code, _, stderr := runArgs(t, "run", "abort", "no-such-run", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2, stderr = %q", code, stderr)
	}
}
