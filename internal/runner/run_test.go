package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// --- stub executors (the "stub stage effects" issue #17 asks for), built
// against the real invoke.Deterministic/invoke.Goober interfaces #18/#19
// implement — not a bespoke seam of this package's own. ---

// stubTaskResult scripts one task's canned outcome, keyed by TaskID.
type stubTaskResult struct {
	status            apiv1.ResultStatus
	summary           string
	errorInfo         *apiv1.ErrorInfo
	artifactName      string
	artifactData      []byte
	artifactMediaType string
}

// stubDeterministic implements invoke.Deterministic like a real executor
// would: it commits its own artifact to the run's journal (via the
// ArtifactRecorder bound at construction) and returns a ResultEnvelope whose
// Artifacts are already real ArtifactPointers — mirroring
// internal/executor.ShellExecutor exactly, just without the shell.
type stubDeterministic struct {
	rec    ArtifactRecorder
	byTask map[string]stubTaskResult
}

func (s *stubDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	cfg, ok := s.byTask[env.TaskID]
	if !ok {
		return apiv1.ResultEnvelope{}, fmt.Errorf("stub executor: no canned output for %q", env.TaskID)
	}
	result := apiv1.ResultEnvelope{Status: cfg.status, Summary: cfg.summary, Error: cfg.errorInfo}
	if cfg.artifactName != "" {
		ref, err := s.rec.RecordArtifact(cfg.artifactName, cfg.artifactData)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		result.Artifacts = []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: cfg.artifactMediaType,
		}}
	}
	return result, nil
}

// flakyDeterministic fails its first failUntil calls with a dispatch-level Go
// error (not a business ResultStatus), then succeeds — for exercising
// Task.Retry's policy-attempt loop. Safe for the single-goroutine dispatch
// runTask does (no locking needed).
type flakyDeterministic struct {
	failUntil int
	calls     int
}

func (f *flakyDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	f.calls++
	if f.calls <= f.failUntil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("transient failure (call %d)", f.calls)
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "eventually succeeded"}, nil
}

// blockingDeterministic signals started once dispatch actually reaches it,
// then blocks until released and signals finished — for proving a
// SIGTERM-style cancellation lets an in-flight attempt finish rather than
// aborting it. started lets the test synchronize "cancel only once the
// attempt is truly in flight" instead of racing the walk loop's own
// between-stages cancellation check.
type blockingDeterministic struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (b *blockingDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	close(b.started)
	<-b.release
	close(b.finished)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// --- fixture repo: a local bare repo, so the test needs no network access ---

func newFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runGit(t, work, "init", "--initial-branch=main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runGit(t *testing.T, dir string, args ...string) {
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

// --- fixture workflow: implement -> review (gate) -> pass/fail ---

func fixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					"pass": workflow.TerminalComplete,
					"fail": workflow.TargetAbort,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile fixture machine: %v", err)
	}
	return m
}

func newTestRunner(t *testing.T, byTask map[string]stubTaskResult, automated invoke.Automated) (*Runner, string) {
	t.Helper()
	return newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	}, automated)
}

// newTestRunnerWithDeterministic builds a Runner over a fixed deterministic
// executor factory, for tests (retries, drain) that need a stub richer than
// stubTaskResult's canned per-TaskID map.
func newTestRunnerWithDeterministic(t *testing.T, newDet NewDeterministicFunc, automated invoke.Automated) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	r, err := New(Config{
		NewDeterministic: newDet,
		Automated:        automated,
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
}

// retryFixtureMachine is fixtureMachine's single task, but with a declared
// retry policy — a separate spec so the existing single-attempt tests stay
// exactly as they were.
func retryFixtureMachine(t *testing.T, maxAttempts int32) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run:   &apiv1.DeterministicRun{Command: []string{"true"}},
				Next:  "review",
				Retry: &apiv1.RetryPolicy{MaxAttempts: maxAttempts},
			},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "retry-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile retry fixture machine: %v", err)
	}
	return m
}

