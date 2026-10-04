package intervention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func interventionTestMachine(t *testing.T, evaluator apiv1.EvaluatorKind) *workflow.Machine {
	t.Helper()
	return interventionTestMachineNamed(t, "intervention", evaluator, nil)
}

func interventionTestMachineNamed(t *testing.T, name string, evaluator apiv1.EvaluatorKind, approvers []string) *workflow.Machine {
	t.Helper()
	return interventionTestMachineWithPassTarget(t, name, evaluator, approvers, "finish")
}

func interventionTerminalTestMachine(t *testing.T, evaluator apiv1.EvaluatorKind) *workflow.Machine {
	t.Helper()
	return interventionTestMachineWithPassTarget(t, "terminal-intervention", evaluator, nil, workflow.TerminalComplete)
}

func interventionTestMachineWithPassTarget(
	t *testing.T,
	name string,
	evaluator apiv1.EvaluatorKind,
	approvers []string,
	passTarget string,
) *workflow.Machine {
	t.Helper()
	review := apiv1.Gate{
		Name: "review", Evaluator: evaluator,
		Branches: map[string]string{
			"pass":          passTarget,
			"fail":          workflow.TargetEscalate,
			"needs-changes": workflow.TargetEscalate,
		},
	}
	switch evaluator {
	case apiv1.EvaluatorAgentic:
		review.Agentic = &apiv1.AgenticGate{Goober: "reviewer"}
	case apiv1.EvaluatorHuman:
		review.Human = &apiv1.HumanGate{Approvers: approvers}
	default:
		t.Fatalf("unsupported test evaluator %q", evaluator)
	}
	tasks := []apiv1.Task{{
		Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
	}}
	if passTarget != workflow.TerminalComplete {
		tasks = append(tasks, apiv1.Task{
			Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
		})
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: name, Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "implement",
			Tasks: tasks,
			Gates: []apiv1.Gate{review},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func interventionParallelGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "parallel-intervention", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "fan",
			Tasks: []apiv1.Task{
				{
					Name: "branch-work", Type: apiv1.TaskDeterministic, Goal: "branch work",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
				},
				{
					Name: "other-work", Type: apiv1.TaskDeterministic, Goal: "other work",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TargetJoin,
				},
				{
					Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
				},
			},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceScratch},
				Branches: map[string]string{
					"pass":          workflow.TargetJoin,
					"fail":          workflow.TargetEscalate,
					"needs-changes": workflow.TargetEscalate,
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", Join: "collate",
				FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "review", Start: "branch-work"},
					{Name: "other", Start: "other-work"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

type interventionDeterministic struct{}

func (interventionDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// testDefinitions is the swappable definitions source the daemon's registry
// provides in production.
type testDefinitions struct {
	current atomic.Pointer[Definitions]
}

func (d *testDefinitions) Replace(definitions Definitions) { d.current.Store(&definitions) }

func (d *testDefinitions) Snapshot() Definitions { return *d.current.Load() }

// testRunnerRegistry stands in for the daemon's run-owner registry: a
// tracked live owner wins over the definitions' runner.
type testRunnerRegistry struct {
	mu     sync.Mutex
	owners map[string]*runner.Runner
}

func (r *testRunnerRegistry) Track(runID string, owner *runner.Runner) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners[runID] = owner
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.owners, runID)
	}
}

func (r *testRunnerRegistry) Resolve(runID, _ string, fallback *runner.Runner) (*runner.Runner, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.owners[runID]; ok {
		return owner, true
	}
	return fallback, false
}

func (r *testRunnerRegistry) TrackCompatible(runID string, owner *runner.Runner) (func(), bool) {
	r.mu.Lock()
	current, tracked := r.owners[runID]
	r.mu.Unlock()
	if tracked && current != owner {
		return func() {}, false
	}
	if tracked {
		return func() {}, true
	}
	return r.Track(runID, owner), true
}

// noClaims is a claim ledger for runs that claimed nothing. The daemon's real
// ledger is exercised by cmd/goobers' intervention wiring tests.
type noClaims struct{}

func (noClaims) History(string, apiv1.Provider) ([]localscheduler.ClaimEntry, error) { return nil, nil }

