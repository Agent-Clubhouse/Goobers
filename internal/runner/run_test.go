package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// fakeSpanStarter records every span opened, mirroring runner.SpanStarter's
// three methods (issue #126). Returns the zero telemetry.Span throughout, so
// its End/Succeed/Fail calls no-op exactly like a nil Telemetry would.
type fakeSpanStarter struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSpanStarter) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSpanStarter) StartRun(ctx context.Context, attrs telemetry.RunAttributes) (context.Context, telemetry.Span, error) {
	f.record("run:" + attrs.RunID)
	return ctx, telemetry.Span{}, nil
}

func (f *fakeSpanStarter) StartTask(ctx context.Context, attrs telemetry.TaskAttributes) (context.Context, telemetry.Span, error) {
	f.record("task:" + attrs.TaskID)
	return ctx, telemetry.Span{}, nil
}

func (f *fakeSpanStarter) StartGate(ctx context.Context, attrs telemetry.GateAttributes) (context.Context, telemetry.Span, error) {
	f.record("gate:" + attrs.GateID)
	return ctx, telemetry.Span{}, nil
}

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
	outputs           map[string]interface{}
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
	result := apiv1.ResultEnvelope{Status: cfg.status, Summary: cfg.summary, Error: cfg.errorInfo, Outputs: cfg.outputs}
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

// outputCapturingDeterministic is stubDeterministic plus recording each
// call's full InvocationEnvelope by TaskID, so a test can assert what
// actually arrived in env.Inputs — e.g. #132's InputsFrom output->input
// threading, which stubDeterministic's plain canned-Outputs lookup can't
// observe from the consuming side. (Named distinctly from #107's
// capturingDeterministic below, which records only its last call for a
// different purpose — resume pointer-visibility, not input threading.)
type outputCapturingDeterministic struct {
	rec      ArtifactRecorder
	byTask   map[string]stubTaskResult
	received map[string]apiv1.InvocationEnvelope
}

func (c *outputCapturingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if c.received == nil {
		c.received = map[string]apiv1.InvocationEnvelope{}
	}
	c.received[env.TaskID] = env
	cfg, ok := c.byTask[env.TaskID]
	if !ok {
		return apiv1.ResultEnvelope{}, fmt.Errorf("stub executor: no canned output for %q", env.TaskID)
	}
	result := apiv1.ResultEnvelope{Status: cfg.status, Summary: cfg.summary, Error: cfg.errorInfo, Outputs: cfg.outputs}
	if cfg.artifactName != "" {
		ref, err := c.rec.RecordArtifact(cfg.artifactName, cfg.artifactData)
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

// alwaysFailAutomated is an invoke.Automated that always reports "fail",
// regardless of the subject's actual status — for constructing a gate whose
// declared "pass" branch is statically reachable (satisfying
// workflow.Compile's reachability check) but never actually taken at
// runtime, e.g. to exercise a genuine runaway machine against maxSteps.
type alwaysFailAutomated struct{}

func (alwaysFailAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return "fail", nil
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

// taskReservedNextFixtureMachine is a single deterministic task whose Next is
// a reserved terminal target directly (#123) — unlike retryFixtureMachine's
// abort path, there is no gate mediating it: workflow.Compile admits a
// reserved target as a task's Next exactly as it does for a gate branch
// (compile.go's isTerminal check covers both), so the runner must walk it the
// same way too.
func taskReservedNextFixtureMachine(t *testing.T, next string) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: next,
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "task-reserved-next-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile task-reserved-next fixture machine (next=%q): %v", next, err)
	}
	return m
}