func TestRunnerAdvancesFixtureWorkflowToCompletion(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-1:implement": {
			status: apiv1.ResultSuccess, summary: "wrote a diff",
			artifactName: "diff", artifactData: []byte("--- a\n+++ b\n"), artifactMediaType: "text/x-patch",
		},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if res.FinalState != "review" {
		t.Fatalf("finalState = %q, want review", res.FinalState)
	}

	// Journal is complete and consistent: verify via the Reader, not just the
	// in-memory Result — this is what a resume replays.
	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-1"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.WorkflowDigest != machine.Digest() {
		t.Errorf("run.yaml workflowDigest = %q, want %q (WF-016 pin)", id.WorkflowDigest, machine.Digest())
	}
	if len(id.Inputs) != 1 || id.Inputs[0].Name != "item" {
		t.Errorf("expected the backlog item snapshotted as an immutable input, got %+v", id.Inputs)
	}

	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var types []journal.EventType
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []journal.EventType{
		journal.EventRunStarted,
		journal.EventStageStarted,
		journal.EventArtifactRecorded,
		journal.EventStageFinished,
		journal.EventGateEvaluated,
		journal.EventRunFinished,
	}
	if !eventTypesEqual(types, want) {
		t.Fatalf("event sequence = %v, want %v", types, want)
	}

	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted {
		t.Errorf("state.json phase = %q, want completed", st.Phase)
	}
	if st.MachineState != "" {
		t.Errorf("state.json machineState = %q, want empty (terminal)", st.MachineState)
	}

	// The artifact the task produced is readable back through its recorded Ref,
	// digest-verified — the same round-trip a downstream stage would do via
	// ArtifactPointer.Resolve (#10).
	for _, e := range events {
		if e.Type == journal.EventArtifactRecorded {
			b, err := rd.ArtifactBytes(*e.Ref)
			if err != nil {
				t.Fatalf("ArtifactBytes: %v", err)
			}
			if string(b) != "--- a\n+++ b\n" {
				t.Errorf("artifact bytes = %q", b)
			}
		}
	}
}

func TestRunnerBranchesToAbortOnGateFail(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-2:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-2",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted", res.Phase)
	}
}

func TestRunnerPausesAtHumanGate(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "approve",
		Gates: []apiv1.Gate{
			{
				Name:      "approve",
				Evaluator: apiv1.EvaluatorHuman,
				Human:     &apiv1.HumanGate{},
				Branches:  map[string]string{"approved": workflow.TerminalComplete, "rejected": workflow.TargetAbort},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "human-gate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	r, runsDir := newTestRunner(t, nil, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-3",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseRunning || res.FinalState != "approve" {
		t.Fatalf("result = %+v, want paused at the human gate", res)
	}

	// A human gate executes nothing (§5): no gate.evaluated event, and
	// state.json checkpoints exactly the pause point for a future resume.
	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-3"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseRunning || st.MachineState != "approve" {
		t.Fatalf("state.json = %+v, want running at gate approve", st)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			t.Fatalf("human gate must not be dispatched through the evaluator seam, got gate.evaluated event")
		}
	}
}