func (noClaims) Reclaim([]localscheduler.ClaimEntry, string, string, string) (bool, string, error) {
	return true, "", nil
}

func (noClaims) Release(string) error { return nil }

// locateTestRun finds a run under a declared gaggle, then the legacy root.
func locateTestRun(layout instance.Layout) func([]string, string, bool) (string, string, error) {
	return func(gaggles []string, runID string, includeLegacy bool) (string, string, error) {
		for _, gaggle := range gaggles {
			dir := filepath.Join(layout.ForGaggle(gaggle).RunsDir(), runID)
			if _, err := os.Stat(filepath.Join(dir, "run.yaml")); err == nil {
				return dir, gaggle, nil
			}
		}
		legacy := filepath.Join(layout.RunsDir(), runID)
		if _, err := os.Stat(filepath.Join(legacy, "run.yaml")); includeLegacy && err == nil {
			return legacy, "", nil
		}
		return "", "", httpapi.NewInterventionError(http.StatusNotFound, "run_not_found", "run was not found", nil)
	}
}

func testEngineDrivenRefusal(runID, action string) error {
	return fmt.Errorf("run %s is engine-driven: %s would edit a journal whose only writer is the engine's workflow", runID, action)
}

type interventionTestFixture struct {
	service     *Service
	runDir      string
	layout      instance.Layout
	definitions *testDefinitions
	registry    *testRunnerRegistry
}

func newInterventionServiceTestRun(
	t *testing.T,
	machine *workflow.Machine,
	runID string,
	events []journal.Event,
) (*Service, string) {
	t.Helper()
	return newInterventionServiceTestRunWithDeterministic(t, machine, runID, events, interventionDeterministic{})
}

func newInterventionServiceTestRunWithDeterministic(
	t *testing.T,
	machine *workflow.Machine,
	runID string,
	events []journal.Event,
	deterministic invoke.Deterministic,
) (*Service, string) {
	t.Helper()
	return newDriftedInterventionServiceTestRun(t, machine, machine, false, runID, events, deterministic)
}

// newDriftedInterventionServiceTestRun creates a run pinned to pinnedMachine
// while the definitions registry serves servedMachine — the #3376 shape: the
// workflow config was edited (or replaced wholesale) between the run's start
// and the operator's intervention. When snapshotDefinition is true the run
// carries the trusted pinned-definition input that Runner.Start journals;
// false reproduces a pre-snapshot run whose pin cannot be reconstructed.
func newDriftedInterventionServiceTestRun(
	t *testing.T,
	pinnedMachine, servedMachine *workflow.Machine,
	snapshotDefinition bool,
	runID string,
	events []journal.Event,
	deterministic invoke.Deterministic,
) (*Service, string) {
	t.Helper()
	fixture := newInterventionTestFixture(t, pinnedMachine, servedMachine, snapshotDefinition, runID, events, deterministic)
	return fixture.service, fixture.runDir
}

func newInterventionTestFixture(
	t *testing.T,
	pinnedMachine, servedMachine *workflow.Machine,
	snapshotDefinition bool,
	runID string,
	events []journal.Event,
	deterministic invoke.Deterministic,
) *interventionTestFixture {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	scoped := layout.ForGaggle("example")
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	manager, err := worktree.NewManager(scoped.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  manager,
		ScratchDir: filepath.Join(scoped.WorkcopiesDir(), "scratch"),
		RunsDir:    scoped.RunsDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var inputs map[string][]byte
	var createOpts []journal.Option
	if snapshotDefinition {
		definition, err := json.Marshal(pinnedMachine.Def)
		if err != nil {
			t.Fatal(err)
		}
		inputs = map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition}
		createOpts = append(createOpts, journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	}
	run, err := journal.Create(scoped.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: pinnedMachine.Def.Name, WorkflowVersion: pinnedMachine.Def.Version,
		WorkflowDigest: pinnedMachine.Digest(), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, inputs, createOpts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	key := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: servedMachine.Def.Name}
	definitions := &testDefinitions{}
	definitions.Replace(Definitions{
		Runners:       map[string]*runner.Runner{"example": runRunner},
		Machines:      map[localscheduler.WorkflowIdentity]*workflow.Machine{key: servedMachine},
		GooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""},
		RepoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{
			key: {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"},
		},
	})
	registry := &testRunnerRegistry{owners: map[string]*runner.Runner{}}
	service := New(Config{
		Definitions: definitions.Snapshot,
		Runners:     registry,
		PinnedExecution: func(context.Context, journal.RunIdentity) (Execution, error) {
			return Execution{}, errors.New("no pinned execution generations in this fixture")
		},
		LocateRun:           locateTestRun(layout),
		Claims:              noClaims{},
		EngineDrivenRefusal: testEngineDrivenRefusal,
	})
	service.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: servedMachine.Def.Name,
		Gaggle:   "example",
		Readiness: apiv1.ReadinessConditions{
			MaxConcurrentRuns: 1,
		},
	}}, instanceLog))
	return &interventionTestFixture{
		service: service, runDir: filepath.Join(scoped.RunsDir(), runID),
		layout: layout, definitions: definitions, registry: registry,
	}
}