// inputsFromFixtureMachine is a minimal open-pr -> ci-poll chain proving
// #132's task-to-task output->input threading: ci-poll declares
// InputsFrom: {"prNumber": "prNumber"}, so it must receive open-pr's
// Outputs["prNumber"] as its own env.Inputs["prNumber"].
func inputsFromFixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "open-pr",
		Tasks: []apiv1.Task{
			{Name: "open-pr", Type: apiv1.TaskDeterministic, Goal: "open the pr", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "ci-poll"},
			{Name: "ci-poll", Type: apiv1.TaskDeterministic, Goal: "poll ci", Run: &apiv1.DeterministicRun{Command: []string{"true"}},
				InputsFrom: map[string]string{"prNumber": "prNumber"}},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "inputs-from-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile inputs-from fixture machine: %v", err)
	}
	return m
}

// TestRunnerThreadsInputsFromUpstreamOutputs proves the #132 prNumber-handoff
// mechanism end to end at the runner level: open-pr's Outputs["prNumber"]
// arrives in ci-poll's env.Inputs["prNumber"] via the declared InputsFrom
// mapping, not blanket propagation.
func TestRunnerThreadsInputsFromUpstreamOutputs(t *testing.T) {
	machine := inputsFromFixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-1:open-pr": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"prNumber": "42"}},
		"run-1:ci-poll": {status: apiv1.ResultSuccess},
	}
	det := &outputCapturingDeterministic{byTask: byTask}
	r, _ := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		det.rec = rec
		return det, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
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
	ciPollEnv, ok := det.received["run-1:ci-poll"]
	if !ok {
		t.Fatal("ci-poll never dispatched")
	}
	if got := ciPollEnv.Inputs["prNumber"]; got != "42" {
		t.Fatalf("ci-poll env.Inputs[prNumber] = %v, want \"42\" (threaded from open-pr's Outputs)", got)
	}
}

// TestRunnerInputsFromMissingUpstreamOutputFailsClosed proves a declared
// InputsFrom entry whose referenced upstream output key never showed up
// fails the stage closed rather than silently dispatching with the key
// absent — InputsFrom is a contract, not a best-effort hint.
func TestRunnerInputsFromMissingUpstreamOutputFailsClosed(t *testing.T) {
	machine := inputsFromFixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-1:open-pr": {status: apiv1.ResultSuccess}, // no Outputs["prNumber"] set
		"run-1:ci-poll": {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	_, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("expected an error when a declared InputsFrom output is missing upstream")
	}
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
		journal.EventRefTouched, // the run branch, journaled up front (#133)
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

// TestRunnerWalksTaskReservedNextTargets is the regression test for #123: a
// task's Next can itself be a reserved terminal target (@abort/@escalate),
// admitted by workflow.Compile the same way a gate's branch is, but before
// this fix walk() only special-cased Next == "" and fell through to
// "unknown state" for anything else — a compile-admitted definition crashing
// the local runner while completing fine on the (quarantined) Temporal
// engine, a direct §3.3 conformance violation.
func TestRunnerWalksTaskReservedNextTargets(t *testing.T) {
	cases := []struct {
		name      string
		next      string
		wantPhase journal.RunPhase
	}{
		{"abort", workflow.TargetAbort, journal.PhaseAborted},
		{"escalate", workflow.TargetEscalate, journal.PhaseEscalated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := taskReservedNextFixtureMachine(t, tc.next)
			runID := "run-task-next-" + tc.name
			byTask := map[string]stubTaskResult{runID + ":implement": {status: apiv1.ResultSuccess, summary: "done"}}
			r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

			res, err := r.Start(context.Background(), StartInput{
				RunID:   runID,
				Machine: machine,
				Gaggle:  "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", res.Phase, tc.wantPhase)
			}
		})
	}
}

// terminalFailMachine is a single task with no Next (terminal) — the exact
// shape #110's ruling targets: "a terminal task's failure journals the run
// as PhaseCompleted" was the bug (run.go:303-305 pre-fix).
func terminalFailMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "terminal-fail", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile terminal-fail machine: %v", err)
	}
	return m
}