// TestRunnerRetriesTaskPerPolicy proves Task.Retry's declared policy governs
// dispatch-level (executor Go-error) failures: the task fails twice then
// succeeds on its 3rd attempt, the run still completes, and every attempt is
// journaled distinctly with the first carrying no AttemptClass and the
// retries carrying "policy".
func TestRunnerRetriesTaskPerPolicy(t *testing.T) {
	machine := retryFixtureMachine(t, 3)
	flaky := &flakyDeterministic{failUntil: 2}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-retry",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if flaky.calls != 3 {
		t.Fatalf("executor called %d times, want 3 (2 failures + 1 success)", flaky.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-retry"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var starts []journal.Event
	for _, e := range events {
		if e.Type == journal.EventStageStarted {
			starts = append(starts, e)
		}
	}
	if len(starts) != 3 {
		t.Fatalf("stage.started events = %d, want 3, got %+v", len(starts), starts)
	}
	wantClasses := []journal.AttemptClass{"", journal.AttemptPolicy, journal.AttemptPolicy}
	for i, e := range starts {
		if e.Attempt != i+1 {
			t.Errorf("start[%d].Attempt = %d, want %d", i, e.Attempt, i+1)
		}
		if e.AttemptClass != wantClasses[i] {
			t.Errorf("start[%d].AttemptClass = %q, want %q", i, e.AttemptClass, wantClasses[i])
		}
	}
}

// TestRunnerExhaustsRetriesAndFails proves a stage that never succeeds within
// its declared MaxAttempts fails the run (Start returns an error) rather than
// retrying forever, and every attempt up to the budget is journaled.
func TestRunnerExhaustsRetriesAndFails(t *testing.T) {
	machine := retryFixtureMachine(t, 2)
	flaky := &flakyDeterministic{failUntil: 1000} // always fails within budget
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	_, err := r.Start(context.Background(), StartInput{
		RunID:   "run-exhaust",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Start: want an error after exhausting the retry budget, got nil")
	}
	if flaky.calls != 2 {
		t.Fatalf("executor called %d times, want exactly 2 (the declared budget)", flaky.calls)
	}

	rd, rerr := journal.OpenRead(filepath.Join(runsDir, "run-exhaust"))
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	events, eerr := rd.Events()
	if eerr != nil {
		t.Fatalf("Events: %v", eerr)
	}
	var starts, errs int
	for _, e := range events {
		if e.Type == journal.EventStageStarted {
			starts++
		}
		if e.Type == journal.EventError {
			errs++
		}
	}
	if starts != 2 || errs != 2 {
		t.Fatalf("stage.started=%d error=%d, want 2 and 2", starts, errs)
	}
}

// TestRunnerDrainsInFlightAttemptOnCancellation proves a SIGTERM-style
// cancellation of the run-level context (internal/signals cancels this exact
// context) lets an in-flight stage attempt finish and journal normally,
// pausing only before the NEXT stage — never aborting mid-attempt.
func TestRunnerDrainsInFlightAttemptOnCancellation(t *testing.T) {
	machine := fixtureMachine(t)
	blocker := &blockingDeterministic{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return blocker, nil
	}, gate.NewAutomatedEvaluator())

	ctx, cancel := context.WithCancel(context.Background())
	type startResult struct {
		res Result
		err error
	}
	done := make(chan startResult, 1)
	go func() {
		res, err := r.Start(ctx, StartInput{
			RunID:   "run-drain",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- startResult{res, err}
	}()

	// Wait until dispatch has actually reached the blocking call before
	// cancelling — otherwise cancel() races the walk loop's own between-
	// stages check and could fire before "implement" is ever dispatched.
	select {
	case <-blocker.started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking executor was never dispatched")
	}

	// Cancel WHILE the (only) task's attempt is blocked mid-dispatch, then
	// release it — proving the cancellation didn't abort the in-flight call.
	cancel()
	select {
	case <-blocker.finished:
		t.Fatal("blocking executor finished before being released — it should still be blocked here")
	case <-time.After(50 * time.Millisecond):
	}
	close(blocker.release)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Start: %v", r.err)
		}
		if r.res.Phase != journal.PhaseRunning {
			t.Fatalf("phase = %q, want running (paused, not aborted)", r.res.Phase)
		}
		if r.res.FinalState != "review" {
			t.Fatalf("finalState = %q, want review (paused before the NEXT stage)", r.res.FinalState)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the blocking call was released")
	}

	select {
	case <-blocker.finished:
	default:
		t.Fatal("expected the in-flight attempt to have finished, not been abandoned")
	}

	// The completed attempt is fully journaled despite the cancellation.
	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-drain"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawFinished bool
	for _, e := range events {
		if e.Type == journal.EventStageFinished && e.Stage == "implement" {
			sawFinished = true
		}
		if e.Type == journal.EventGateEvaluated {
			t.Fatalf("gate should not have been dispatched — the run must pause BEFORE it, got %+v", e)
		}
	}
	if !sawFinished {
		t.Fatal("expected implement's stage.finished to be journaled despite the cancellation")
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.MachineState != "review" {
		t.Fatalf("state.json machineState = %q, want review (resume point)", st.MachineState)
	}
}

// simulateCrashMidAttempt hand-builds a run journal exactly to the point a
// real Start would have reached had the process died right after dispatching
// stageName's attempt N: run.started, then stage.started(stageName, attempt),
// with NO matching stage.finished — the crash signature Resume must detect.
// A clean journal.Create/Close (no torn write) is sufficient here since
// torn-write repair is internal/journal's own, already-tested concern
// (TestKill9MidAppendRecovers); this test is about the runner's
// interpretation of "started with no finished", not journal-write durability.
func simulateCrashMidAttempt(t *testing.T, runsDir string, machine *workflow.Machine, runID, stageName string, attempt int) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("simulateCrashMidAttempt: journal.Create: %v", err)
	}
	jr.SetMachineState(stageName)
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: stageName, Attempt: attempt}); err != nil {
		t.Fatalf("simulateCrashMidAttempt: append stage.started: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("simulateCrashMidAttempt: close: %v", err)
	}
}