func TestRunInterventionTerminalBranchesComplete(t *testing.T) {
	for _, action := range []string{"approve", "override"} {
		t.Run(action, func(t *testing.T) {
			machine := interventionTerminalTestMachine(t, apiv1.EvaluatorAgentic)
			runID := "run-terminal-" + action
			service, runDir := newInterventionServiceTestRun(t, machine, runID, []journal.Event{
				{Type: journal.EventGateStarted, Gate: "review"},
				{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
				{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
			})
			input := httpapi.InterventionRequest{
				RunID: runID, Stage: "review", Actor: "operator", Decision: "pass",
				Rationale: "accepted terminal outcome",
			}

			var (
				result httpapi.InterventionResult
				err    error
			)
			if action == "approve" {
				result, err = service.Approve(context.Background(), input)
			} else {
				result, err = service.Override(context.Background(), input)
			}
			if err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			if result.Phase != string(journal.PhaseCompleted) {
				t.Fatalf("result = %+v, want completed", result)
			}

			reader, err := journal.OpenRead(runDir)
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			var resumed journal.Event
			for _, event := range events {
				if event.Type == journal.EventRunResumed {
					resumed = event
				}
			}

			if resumed.Type != journal.EventRunResumed ||
				resumed.Action != action ||
				resumed.Target != "" ||
				!resumed.Complete {
				t.Fatalf("run.resumed = %+v", resumed)
			}
		})
	}
}

func TestRunInterventionOverrideReopensParallelBranchAtJoin(t *testing.T) {
	machine := interventionParallelGateMachine(t)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-parallel-override", []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan"},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "review", Stage: "branch-work"},
		{Type: journal.EventStageStarted, Branch: 1, Stage: "branch-work", Attempt: 1},
		{Type: journal.EventStageFinished, Branch: 1, Stage: "branch-work", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Branch: 1, Gate: "review"},
		{Type: journal.EventGateEvaluated, Branch: 1, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 1, BranchName: "review", BranchStatus: journal.BranchFailed},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 2, BranchName: "other", BranchStatus: journal.BranchCancelled},
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})

	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-parallel-override", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "accepted branch result",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var resumed journal.Event
	otherStarted := false
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			resumed = event
		}
		if event.Type == journal.EventBranchStarted && event.Branch == 2 {
			otherStarted = true
		}
	}
	if resumed.Target != workflow.TargetJoin || resumed.Parallel != "fan" || resumed.Branch != 1 {
		t.Fatalf("run.resumed = %+v", resumed)
	}
	if !otherStarted {
		t.Fatal("parallel sibling was not resumed after the approved branch joined")
	}
}

func TestRunInterventionOverrideResumesAndJournalsAction(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-override", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-override", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "accepted after manual review",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var resumed journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			resumed = event
		}
	}
	if resumed.Type != journal.EventRunResumed ||
		resumed.Actor != "operator" ||
		resumed.Target != "finish" ||
		resumed.Action != "override" ||
		resumed.Gate != "review" ||
		resumed.Decision != "pass" ||
		resumed.Rationale != "accepted after manual review" {
		t.Fatalf("run.resumed = %+v", resumed)
	}
}

