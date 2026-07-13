package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated: automated,
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
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
	// in-memory Result — this is what a resume (deliverable B) will replay.
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
