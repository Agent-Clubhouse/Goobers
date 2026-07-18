package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	// The daemon binds an ephemeral loopback port rather than the fixed default
	// 127.0.0.1:8080 — see the suite-wide apiListenAddress seam in
	// testmain_test.go (#798), which redirects the default for every
	// daemon-starting test so none collides with a co-located `goobers up`.

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

func TestSummarizeHeartbeatCountsOnlyNewSchedulerActivity(t *testing.T) {
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventTriggerFired},
		{Seq: 3, Type: journal.EventTriggerFired},
		{Seq: 4, Type: journal.EventRunStarted},
		{Seq: 5, Type: journal.EventRunFinished},
		{Seq: 6, Type: journal.EventTickSkipped},
		{Seq: 7, Type: journal.EventClaimReleased},
	}

	got, lastSeq := summarizeHeartbeat(events, 2)
	want := heartbeatActivity{triggers: 1, started: 1, finished: 1, skipped: 1}
	if got != want {
		t.Fatalf("activity = %+v, want %+v", got, want)
	}
	if lastSeq != 7 {
		t.Fatalf("last seq = %d, want 7", lastSeq)
	}
}

func TestUpHeartbeatIsDefaultOnAndQuietSuppressesIt(t *testing.T) {
	previous := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = previous })

	for _, tc := range []struct {
		name          string
		args          func(string) []string
		wantHeartbeat bool
	}{
		{name: "default", args: func(root string) []string { return []string{root} }, wantHeartbeat: true},
		{name: "quiet", args: func(root string) []string { return []string{"--quiet", root} }, wantHeartbeat: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDeterministicDemo(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stdout := newDaemonOutput()
			var stderr bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- runUpContext(ctx, tc.args(root), stdout, &stderr)
			}()

			select {
			case <-stdout.started:
			case code := <-done:
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			case <-time.After(10 * time.Second):
				t.Fatal("daemon did not start")
			}

			if tc.wantHeartbeat {
				select {
				case <-stdout.heartbeat:
				case code := <-done:
					t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
				case <-time.After(10 * time.Second):
					t.Fatalf("stdout = %q, want heartbeat", stdout.String())
				}
			} else {
				select {
				case <-stdout.heartbeat:
					t.Fatalf("stdout = %q, want no heartbeat", stdout.String())
				case code := <-done:
					t.Fatalf("daemon exited early with code %d, stderr = %q", code, stderr.String())
				case <-time.After(10 * heartbeatInterval):
				}
			}

			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("code = %d, stderr = %q", code, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("runUpContext did not return after ctx cancellation")
			}
		})
	}
}

type daemonOutput struct {
	mu            sync.Mutex
	buf           bytes.Buffer
	started       chan struct{}
	heartbeat     chan struct{}
	startedOnce   sync.Once
	heartbeatOnce sync.Once
}

func newDaemonOutput() *daemonOutput {
	return &daemonOutput{
		started:   make(chan struct{}),
		heartbeat: make(chan struct{}),
	}
}

func (o *daemonOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buf.Write(p)
	output := o.buf.String()
	if strings.Contains(output, "daemon started") {
		o.startedOnce.Do(func() { close(o.started) })
	}
	if strings.Contains(output, "] alive — ") {
		o.heartbeatOnce.Do(func() { close(o.heartbeat) })
	}
	return n, err
}

func (o *daemonOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
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
	// Wait for the resumed run to actually reach a terminal phase rather than
	// guessing at a wall-clock window: a fixed sleep is long enough on an idle
	// machine but not on a loaded CI runner under -race, which made this test
	// flake with phase still "running".
	stop := pollUntilRunTerminal(t, filepath.Join(l.RunsDir(), runID), cancel)
	defer stop()

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

// TestRunTakesSameLockAsUp is issue #134's lock half: `goobers run` used to
// skip the instance lock entirely, so two concurrent processes (or a manual
// run against a live `up` daemon) could mutate scheduler/run-condition state
// and the shared workcopies/ tree at once. Now it takes the same lock `up`
// does — this test's lock holder isn't a real daemon sweeping delegation
// requests, so the attempt still surfaces as a failure, just via #343's
// delegation timeout rather than the pre-#343 immediate lock-conflict error
// (see TestRunLockConflictDelegatesRatherThanFailingImmediately in
// lock_test.go for that distinction, and TestRunDelegatesToLiveDaemon in
// rundelegate_test.go for the real success path against a live daemon).
func TestRunTakesSameLockAsUp(t *testing.T) {
	prevTimeout := triggerDelegationTimeout
	triggerDelegationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { triggerDelegationTimeout = prevTimeout })

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
	if !strings.Contains(stderr, "timed out") {
		t.Fatalf("stderr = %q, want a delegation timeout", stderr)
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

// pollUntilRunTerminal watches runDir until the run reaches a terminal phase
// and then calls cancel, so a resume test stops the daemon on the run's actual
// progress instead of a fixed wall-clock guess. It gives up after 30s so a
// genuinely stuck run still fails the test (via the caller's phase assertion)
// rather than hanging. The returned func stops the watcher.
func pollUntilRunTerminal(t *testing.T, runDir string, cancel context.CancelFunc) func() {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		defer cancel()
		deadline := time.After(30 * time.Second)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-deadline:
				return
			case <-ticker.C:
				rd, err := journal.OpenRead(runDir)
				if err != nil {
					continue
				}
				st, err := rd.State()
				if err != nil {
					continue
				}
				switch st.Phase {
				case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}