func TestRunInterventionIdempotencyReplaysCompletedAction(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-idempotent", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	input := httpapi.InterventionRequest{
		RunID: "run-idempotent", Stage: "review", Actor: "operator", Decision: "pass",
		Rationale: "accepted after manual review", IdempotencyKey: "same-request",
	}
	first, err := service.Override(context.Background(), input)
	if err != nil {
		t.Fatalf("first Override: %v", err)
	}
	second, err := service.Override(context.Background(), input)
	if err != nil {
		t.Fatalf("replayed Override: %v", err)
	}
	if second != first || first.JournalSeq == 0 {
		t.Fatalf("results = first %+v, second %+v", first, second)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	markers, resumed := 0, 0
	for _, event := range events {
		if interventionMarkerKey(event) == input.IdempotencyKey {
			markers++
		}
		if event.Type == journal.EventRunResumed && event.Action == "override" {
			resumed++
		}
	}
	if markers != 1 || resumed != 1 {
		t.Fatalf("markers = %d, resumed = %d; want one each", markers, resumed)
	}

	input.Rationale = "different action"
	_, err = service.Override(context.Background(), input)
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Code != "idempotency_key_reused" {
		t.Fatalf("key reuse error = %#v, want idempotency_key_reused", err)
	}
}

func TestPrepareInterventionReplaysCompletedActions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     hitlAction
		completion journal.Event
		input      httpapi.InterventionRequest
	}{
		{
			name:       "approve",
			action:     hitlActionApprove,
			completion: journal.Event{Type: journal.EventGateEvaluated, Gate: "review"},
			input:      httpapi.InterventionRequest{Stage: "review", Decision: "pass"},
		},
		{
			name:       "override",
			action:     hitlActionOverride,
			completion: journal.Event{Type: journal.EventRunResumed, Action: "override", Gate: "review"},
			input:      httpapi.InterventionRequest{Stage: "review", Decision: "pass", Rationale: "manual review"},
		},
		{
			name:       "rerun",
			action:     hitlActionRerun,
			completion: journal.Event{Type: journal.EventStageRerunRequested, Stage: "implement"},
			input:      httpapi.InterventionRequest{Stage: "implement", InstructionAddendum: "try again"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-replay-" + tc.name
			input := tc.input
			input.RunID = runID
			input.IdempotencyKey = "same-request"
			fingerprint, err := interventionFingerprint(tc.name, input)
			if err != nil {
				t.Fatal(err)
			}
			service, _ := newInterventionServiceTestRun(t, interventionTestMachine(t, apiv1.EvaluatorAgentic), runID, []journal.Event{
				{
					Type: journal.EventRunnerAnnotation,
					Runner: map[string]any{
						"kind":           interventionIdempotencyMarker,
						"idempotencyKey": input.IdempotencyKey,
						"fingerprint":    fingerprint,
					},
				},
				tc.completion,
				{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
			})

			_, result, done, err := service.prepareIntervention(context.Background(), tc.action, tc.name, input)
			if err != nil {
				t.Fatalf("prepareIntervention: %v", err)
			}
			if !done || result == nil || result.Phase != string(journal.PhaseCompleted) {
				t.Fatalf("done = %t, result = %+v; want replayed completed result", done, result)
			}
		})
	}
}

func TestRunInterventionApproveResolvesPausedHumanGate(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorHuman)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-approve", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGatePaused, Gate: "review"},
	})
	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-approve", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var evaluated journal.Event
	for _, event := range events {
		if event.Type == journal.EventGateEvaluated {
			evaluated = event
		}
	}
	if evaluated.Gate != "review" || evaluated.Verdict != "pass" || evaluated.Target != "finish" || evaluated.Actor != "approver" {
		t.Fatalf("gate.evaluated = %+v", evaluated)
	}
}