// chainedNoGateMachine is two tasks back to back with no gate between them —
// "implement" feeds directly into "deploy", never through a gate that could
// branch on a business failure.
func chainedNoGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "deploy"},
			{Name: "deploy", Type: apiv1.TaskDeterministic, Goal: "ship it", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "chained-no-gate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile chained-no-gate machine: %v", err)
	}
	return m
}

// TestRunnerTaskFailureWithNoGateNextFailsRun proves the #110 ruling's core
// fix: a terminal task's business "failure" must journal PhaseFailed, not
// PhaseCompleted — the exact defect the architect review caught (every
// shipped V0 workflow has no gate after its first stage, so every broken run
// used to journal as completed, fail-open).
func TestRunnerTaskFailureWithNoGateNextFailsRun(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-fail-terminal:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
	}
	r, runsDir := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-fail-terminal",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed (a terminal task's failure must never journal as completed)", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-fail-terminal"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state.json phase = %q, want failed", st.Phase)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseFailed) {
		t.Fatalf("last event = %+v, want run.finished status=failed", last)
	}
}

// TestRunnerTaskFailureWithNonGateNextFailsRunAndSkipsDownstream proves a
// business failure whose Next names another task (not a gate) never
// dispatches that downstream task — "never run downstream stages on
// garbage" (ruling #110) — instead of the pre-fix behavior of advancing
// unconditionally regardless of status.
func TestRunnerTaskFailureWithNonGateNextFailsRunAndSkipsDownstream(t *testing.T) {
	machine := chainedNoGateMachine(t)
	byTask := map[string]stubTaskResult{
		"run-fail-chain:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
		// Deliberately no canned result for "deploy" — if the runner ever
		// dispatches it, stubDeterministic.Run errors ("no canned output"),
		// which would surface as a different failure than PhaseFailed and
		// fail this test's phase assertion below.
	}
	r, runsDir := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-fail-chain",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if res.FinalState != "implement" {
		t.Fatalf("finalState = %q, want implement (the run must stop there, never reach deploy)", res.FinalState)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-fail-chain"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Stage == "deploy" {
			t.Fatalf("deploy must never be dispatched after implement's failure, got event %+v", e)
		}
	}
}

// TestRunnerTaskFailureWithGateNextStillBranches proves the ruling's
// preserved case: a business failure whose Next is a gate still advances so
// the gate can branch on it (the shipped reviewer-gate pattern) — the same
// path TestRunnerBranchesToAbortOnGateFail exercises end to end, asserted
// here directly against the gate.evaluated event so a regression that
// terminalizes on ANY failure (over-correcting #110) is caught too.
func TestRunnerTaskFailureWithGateNextStillBranches(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-fail-gate:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-fail-gate",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted (the gate branches fail->@abort)", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-fail-gate"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawGateEval bool
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			sawGateEval = true
			if e.Verdict != "fail" {
				t.Errorf("gate verdict = %q, want fail", e.Verdict)
			}
		}
	}
	if !sawGateEval {
		t.Fatal("expected the review gate to evaluate the failure, got no gate.evaluated event")
	}
}

// TestRunnerTaskBlockedHaltsResumablePause proves a "blocked" business status
// halts the run at that task — a resumable pause like a human gate's, at any
// position — instead of the pre-fix behavior of advancing to Next
// unconditionally (which would evaluate "review" against a blocked result).
func TestRunnerTaskBlockedHaltsResumablePause(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-blocked:implement": {status: apiv1.ResultBlocked, summary: "waiting on an external dependency"},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-blocked",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseRunning {
		t.Fatalf("phase = %q, want running (paused pending intervention, not terminal)", res.Phase)
	}
	if res.FinalState != "implement" {
		t.Fatalf("finalState = %q, want implement (halted there, not advanced to review)", res.FinalState)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-blocked"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseRunning || st.MachineState != "implement" {
		t.Fatalf("state.json = %+v, want running at implement (resume point)", st)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			t.Fatalf("review must not be evaluated against a blocked result, got gate.evaluated event %+v", e)
		}
	}
}

