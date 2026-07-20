package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
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

	resumed, warned, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.ReleaseReconciled, &wg)
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
	// documents — poll the run's own journal first instead of relying on wg
	// immediately.
	waitForRunPhase(t, l.RunsDir(), runID, journal.PhaseCompleted)
	// By the time the journal shows PhaseCompleted, wg.Add(1) has
	// unconditionally already run (it's the first line of Start, in the
	// same goroutine that later journals the terminal phase via s.r.Start)
	// — so the Add-race above no longer applies and this Wait is safe.
	// It's still necessary: trackedStarter.Start calls ingestRunTelemetry
	// (rollup DB writes under l.RunsDir()/l.SchedulerDir()) AFTER s.r.Start
	// returns but BEFORE the deferred wg.Done() fires (issue #320) — without
	// this Wait, the test can return and let t.TempDir()'s cleanup race that
	// still-in-flight ingest, producing the observed "directory not empty"
	// flake.
	wg.Wait()
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

	if _, _, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.ReleaseReconciled, &wg); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished && ev.RunID == "stuck-2" {
			finished = ev
		}
	}
	if finished.Status != string(journal.PhaseCompleted) {
		t.Fatalf("instance-log status = %q, want %q (the run's real outcome, not a hardcoded \"resumed\")", finished.Status, journal.PhaseCompleted)
	}
	if finished.Gaggle != "example" || finished.Workflow != "default-implement" {
		t.Fatalf("instance-log workflow identity = %q/%q, want example/default-implement", finished.Gaggle, finished.Workflow)
	}
}

// TestResumePastOrphanedWorktreeAtSameKey is issue #136's core fix
// end-to-end: a real leftover worktree directory at the exact key a resumed
// run's stage would reuse (RunID + "-" + stageName) — the signature of a
// mid-stage crash that never got to call Remove — must not make Resume fail
// forever with "already has a worktree"; worktree.Create's adopt-and-reset
// clears it and starts fresh.
func TestResumePastOrphanedWorktreeAtSameKey(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "stuck-orphan-1"
	newStuckRun(t, l, runID, "default-implement")

	// repoCloneURL is the same test-seam closure initDeterministicDemo just
	// installed — call it to get the identical fixture repo path, so the
	// orphaned worktree below is keyed against the exact repo the resumed
	// run's worktree.Manager will resolve to.
	fixtureRepo, err := repoCloneURL(apiv1.RepoRef{})
	if err != nil {
		t.Fatalf("repoCloneURL: %v", err)
	}

	wtMgr, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	orphanKey := runID + "-local-ci" // buildEnvelope's RunID+"-"+stageName convention
	if _, err := wtMgr.Create(context.Background(), worktree.CreateOptions{
		RepoURL: fixtureRepo, RunID: orphanKey, BaseRef: "main",
	}); err != nil {
		t.Fatalf("plant orphaned worktree: %v", err)
	}
	// Never call Remove — simulating the crash that left this worktree
	// behind mid-stage, exactly what issue #136 says makes resume fail
	// forever without the adopt-and-reset fix.

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		t.Fatal(err)
	}

	resumed, warned, err := resumeInterruptedRuns(context.Background(), l, setup.Runner, setup.Machines, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.ReleaseReconciled, &wg)
	if err != nil {
		t.Fatal(err)
	}
	if len(warned) != 0 {
		t.Fatalf("warned = %v, want none", warned)
	}
	if len(resumed) != 1 || resumed[0] != runID {
		t.Fatalf("resumed = %v, want [%s]", resumed, runID)
	}
	waitForRunPhase(t, l.RunsDir(), runID, journal.PhaseCompleted)
	// resumeInterruptedRuns' wg.Add(1) runs synchronously in its own loop,
	// before the resume goroutine launches (#320's fix comment above has the
	// full analysis for the Trigger-dispatch case) — no Add-race here, but
	// this Wait is still needed: the goroutine's ingestRunTelemetry call
	// (rollup DB writes under l.RunsDir()/l.SchedulerDir()) runs after the
	// journal already shows PhaseCompleted, so returning right after
	// waitForRunPhase can let t.TempDir()'s cleanup race that still-in-flight
	// write.
	wg.Wait()
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
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var recovery journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.RunID == "stale-1" {
			recovery = ev
		}
	}
	if recovery.Gaggle != "example" || recovery.Workflow != "no-such-workflow" {
		t.Fatalf("instance-log workflow identity = %q/%q, want example/no-such-workflow", recovery.Gaggle, recovery.Workflow)
	}
}

