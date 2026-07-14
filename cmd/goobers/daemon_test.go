package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// newDaemonFixtureRepo creates a local bare git repo, mirroring
// test/e2e/walking_skeleton_test.go's fixture — so daemon/run integration
// tests need no network access.
func newDaemonFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runFixtureGit(t, work, "init", "--initial-branch=main")
	runFixtureGit(t, work, "config", "user.email", "test@example.com")
	runFixtureGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, work, "add", "README.md")
	runFixtureGit(t, work, "commit", "-m", "initial")
	runFixtureGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runFixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
}

// deterministicWorkflowYAML replaces the demo scaffold's agentic
// default-implement workflow with a deterministic-only one, so daemon/run
// integration tests need neither a real Copilot CLI installation nor network
// access — only a local git fixture (via the repoCloneURL test seam,
// runnerwiring.go) and the `true` binary.
const deterministicWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: backlog-item
      selector:
        goobers: "true"
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a no-op local command
      run:
        command: ["true"]
`

// initDeterministicDemo scaffolds an instance via `goobers init`, then swaps
// its starter workflow for one with a single deterministic task and drops
// the starter's agentic goober entirely, so tests exercise the real
// runner/scheduler wiring (issue #23) without a Copilot CLI or network
// access. It also points the repoCloneURL test seam at a local bare git
// fixture instead of a real GitHub clone, restored via t.Cleanup.
func initDeterministicDemo(t *testing.T) string {
	t.Helper()
	root := initDemo(t)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(deterministicWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "config", "gaggles", "example", "goobers")); err != nil {
		t.Fatal(err)
	}

	fixtureRepo := newDaemonFixtureRepo(t)
	prev := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	t.Cleanup(func() { repoCloneURL = prev })

	return root
}

// TestUpIdlesThenDrainsOnCancel is issue #23's core daemon-loop acceptance:
// `goobers up` starts the scheduler+runner daemon, and a cancelled context
// (standing in for SIGINT/SIGTERM — runUp itself wires the real signal via
// internal/signals) drains cleanly and returns 0 rather than hanging. The
// deterministic demo's only workflow has a backlog-item trigger, not a
// schedule trigger, so the scheduler has nothing to dispatch and simply
// idles — proving the idle path doesn't busy-loop or block shutdown.
func TestUpIdlesThenDrainsOnCancel(t *testing.T) {
	root := initDeterministicDemo(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, &stdout, &stderr) }()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after ctx cancellation")
	}

	if !strings.Contains(stdout.String(), "daemon started") {
		t.Fatalf("stdout = %q, want daemon-started message", stdout.String())
	}
	if !strings.Contains(stdout.String(), "shutdown complete") {
		t.Fatalf("stdout = %q, want clean-shutdown message", stdout.String())
	}
}

// TestUpResumesInterruptedRun is issue #23's crash-resume acceptance: a run
// left non-terminal (state.json checkpointed at a task, no run.finished
// event — the signature of a prior crash or unclean shutdown, per
// resumeInterruptedRuns' doc comment) restarts via Runner.Resume the next
// time `goobers up` starts, rather than being silently ignored.
func TestUpResumesInterruptedRun(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	var wf *apiv1.Workflow
	for i := range set.Workflows {
		if set.Workflows[i].Name == "default-implement" {
			wf = &set.Workflows[i]
		}
	}
	if wf == nil {
		t.Fatal("default-implement workflow not found in fixture config")
	}
	machine, err := workflow.Compile(workflow.Definition{Name: wf.Name, Version: 1, Spec: wf.Spec})
	if err != nil {
		t.Fatalf("compile fixture workflow: %v", err)
	}

	const runID = "interrupted-run-1"
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        wf.Name,
		WorkflowVersion: 1,
		WorkflowDigest:  machine.Digest(),
		Gaggle:          wf.Spec.Gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("hand-construct interrupted run journal: %v", err)
	}
	jr.SetMachineState("local-ci")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(800*time.Millisecond, cancel) // give the resumed task time to actually dispatch and finish

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, &stdout, &stderr) }()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after ctx cancellation")
	}

	if !strings.Contains(stdout.String(), "resuming interrupted run "+runID) {
		t.Fatalf("stdout = %q, want a mention of the resumed run", stdout.String())
	}

	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != journal.PhaseCompleted {
		t.Fatalf("resumed run phase = %q, want %q (Resume should have driven the single deterministic task to completion)", st.Phase, journal.PhaseCompleted)
	}
}

// The single-instance lock itself (#23 AC3) is unaffected by the daemon-loop
// rewrite and already covered by lock_test.go's TestUpFailsFastOnSecondInstance.

// TestRunFailsFastWhenLockHeld is issue #134's lock half: `goobers run` used
// to skip the instance lock entirely, so two concurrent processes (or a
// manual run against a live `up` daemon) could mutate scheduler/run-condition
// state and the shared workcopies/ tree at once. Now it takes the same lock
// `up` does and fails fast, mirroring TestUpFailsFastOnSecondInstance.
func TestRunFailsFastWhenLockHeld(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	code, _, stderr := runArgs(t, "run", "default-implement", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "already holds the lock") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestRunAppearsInInstanceJournalAsManual is issue #134's other acceptance
// criterion: a manual `goobers run` must be visible in the instance journal
// (scheduler/events.jsonl) — previously it called Runner.Start directly and
// left no trace there at all — tagged "manual", never "scheduled" (the
// fireReason mislabeling bug the issue also calls out).
func TestRunAppearsInInstanceJournalAsManual(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("run stdout = %q", stdout)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var sawManualFire, sawRunStarted bool
	for _, ev := range events {
		if ev.Workflow != "default-implement" {
			continue
		}
		if ev.Type == journal.EventTriggerFired && ev.Reason == "manual" {
			sawManualFire = true
		}
		if ev.Type == journal.EventRunStarted {
			sawRunStarted = true
		}
	}
	if !sawManualFire {
		t.Fatalf("expected a trigger.fired(reason=manual) event in the instance journal: %+v", events)
	}
	if !sawRunStarted {
		t.Fatalf("expected a run.started event in the instance journal: %+v", events)
	}
}

// TestRunRejectedOverMaxConcurrentRuns is issue #134's admission-limit
// acceptance criterion at the CLI level: with maxConcurrentRuns already
// exhausted by a run the scheduler's own Conditions tracks as active (seeded
// via Reconcile from a hand-built in-flight run, mirroring
// TestUpResumesInterruptedRun's fixture style), a second manual `goobers run`
// for the same workflow must be rejected, not silently dispatch alongside it.
func TestRunRejectedOverMaxConcurrentRuns(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	var wf *apiv1.Workflow
	for i := range set.Workflows {
		if set.Workflows[i].Name == "default-implement" {
			wf = &set.Workflows[i]
		}
	}
	if wf == nil {
		t.Fatal("default-implement workflow not found in fixture config")
	}
	// maxConcurrentRuns defaults to 1 when unset (localscheduler.Conditions.Admit).
	machine, err := workflow.Compile(workflow.Definition{Name: wf.Name, Version: 1, Spec: wf.Spec})
	if err != nil {
		t.Fatalf("compile fixture workflow: %v", err)
	}
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: "already-running-1", Workflow: wf.Name, WorkflowVersion: 1,
		WorkflowDigest: machine.Digest(), Gaggle: wf.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("hand-construct in-flight run journal: %v", err)
	}
	jr.SetMachineState("local-ci")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	// Left at PhaseRunning (no run.finished appended) — ActiveRunCounts and
	// Scheduler.Reconcile both treat this as an active run for the workflow.

	code, _, stderr := runArgs(t, "run", "default-implement", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "run conditions rejected") {
		t.Fatalf("stderr = %q, want it to mention run conditions rejecting the trigger", stderr)
	}
}