// TestRunnerMaxStepsExceededFailsRunClosed proves a runaway machine's
// max-steps abort journals PhaseFailed instead of leaving the run stuck at
// phase=running forever — the daemon auto-resumes every PhaseRunning run on
// restart (cmd/goobers/daemon.go), so an unterminated runaway run would
// re-loop and re-fail identically on every restart (ruling #110 item 6: every
// error return out of walk must first append a terminal event).
func TestRunnerMaxStepsExceededFailsRunClosed(t *testing.T) {
	// compile.go's reachability check requires every state have SOME path to
	// a terminal, so a bare task->task self-loop is rejected at compile time
	// (tasks have only one Next, no branching). A gate's "pass" branch gives
	// the machine a statically reachable terminal; alwaysFailAutomated below
	// makes the gate never actually take it at runtime, so "loop"->"check"
	// cycles forever — the genuine runaway-machine shape maxSteps guards.
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "loop",
		Tasks: []apiv1.Task{
			{Name: "loop", Type: apiv1.TaskDeterministic, Goal: "spin", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "check"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "check",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "loop"},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "runaway", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	det := &stubDeterministic{byTask: map[string]stubTaskResult{
		"run-runaway:loop": {status: apiv1.ResultSuccess},
	}}

	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        alwaysFailAutomated{},
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		MaxSteps:         3,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = r.Start(context.Background(), StartInput{
		RunID:   "run-runaway",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Start: want an error after exceeding max steps")
	}

	rd, rerr := journal.OpenRead(filepath.Join(runsDir, "run-runaway"))
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	st, serr := rd.State()
	if serr != nil {
		t.Fatalf("State: %v", serr)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state.json phase = %q, want failed — the run must not be left at phase=running for the daemon to re-resume and re-loop forever", st.Phase)
	}
	events, eerr := rd.Events()
	if eerr != nil {
		t.Fatalf("Events: %v", eerr)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseFailed) {
		t.Fatalf("last event = %+v, want run.finished status=failed", last)
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
	// Mirror Start's unconditional run-branch ref.touched event (#133) — a
	// real crash always went through Start first, so a faithful simulation
	// must too, or a conformance comparison against a real Start'd run sees
	// a phantom extra event on the clean side only.
	if err := jr.Append(journal.Event{
		Type: journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{
			Provider: string(apiv1.ProviderGitHub),
			Kind:     "branch",
			ID:       providers.BranchName(machine.Def.Name, runID),
		},
	}); err != nil {
		t.Fatalf("simulateCrashMidAttempt: append ref.touched: %v", err)
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
		journal.EventStageStarted,  // attempt 2, the crash-driven continuation
		journal.EventStageFinished, // attempt 2, the crash-driven continuation, success
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
	// #111: the continuation dispatched right after an interrupted attempt is
	// driven by the CRASH, not Task.Retry — it must be tagged "infra", not
	// "policy" (which would wrongly make it conformance-normative, §3.3).
	if stageEvents[2].Attempt != 2 || stageEvents[2].AttemptClass != journal.AttemptInfra {
		t.Errorf("resumed-attempt stage.started = %+v, want attempt=2 class=infra", stageEvents[2])
	}
	if stageEvents[3].Attempt != 2 || stageEvents[3].AttemptClass != journal.AttemptInfra || stageEvents[3].Status != string(apiv1.ResultSuccess) {
		t.Errorf("resumed-attempt stage.finished = %+v, want attempt=2 class=infra status=success", stageEvents[3])
	}

	// Every post-crash event for "implement" (the interrupted marker AND the
	// crash-driven continuation's own started/finished) is excluded from the
	// conformance set (§3.3) — confirm IsConformanceNormative agrees for all
	// three, not just the interrupted marker.
	for i := 1; i <= 3; i++ {
		if stageEvents[i].IsConformanceNormative() {
			t.Errorf("event[%d] = %+v must be excluded from conformance (§3.3) — only the original attempt=1 started event may be normative for a crashed stage", i, stageEvents[i])
		}
	}

	// #111's conformance-seed assertion: a crash+resume run's normative
	// event set must not gain phantom policy-retry events a crash-free run
	// of the identical workflow never produces. Build a clean run of the
	// SAME machine (succeeding on its first attempt, no crash) and confirm
	// its normative set is exactly the crash run's normative set PLUS the
	// one event a crash can never journal — "implement"'s own
	// stage.finished(attempt=1) — since the interrupted attempt's true
	// result is genuinely unknowable, not because of any policy-tagging bug.
	cleanByTask := map[string]stubTaskResult{"run-clean:implement": {status: apiv1.ResultSuccess}}
	cleanRunner, cleanRunsDir := newTestRunner(t, cleanByTask, gate.NewAutomatedEvaluator())
	cleanRes, cerr := cleanRunner.Start(context.Background(), StartInput{
		RunID:   "run-clean",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if cerr != nil {
		t.Fatalf("clean Start: %v", cerr)
	}
	if cleanRes.Phase != journal.PhaseCompleted {
		t.Fatalf("clean run phase = %q, want completed", cleanRes.Phase)
	}
	cleanRd, cerr := journal.OpenRead(filepath.Join(cleanRunsDir, "run-clean"))
	if cerr != nil {
		t.Fatalf("OpenRead (clean): %v", cerr)
	}
	cleanEvents, cerr := cleanRd.Events()
	if cerr != nil {
		t.Fatalf("Events (clean): %v", cerr)
	}

	normativeTypes := func(evs []journal.Event) []journal.EventType {
		var out []journal.EventType
		for _, e := range evs {
			if e.IsConformanceNormative() {
				out = append(out, e.Type)
			}
		}
		return out
	}
	crashNormative := normativeTypes(events)
	cleanNormative := normativeTypes(cleanEvents)
	// The clean run has exactly one extra normative event vs the crash run:
	// "implement"'s stage.finished(attempt=1) — remove it before comparing.
	removed := false
	var cleanNormativeMinusStageFinished []journal.EventType
	for _, ty := range cleanNormative {
		if !removed && ty == journal.EventStageFinished {
			removed = true
			continue
		}
		cleanNormativeMinusStageFinished = append(cleanNormativeMinusStageFinished, ty)
	}
	if !removed {
		t.Fatal("clean run's normative set unexpectedly has no stage.finished to remove for comparison")
	}
	if !eventTypesEqual(crashNormative, cleanNormativeMinusStageFinished) {
		t.Fatalf("crash-run normative types = %v, want clean-run normative types minus one stage.finished = %v (a crash must not add phantom normative events beyond the one it structurally cannot produce)", crashNormative, cleanNormativeMinusStageFinished)
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

	// The exhaustion must reach a terminal journal state (ruling #110's
	// failTerminal), not leave the run at phase=running — that's what makes
	// a SECOND Resume (TestDoubleResumeAfterExhaustedBudgetDoesNotGrantFreshBudget,
	// #109) short-circuit instead of granting a fresh attempt budget.
	rd, rerr := journal.OpenRead(filepath.Join(runsDir, "run-crash-exhausted"))
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	st, serr := rd.State()
	if serr != nil {
		t.Fatalf("State: %v", serr)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state.json phase = %q, want failed", st.Phase)
	}
}

// TestDoubleResumeAfterExhaustedBudgetDoesNotGrantFreshBudget is #109's
// acceptance scenario: Resume #1 of a crash on a task's LAST allowed attempt
// must not leave the run resumable again with a fresh budget. Before #110's
// failTerminal fix, Resume #1's "no attempts left" error propagated with no
// terminal journal write, so Resume #2 read a still-"running" state.json,
// found interruptedAttempt back at 0 (the infra-tagged marker Resume #1 DID
// manage to journal made started==finished again), and re-dispatched
// "implement" from attempt 1 with its full budget restored — exactly what a
// crash-loop-of-restarts (cmd/goobers/daemon.go auto-resumes every
// PhaseRunning run) would repeat forever. #110's failTerminal closes this by
// journaling PhaseFailed before Resume #1 returns its error, so Resume #2
// short-circuits at the terminal-phase check instead of re-entering the walk
// at all.
func TestDoubleResumeAfterExhaustedBudgetDoesNotGrantFreshBudget(t *testing.T) {
	machine := retryFixtureMachine(t, 1) // MaxAttempts=1: no retries allowed at all
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	simulateCrashMidAttempt(t, runsDir, machine, "run-double-resume", "implement", 1)

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

	// Resume #1: the interrupted attempt already consumed the entire budget,
	// so this errors (TestRunnerResumeFailsWhenInterruptedAttemptExhaustsBudget
	// covers this in isolation) — but must still journal a terminal phase.
	if _, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-double-resume",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err == nil {
		t.Fatal("Resume #1: want an error — the interrupted attempt already exhausted its budget")
	}

	// Resume #2 (the daemon-restart-retries-forever scenario): must NOT
	// re-dispatch "implement" with a fresh budget — it should short-circuit
	// on the terminal phase Resume #1 already journaled.
	res2, err2 := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-double-resume",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err2 != nil {
		t.Fatalf("Resume #2: %v", err2)
	}
	if res2.Phase != journal.PhaseFailed {
		t.Fatalf("Resume #2 phase = %q, want failed (idempotent terminal short-circuit, not a fresh attempt)", res2.Phase)
	}
	if det.calls != 0 {
		t.Fatalf("executor called %d times across both resumes, want 0 — a crash on the last attempt must never grant a fresh budget on a later resume", det.calls)
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

// countingDeterministic records how many times it was dispatched — for
// proving a stage was NOT re-dispatched (#107): an already-finished stage
// resumed at must never re-invoke its executor.
type countingDeterministic struct{ calls int }

func (c *countingDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// capturingDeterministic records the invocation envelope of its last call —
// for asserting a downstream stage's ContextPointers after a resume (#107's
// pointer-visibility scenario).
type capturingDeterministic struct {
	calls   int
	lastEnv apiv1.InvocationEnvelope
}

func (c *capturingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.calls++
	c.lastEnv = env
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// chainedThroughGateMachine is implement -> review (gate, pass->deploy) ->
// deploy — for proving a resumed run's ContextPointers still carry an
// artifact a stage produced before the crash (#107), once it flows through
// a gate rather than directly to the next task.
func chainedThroughGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
			{Name: "deploy", Type: apiv1.TaskDeterministic, Goal: "ship it", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": "deploy", "fail": workflow.TargetAbort},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "chained-through-gate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile chained-through-gate machine: %v", err)
	}
	return m
}

// newTestRunnerEnv builds the worktree manager, runs directory, and fixture
// repo a hand-built-journal resume test needs, without a byTask stub map
// (the crash-simulation tests dispatch through custom invoke.Deterministic
// implementations instead of stubDeterministic).
func newTestRunnerEnv(t *testing.T) (runsDir, fixtureRepo string, wtMgr *worktree.Manager) {
	t.Helper()
	instanceRoot := t.TempDir()
	mgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	return filepath.Join(instanceRoot, "runs"), newFixtureRepo(t), mgr
}

// TestRunnerResumeAdvancesPastFinishedTaskWithoutRedispatch is #107's core
// acceptance scenario: a crash right after a task's stage.finished is
// journaled, before the machine advances past it (state.json's
// machineState still names the task — walk's SetMachineState timing).
// Resume's only pre-#107 guard, interruptedAttempt, reports "not
// interrupted" here (started == finished) — the exact gap that used to
// re-dispatch the completed stage from attempt 1, re-running its side
// effects.
func TestRunnerResumeAdvancesPastFinishedTaskWithoutRedispatch(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-finished-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	// No further events: the crash lands here, before walk's next loop
	// iteration ever reassigns state to "review".
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	counting := &countingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return counting, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-finished-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if counting.calls != 0 {
		t.Fatalf("implement was re-dispatched %d times after resume, want 0 — an already-finished stage must never re-run its side effects", counting.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-finished-crash"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var starts int
	for _, e := range events {
		if e.Type == journal.EventStageStarted && e.Stage == "implement" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("stage.started(implement) count = %d, want 1 (the pre-crash one only — no re-dispatch)", starts)
	}
}

// TestRunnerResumeReconstructsUpstreamPointersForDownstreamStage is #107's
// pointer-visibility scenario: a stage that finished (and produced an
// artifact) before the crash must still be visible to a downstream stage as
// a ContextPointer after resume, even though resume advances past it
// without re-dispatching it (so there is no live in-process
// `pointers = append(pointers, produced...)` to rebuild it from).
func TestRunnerResumeReconstructsUpstreamPointersForDownstreamStage(t *testing.T) {
	machine := chainedThroughGateMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-ptr-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	artRef, err := jr.RecordArtifact("diff", []byte("--- a\n+++ b\n"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{{Path: artRef.Path, Digest: artRef.Digest, Size: artRef.Size}},
	}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	capturing := &capturingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return capturing, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-ptr-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if capturing.calls != 1 {
		t.Fatalf("deploy dispatched %d times, want exactly 1", capturing.calls)
	}

	var found bool
	for _, cp := range capturing.lastEnv.ContextPointers {
		if cp.Name == "implement.artifact[0]" && cp.Artifact != nil && cp.Artifact.Digest == artRef.Digest {
			found = true
		}
	}
	if !found {
		t.Fatalf("deploy's ContextPointers = %+v, want implement.artifact[0] pointing at digest %q (the pre-crash artifact, reconstructed from the journal)", capturing.lastEnv.ContextPointers, artRef.Digest)
	}
}

// TestRunnerResumeAtGateEvaluatesRealSubject is #108's core acceptance
// scenario: a crash after a task finishes but before its downstream gate
// ever evaluates leaves state.json pointing at the gate with no
// gate.evaluated event at all. Resume must reconstruct the finished task's
// REAL result as the gate's subject (status/outputs/artifacts, now
// journaled on stage.finished for exactly this) — not evaluate against a
// zero-value ResultEnvelope, which would make an automated status-equals
// check read status="" and always fail closed, wrongly aborting a run whose
// subject stage actually succeeded.
func TestRunnerResumeAtGateEvaluatesRealSubject(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-gate-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	// walk's next loop iteration reaches the gate and checkpoints there
	// (SetMachineState("review")) before crashing — no gate.evaluated event
	// exists yet.
	jr.SetMachineState("review")
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Deliberately no canned output for "implement" — if resume ever
	// re-dispatches it (a #107 regression), stubDeterministic errors and
	// this test's phase assertion fails.
	det := &stubDeterministic{byTask: map[string]stubTaskResult{}}
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
		RunID:   "run-gate-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed — the automated gate must see the real success status reconstructed from the journal, not a zero-value subject that always evaluates to fail", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-gate-crash"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated && e.Verdict != gate.OutcomePass {
			t.Fatalf("gate.evaluated verdict = %q, want %q", e.Verdict, gate.OutcomePass)
		}
	}
}

// TestRunnerResumeRefusesEmptyWorkflowDigest proves the #112 hardening fix:
// a run journal with no pinned WorkflowDigest (a corrupted or pre-WF-016
// run.yaml — Start always pins one, so this should never happen in
// practice) is refused, not silently resumed under whatever Machine the
// caller happens to pass — WF-016's whole point is that a changed
// definition is refused, not reinterpreted, and an unpinned run is
// indistinguishable from "we don't know if the definition changed."
func TestRunnerResumeRefusesEmptyWorkflowDigest(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-no-digest", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		// WorkflowDigest deliberately left empty.
		Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r, err := New(Config{
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = r.Resume(context.Background(), ResumeInput{
		RunID:   "run-no-digest",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Resume: want an error — an unpinned run must be refused, not silently resumed (WF-016)")
	}
}

// blockingCommenter blocks inside UpdateWorkItem until released, recording
// the ctx it was called with (checked AFTER release, once the caller has
// had a chance to cancel it) — for proving NotifyEscalated survives a
// SIGTERM-style cancellation of the run-level context (#112) instead of
// racing it.
type blockingCommenter struct {
	called  chan struct{}
	release chan struct{}
	ctxErr  error
}

func (c *blockingCommenter) UpdateWorkItem(ctx context.Context, _ providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	close(c.called)
	<-c.release
	c.ctxErr = ctx.Err()
	return providers.WorkItem{}, nil
}

// repassLoopMachine is implement (always fails) -> review (automated gate,
// fail loops back to implement) — the same shape
// TestRunnerResumeRestoresGateRepassCounter uses, factored out here so this
// file's escalation test can drive it fresh via Start instead of a
// hand-built journal.
func repassLoopMachine(t *testing.T) *workflow.Machine {
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
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "implement"},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "repass-loop-escalate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile repass-loop machine: %v", err)
	}
	return m
}

// TestRunnerNotifyEscalatedSurvivesCancellation proves the #112 hardening
// fix: NotifyEscalated now runs on context.WithoutCancel, the same drain
// contract as every other post-decision effect (runTask's dispatch,
// evaluateGate) — a SIGTERM mid-notification must let it finish instead of
// racing it, so the escalation comment reliably posts exactly once instead
// of risking an aborted post that a later resume's re-evaluation would then
// duplicate.
func TestRunnerNotifyEscalatedSurvivesCancellation(t *testing.T) {
	machine := repassLoopMachine(t)
	byTask := map[string]stubTaskResult{
		"run-escalate:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "x", Message: "always fails"}},
	}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	commenter := &blockingCommenter{called: make(chan struct{}), release: make(chan struct{})}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		MaxRepasses:  0, // escalate on the very first non-pass evaluation
		Escalation:   &gate.EscalationNotifier{Poster: commenter, Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type startResult struct {
		res Result
		err error
	}
	done := make(chan startResult, 1)
	go func() {
		res, err := r.Start(ctx, StartInput{
			RunID:   "run-escalate",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"},
		})
		done <- startResult{res, err}
	}()

	select {
	case <-commenter.called:
	case <-time.After(5 * time.Second):
		t.Fatal("NotifyEscalated's UpdateWorkItem was never dispatched")
	}

	// Cancel WHILE the notification is blocked mid-flight, then release it —
	// proving the cancellation didn't abort the in-flight call.
	cancel()
	select {
	case <-done:
		t.Fatal("Start returned before the blocking notification was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(commenter.release)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Start: %v", r.err)
		}
		if r.res.Phase != journal.PhaseEscalated {
			t.Fatalf("phase = %q, want escalated", r.res.Phase)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the blocking notification was released")
	}

	if commenter.ctxErr != nil {
		t.Fatalf("UpdateWorkItem's ctx.Err() = %v, want nil — the run-level cancellation must not have propagated to the escalation notification", commenter.ctxErr)
	}
}

// TestRunnerEmitsRunTaskAndGateSpans is issue #126's runner-level acceptance:
// when Config.Telemetry is set, Start opens a run span before walking and a
// task/gate span for each stage dispatched, in walk order. Before this fix,
// Config had no Telemetry field at all and none of these were ever called.
func TestRunnerEmitsRunTaskAndGateSpans(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-span:implement": {status: apiv1.ResultSuccess},
	}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	spans := &fakeSpanStarter{}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		Telemetry:    spans,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-span",
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

	want := []string{"run:run-span", "task:implement", "gate:review"}
	if !reflect.DeepEqual(spans.calls, want) {
		t.Fatalf("span calls = %v, want %v", spans.calls, want)
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