// interventionGoalDriftMachine compiles the same human-gated workflow shape
// with a caller-chosen goal for the implement task — two goals, two digests,
// one workflow name: the #3376 residual case, a legitimate semantic edit
// landing between a run's start and an operator's intervention.
func interventionGoalDriftMachine(t *testing.T, goal string) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "intervention", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "implement",
			Tasks: []apiv1.Task{
				{
					Name: "implement", Type: apiv1.TaskDeterministic, Goal: goal,
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
				},
				{
					Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
				},
			},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorHuman,
				Human: &apiv1.HumanGate{},
				Branches: map[string]string{
					"pass": "finish",
					"fail": workflow.TargetEscalate,
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

// TestRunInterventionApproveResumesDriftedRunFromPinnedDefinition is the
// #3376 residual-case regression: the workflow was legitimately edited after
// this run paused at its human gate, so the served machine's digest no longer
// matches the run's WF-016 pin. Before the fix the operator's approve was
// executed against the current machine and refuseResume destroyed the paused
// run (terminal failed, resume_refused_digest_mismatch); with the pinned
// definition snapshot journaled at Start, resolve() must reconstruct the
// historical machine and the approval must walk the run to completion.
func TestRunInterventionApproveResumesDriftedRunFromPinnedDefinition(t *testing.T) {
	pinned := interventionGoalDriftMachine(t, "implement")
	edited := interventionGoalDriftMachine(t, "implement, but the goal was edited mid-run")
	if pinned.Digest() == edited.Digest() {
		t.Fatal("fixture machines must have drifted digests")
	}
	service, runDir := newDriftedInterventionServiceTestRun(t, pinned, edited, true, "run-drifted-approve", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGatePaused, Gate: "review"},
	}, interventionDeterministic{})

	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-drifted-approve", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v — a drifted-but-snapshotted run must resume against its pinned definition, not refuse", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finished = event
		}
	}
	if finished.Status != string(journal.PhaseCompleted) || finished.Error != nil {
		t.Fatalf("run.finished = %+v, want clean completed terminal", finished)
	}
}

// TestRunInterventionTerminalApproveResumesDriftedRunFromPinnedDefinition
// covers the second intervention entrypoint, ResumeFromTerminal: an escalated
// run whose workflow was edited afterwards must still be reopenable by a
// human against its pinned definition (WF-016 pin verified against the
// reconstructed machine, not the drifted current one).
func TestRunInterventionTerminalApproveResumesDriftedRunFromPinnedDefinition(t *testing.T) {
	pinned := interventionGoalDriftMachine(t, "implement")
	edited := interventionGoalDriftMachine(t, "implement, but the goal was edited mid-run")
	service, runDir := newDriftedInterventionServiceTestRun(t, pinned, edited, true, "run-drifted-terminal", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, interventionDeterministic{})

	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-drifted-terminal", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v — terminal resume must verify the pin against the reconstructed machine", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finished = event
		}
	}
	if finished.Status != string(journal.PhaseCompleted) {
		t.Fatalf("final run.finished = %+v, want completed", finished)
	}
}

// TestRunInterventionApproveStillRefusesDriftedRunWithoutSnapshot pins the
// tamper-evidence property (#3376 scope: refusal MUST remain when the old
// definition is unavailable): when the run's pin cannot be reconstructed from
// a trusted snapshot, the intervention proceeds against the current machine
// and the runner's WF-016 verification refuses exactly as before.
func TestRunInterventionApproveStillRefusesDriftedRunWithoutSnapshot(t *testing.T) {
	pinned := interventionGoalDriftMachine(t, "implement")
	edited := interventionGoalDriftMachine(t, "implement, but the goal was edited mid-run")
	service, runDir := newDriftedInterventionServiceTestRun(t, pinned, edited, false, "run-drifted-unpinned", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGatePaused, Gate: "review"},
	}, interventionDeterministic{})

	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-drifted-unpinned", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v — a handled WF-016 refusal is a result, not an error", err)
	}
	if result.Phase != string(journal.PhaseFailed) {
		t.Fatalf("result = %+v, want the WF-016 refusal's canonical failed terminal", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finished = event
		}
	}
	if finished.Error == nil || finished.Error.Code != "resume_refused_digest_mismatch" {
		t.Fatalf("run.finished error = %+v, want resume_refused_digest_mismatch — tamper evidence must survive the pinned-definition fallback", finished.Error)
	}
}

