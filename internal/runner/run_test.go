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
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// --- stub executors/evaluators (the "stub stage effects" issue #17 asks for) ---

// stubExecutor returns a canned StageOutput per task name, and echoes an
// artifact whose content is the task's own name so downstream stages have
// something concrete to resolve.
type stubExecutor struct {
	byTask map[string]StageOutput
}

func (s stubExecutor) Execute(_ context.Context, req StageRequest) (StageOutput, error) {
	name := req.Envelope.TaskID
	if out, ok := s.byTask[name]; ok {
		return out, nil
	}
	return StageOutput{}, fmt.Errorf("stub executor: no canned output for %q", name)
}

// stubGateEvaluator always returns the same verdict.
type stubGateEvaluator struct {
	verdict apiv1.Verdict
}

func (s stubGateEvaluator) Evaluate(context.Context, StageRequest) (apiv1.Verdict, error) {
	return s.verdict, nil
}

type nopRegistrar struct{}

func (nopRegistrar) Register([]byte) {}

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

// --- fixture workflow: implement -> review (gate) -> pass/fail/needs-changes ---

func fixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "tests-pass"},
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

func newTestRunner(t *testing.T, machine *workflow.Machine, executors Executors, gates GateEvaluators) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, nil, nopRegistrar{})
	if err != nil {
		t.Fatalf("new injector: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")

	fixtureRepo := newFixtureRepo(t)

	r, err := New(Config{
		Machine:     machine,
		Executors:   executors,
		Gates:       gates,
		Worktrees:   wtMgr,
		Credentials: injector,
		RunsDir:     runsDir,
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
	executors := Executors{
		apiv1.TaskDeterministic: stubExecutor{byTask: map[string]StageOutput{
			"run-1:implement": {
				Result:   apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "wrote a diff"},
				Produced: []ProducedArtifact{{Name: "diff", Data: []byte("--- a\n+++ b\n"), MediaType: "text/x-patch"}},
			},
		}},
	}
	gates := GateEvaluators{
		apiv1.EvaluatorAutomated: stubGateEvaluator{verdict: apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "tests pass"}},
	}
	r, runsDir := newTestRunner(t, machine, executors, gates)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
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
	executors := Executors{
		apiv1.TaskDeterministic: stubExecutor{byTask: map[string]StageOutput{
			"run-2:implement": {Result: apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}}},
		}},
	}
	gates := GateEvaluators{
		apiv1.EvaluatorAutomated: stubGateEvaluator{verdict: apiv1.Verdict{Decision: apiv1.VerdictFail, Summary: "tests fail"}},
	}
	r, _ := newTestRunner(t, machine, executors, gates)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-2",
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
	r, runsDir := newTestRunner(t, machine, Executors{}, GateEvaluators{})

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-3",
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