// TestRunnerResumeRetriesInterruptedAttempt is #17's crash/resume acceptance
// scenario: a run interrupted mid-attempt resumes, journals the interrupted
// attempt as a terminal infra-tagged failure (never silently re-run), and
// continues the SAME attempt count against the task's own retry budget.
func TestRunnerResumeRetriesInterruptedAttempt(t *testing.T) {
	machine := retryFixtureMachine(t, 3)
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	simulateCrashMidAttempt(t, runsDir, machine, "run-crash", "implement", 1)

	det := &flakyDeterministic{failUntil: 0} // succeeds immediately once dispatched
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if det.calls != 1 {
		t.Fatalf("executor called %d times after resume, want exactly 1 (resume continues at attempt 2, which succeeds immediately)", det.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-crash"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var stageEvents []journal.Event
	for _, e := range events {
		if e.Stage == "implement" {
			stageEvents = append(stageEvents, e)
		}
	}
	wantTypes := []journal.EventType{
		journal.EventStageStarted,  // attempt 1, pre-crash
		journal.EventStageFinished, // attempt 1, infra, journaled by Resume
		journal.EventStageStarted,  // attempt 2, policy
		journal.EventStageFinished, // attempt 2, policy, success
	}
	if len(stageEvents) != len(wantTypes) {
		t.Fatalf("implement-stage events = %d, want %d: %+v", len(stageEvents), len(wantTypes), stageEvents)
	}
	for i, e := range stageEvents {
		if e.Type != wantTypes[i] {
			t.Errorf("event[%d].Type = %q, want %q", i, e.Type, wantTypes[i])
		}
	}
	if stageEvents[1].Attempt != 1 || stageEvents[1].AttemptClass != journal.AttemptInfra || stageEvents[1].Status != string(apiv1.ResultFailure) {
		t.Errorf("interrupted-attempt event = %+v, want attempt=1 class=infra status=failure", stageEvents[1])
	}
	if stageEvents[2].Attempt != 2 || stageEvents[2].AttemptClass != journal.AttemptPolicy {
		t.Errorf("resumed-attempt stage.started = %+v, want attempt=2 class=policy", stageEvents[2])
	}
	if stageEvents[3].Attempt != 2 || stageEvents[3].Status != string(apiv1.ResultSuccess) {
		t.Errorf("resumed-attempt stage.finished = %+v, want attempt=2 status=success", stageEvents[3])
	}

	// interruptedAttempt is excluded from the conformance set (§3.3) via its
	// infra class — confirm IsConformanceNormative agrees.
	if stageEvents[1].IsConformanceNormative() {
		t.Error("the infra-tagged interrupted attempt must be excluded from conformance (§3.3)")
	}
}

// TestRunnerResumeFailsWhenInterruptedAttemptExhaustsBudget proves a crash
// during a task's LAST allowed attempt does not grant it a bonus attempt —
// Resume must fail closed, not silently extend the retry budget.
func TestRunnerResumeFailsWhenInterruptedAttemptExhaustsBudget(t *testing.T) {
	machine := retryFixtureMachine(t, 1) // MaxAttempts=1: no retries allowed at all
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	simulateCrashMidAttempt(t, runsDir, machine, "run-crash-exhausted", "implement", 1)

	det := &flakyDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = r.Resume(context.Background(), ResumeInput{
		RunID:   "run-crash-exhausted",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Resume: want an error — the interrupted attempt already consumed the entire 1-attempt budget")
	}
	if det.calls != 0 {
		t.Fatalf("executor called %d times, want 0 — must not dispatch beyond the exhausted budget", det.calls)
	}
}

// TestRunnerResumeAlreadyTerminalIsIdempotent proves Resume on an
// already-finished run just reports its phase, without re-dispatching
// anything.
func TestRunnerResumeAlreadyTerminalIsIdempotent(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-done:implement": {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-done",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	res2, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-done",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res2.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume phase = %q, want completed (idempotent, no re-dispatch)", res2.Phase)
	}
}

// TestRunnerResumeRestoresGateRepassCounter is #89's acceptance scenario: a
// crash mid repass-loop must not grant the gate extra passes beyond its
// budget. The fixture's "review" gate loops "fail" back to "implement"
// (rather than aborting), so a broken seed would let the resumed run take a
// second full pass before escalating instead of escalating immediately.
func TestRunnerResumeRestoresGateRepassCounter(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "implement"},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "repass-loop", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	byTask := map[string]stubTaskResult{
		"run-repass:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "x", Message: "always fails"}},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())
	r.cfg.MaxRepasses = 1 // a 2nd non-pass evaluation must escalate

	// Hand-build a journal at the point a real Start would have reached had
	// the process died right after journaling "review"'s first fail (attempt
	// 1, not yet escalated) and looping back to "implement" for a 2nd pass —
	// mirroring simulateCrashMidAttempt's pattern for task attempts.
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-repass", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "implement",
		Runner: map[string]any{"repassAttempt": 1, "escalated": false},
	}); err != nil {
		t.Fatalf("append gate.evaluated: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-repass",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated (seeded repass count should exhaust the budget on the very next evaluation)", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-repass"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	gateEvals := 0
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			gateEvals++
		}
	}
	if gateEvals != 2 {
		t.Fatalf("gate.evaluated count = %d, want 2 (1 pre-crash + exactly 1 post-resume before escalating) — a broken repass seed would allow extra passes before escalating", gateEvals)
	}
}

func eventTypesEqual(got, want []journal.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