func TestRunInterventionRejectsInvalidStateAndInput(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, "run-complete", []journal.Event{
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	})

	_, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-complete", Stage: "review", Actor: "operator",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Status != http.StatusBadRequest || interventionErr.Code != "rationale_required" {
		t.Fatalf("missing-rationale error = %#v", err)
	}

	wrongPhase := []struct {
		name string
		call func() error
		code string
	}{
		{
			name: "approve",
			call: func() error {
				_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
					RunID: "run-complete", Stage: "review", Actor: "operator",
				})
				return err
			},
			code: "run_not_intervenable",
		},
		{
			name: "override",
			call: func() error {
				_, err := service.Override(context.Background(), httpapi.InterventionRequest{
					RunID: "run-complete", Stage: "review", Actor: "operator", Rationale: "manual review",
				})
				return err
			},
			code: "run_not_escalated",
		},
		{
			name: "rerun",
			call: func() error {
				_, err := service.RerunStage(context.Background(), httpapi.InterventionRequest{
					RunID: "run-complete", Stage: "implement", Actor: "operator", InstructionAddendum: "try again",
				})
				return err
			},
			code: "run_not_escalated",
		},
	}
	for _, tc := range wrongPhase {
		t.Run(tc.name+"_wrong_phase", func(t *testing.T) {
			err := tc.call()
			var interventionErr *httpapi.InterventionError
			if !errors.As(err, &interventionErr) ||
				interventionErr.Status != http.StatusConflict ||
				interventionErr.Code != tc.code {
				t.Fatalf("wrong-phase error = %#v, want %s", err, tc.code)
			}
		})
	}

	_, err = service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "../escape", Stage: "review", Actor: "operator",
	})
	if !errors.As(err, &interventionErr) || interventionErr.Status != http.StatusBadRequest || interventionErr.Code != "invalid_run_id" {
		t.Fatalf("invalid-run error = %#v", err)
	}
}

func TestFinishInterventionMapsExecutionErrors(t *testing.T) {
	cause := errors.New("runner failed")
	for _, tc := range []struct {
		action  string
		message string
	}{
		{action: "approve", message: "approve failed while advancing the run"},
		{action: "override", message: "override failed while advancing the run"},
		{action: "rerun stage", message: "rerun stage failed while advancing the run"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			result, err := finishIntervention(tc.action, resolvedInterventionRun{}, cause)
			if result != (httpapi.InterventionResult{}) {
				t.Fatalf("result = %+v, want empty", result)
			}
			var interventionErr *httpapi.InterventionError
			if !errors.As(err, &interventionErr) ||
				interventionErr.Status != http.StatusInternalServerError ||
				interventionErr.Code != "intervention_failed" ||
				interventionErr.Message != tc.message ||
				!errors.Is(err, cause) {
				t.Fatalf("execution error = %#v, want intervention_failed with message %q", err, tc.message)
			}
		})
	}
}

func TestRunInterventionApproveRejectsUnauthorizedTerminalHumanActor(t *testing.T) {
	machine := interventionTestMachineNamed(t, "restricted-intervention", apiv1.EvaluatorHuman, []string{"allowed"})
	service, runDir := newInterventionServiceTestRun(t, machine, "run-restricted", []journal.Event{
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})

	_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-restricted", Stage: "review", Actor: "intruder", Decision: "pass",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) ||
		interventionErr.Status != http.StatusForbidden ||
		interventionErr.Code != "approval_forbidden" {
		t.Fatalf("Approve error = %#v, want approval_forbidden", err)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1]; got.Type != journal.EventRunFinished || got.Status != string(journal.PhaseEscalated) {
		t.Fatalf("last event = %+v, want original escalated terminal", got)
	}
}