func TestUpSkipsRunFromRemovedGaggleWithWarningNotFatal(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	removed := l.ForGaggle("removed")

	jr, err := journal.Create(removed.RunsDir(), journal.RunIdentity{
		RunID: "removed-gaggle-run", Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "removed",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr.SetMachineState("local-ci")
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
			t.Fatalf("code = %d (the daemon must still start after gaggle removal), stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after ctx cancellation")
	}

	if !strings.Contains(stdout.String(), "warning: run removed-gaggle-run") {
		t.Fatalf("stdout = %q, want a warning naming the removed gaggle's run", stdout.String())
	}
	if strings.Contains(stderr.String(), "no runner configured") {
		t.Fatalf("stderr = %q, removed gaggle must not be fatal", stderr.String())
	}
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var recovery journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.RunID == "removed-gaggle-run" {
			recovery = ev
		}
	}
	if recovery.Gaggle != "removed" || recovery.Workflow != "default-implement" {
		t.Fatalf("instance-log workflow identity = %q/%q, want removed/default-implement", recovery.Gaggle, recovery.Workflow)
	}
	if recovery.Error == nil || recovery.Error.Code != "resume_unresolvable_gaggle" {
		t.Fatalf("instance-log recovery error = %+v, want resume_unresolvable_gaggle", recovery.Error)
	}
}

func TestResumeRetainedFlatRunUsesLegacyRuntime(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	newStuckRun(t, layout, runID, "default-implement")

	secondGaggleDir := filepath.Join(layout.ConfigDir(), "gaggles", "second")
	if err := os.MkdirAll(secondGaggleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secondGaggle := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: second
spec:
  project:
    provider: github
    owner: your-org
    name: your-repo
    branch: main
    connectionRef: repo-token
  backlog:
    provider: github
    project: your-org/your-repo
    connectionRef: repo-token
  isolation:
    namespace: gaggle-second
`
	if err := os.WriteFile(filepath.Join(secondGaggleDir, "gaggle.yaml"), []byte(secondGaggle), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	updatedManifest := strings.Replace(string(manifest), "    - example\n", "    - example\n    - second\n", 1)
	if updatedManifest == string(manifest) {
		t.Fatal("fixture manifest did not contain the example gaggle")
	}
	if err := os.WriteFile(manifestPath, []byte(updatedManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())
	if setup.LegacyRunner == nil || setup.LegacyWorktrees == nil {
		t.Fatal("retained flat runtime did not receive a legacy runner")
	}
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	runDirs, err := layout.RunDirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := sched.ReconcileAll(runDirs, time.Now()); err != nil {
		t.Fatal(err)
	}

	resumed, warned, err := resumeInterruptedRunsWithRunners(
		context.Background(), layout, setup.Runners, setup.LegacyRunner, setup.RunnerRegistry, setup.Machines,
		setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.ReleaseReconciled, &wg,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(warned) != 0 || len(resumed) != 1 || resumed[0] != runID {
		t.Fatalf("resumed=%v warned=%v, want [%s] and no warnings", resumed, warned, runID)
	}
	waitForRunPhase(t, layout.RunsDir(), runID, journal.PhaseCompleted)
	wg.Wait()

	if _, err := os.Stat(filepath.Join(layout.RunsDir(), runID, "spans", "spans.jsonl")); err != nil {
		t.Fatalf("retained run spans were not written beside the flat journal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layout.ForGaggle("example").RunsDir(), runID)); !os.IsNotExist(err) {
		t.Fatalf("retained run was copied into the scoped root: %v", err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.RunID == runID && event.Error != nil && event.Error.Code == "telemetry_ingest_run_failed" {
			t.Fatalf("retained run telemetry used the wrong journal root: %s", event.Error.Message)
		}
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

	code, stdout, stderr := runArgs(t, "run", "abort", "stuck", root)
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

func TestRunAbortRejectsAmbiguousRunIDPrefix(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const (
		first  = "dd57a3c2aaaaaaaaaaaaaaaaaaaaaaaa"
		second = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
	)
	for _, runID := range []string{first, second} {
		run, err := journal.Create(l.RunsDir(), journal.RunIdentity{
			RunID:           runID,
			Workflow:        "no-such-workflow",
			WorkflowVersion: 1,
			Gaggle:          "example",
			Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		}, nil)
		if err != nil {
			t.Fatalf("create run %q: %v", runID, err)
		}
		if err := run.Close(); err != nil {
			t.Fatalf("close run %q: %v", runID, err)
		}
	}

	code, stdout, stderr := runArgs(t, "run", "abort", "dd57a3c2", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	want := `error: ambiguous prefix "dd57a3c2" matches 2 runs: ` + first + ", " + second + "\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
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