func TestRunInterventionUsesDefinitionsReplacedAfterReload(t *testing.T) {
	initial := interventionTestMachineNamed(t, "initial-intervention", apiv1.EvaluatorAgentic, nil)
	fixture := newInterventionTestFixture(t, initial, initial, false, "run-initial", []journal.Event{
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	}, interventionDeterministic{})
	service := fixture.service
	reloaded := interventionTestMachineNamed(t, "reloaded-intervention", apiv1.EvaluatorAgentic, nil)
	snapshot := fixture.definitions.Snapshot()
	key := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: reloaded.Def.Name}
	fixture.definitions.Replace(Definitions{
		Runners:       snapshot.Runners,
		Machines:      map[localscheduler.WorkflowIdentity]*workflow.Machine{key: reloaded},
		GooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""},
		RepoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{
			key: {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"},
		},
	})
	if err := service.scheduler.Load().Reload([]localscheduler.WorkflowEntry{{
		Workflow: reloaded.Def.Name,
		Gaggle:   "example",
		Readiness: apiv1.ReadinessConditions{
			MaxConcurrentRuns: 1,
		},
	}}, nil, time.Now(), "old", "new"); err != nil {
		t.Fatal(err)
	}

	const runID = "run-after-reload"
	run, err := journal.Create(fixture.layout.ForGaggle("example").RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: reloaded.Def.Name, WorkflowVersion: reloaded.Def.Version,
		WorkflowDigest: reloaded.Digest(), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "reviewed after reload",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want post-reload run completed", result)
	}
}

func TestRunInterventionRejectsDelayedDuplicateFromPriorTerminalSegment(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-delayed-duplicate", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	stale, err := service.resolve("run-delayed-duplicate")
	if err != nil {
		t.Fatal(err)
	}

	recovered, _, err := journal.Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventRunResumed, Status: string(journal.PhaseEscalated),
			Actor: "first-operator", Action: "override", Gate: "review", Decision: "pass", Target: "implement",
			WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(),
		},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	} {
		if err := recovered.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = service.execute(context.Background(), context.Background(), false, stale, true, "override", httpapi.InterventionRequest{}, func(ctx context.Context) (runner.Result, error) {
		return stale.runner.ResumeFromTerminal(ctx, runner.ResumeFromTerminalInput{
			RunID: stale.runID, Machine: stale.machine, GooberDigest: stale.gooberDigest, RepoRef: stale.repoRef,
			Target: "finish", Actor: "delayed-operator", Action: "override", Gate: "review", Decision: "pass",
			Rationale: "stale approval", ExpectedTerminalSeq: stale.terminalSeq,
		})
	})
	err = interventionExecutionError("override", err)
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) ||
		interventionErr.Status != http.StatusConflict ||
		interventionErr.Code != "terminal_generation_changed" {
		t.Fatalf("delayed duplicate error = %#v, want terminal_generation_changed", err)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	resumes := 0
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			resumes++
		}
	}
	if resumes != 1 {
		t.Fatalf("run.resumed events = %d, want no stale duplicate", resumes)
	}
}

func TestRunInterventionRejectsGateEvidenceFromBeforeRerunSegment(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, "run-rerun-failed", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
		{
			Type: journal.EventStageRerunRequested, Stage: "implement", Actor: "first-operator",
			InstructionAddendum: "try a different implementation",
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Status: string(apiv1.ResultFailure)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)},
	})

	_, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-rerun-failed", Stage: "review", Actor: "second-operator",
		Decision: "pass", Rationale: "reuse the earlier review",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) ||
		interventionErr.Status != http.StatusConflict ||
		interventionErr.Code != "gate_not_evaluated" {
		t.Fatalf("Override error = %#v, want gate_not_evaluated", err)
	}
}

func TestRunInterventionResolvePrefersLiveOwnerAcrossReload(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	fixture := newInterventionTestFixture(t, machine, machine, false, "run-live-owner", []journal.Event{
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	}, interventionDeterministic{})
	service := fixture.service
	snapshot := fixture.definitions.Snapshot()
	original := snapshot.Runners["example"]
	reloaded := &runner.Runner{}
	snapshot.Runners = map[string]*runner.Runner{"example": reloaded}
	fixture.definitions.Replace(snapshot)
	untrack := fixture.registry.Track("run-live-owner", original)
	defer untrack()

	resolved, err := service.resolve("run-live-owner")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.runner != original {
		t.Fatalf("resolved runner = %p, want live owner %p", resolved.runner, original)
	}
}
