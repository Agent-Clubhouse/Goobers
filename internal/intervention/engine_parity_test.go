package intervention

// Parity row HITL-intents (HA-P4-12, #7760): an operator's override, rerun
// and deny on an escalated run journal the same normative record whether the
// local runner or the engine drives the run.
//
// Both sides walk the same workflow to the same escalation for real. The
// runner side then takes the production intervention path; the engine side
// receives exactly the intent this service's engine route produces, through
// the workflow's own Update, and the two journals are compared through
// journal.ConformanceView.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

const parityRunID = "run-hitl-parity"

func hitlParitySpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "example", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Workspace: apiv1.WorkspaceScratch, Next: "review"},
			{Name: "ship", Type: apiv1.TaskDeterministic, Goal: "ship", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete},
		},
		Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			Branches: map[string]string{"pass": "ship", "fail": workflow.TargetAbort, "needs-changes": workflow.TargetEscalate}}},
	}
}

// scriptedReviewer escalates on its first review and passes afterwards, on
// both sides, so a rerun returns through a passing gate.
type scriptedReviewer struct {
	mu      sync.Mutex
	reviews int
}

func (r *scriptedReviewer) verdict() apiv1.Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reviews++
	if r.reviews == 1 {
		return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "needs work"}
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "looks good"}
}

func (r *scriptedReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (r *scriptedReviewer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return r.verdict(), nil
}

type succeedingDeterministic struct{}

func (succeedingDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"}, nil
}

func parityMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{Name: "hitl-parity", Version: 1, DSLVersion: "3.1", Spec: hitlParitySpec()}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

// runnerSide escalates the workflow on the local runner, applies the
// intervention through the production service and returns the journal.
func runnerSide(t *testing.T, act func(*Service) error) []journal.Event {
	t.Helper()
	machine := parityMachine(t)
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
	reviewer := &scriptedReviewer{}
	fixture := localFixtureRepo(t)
	r, err := runner.New(runner.Config{
		// Never reach a real remote: the gate's workspace clones this repo.
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixture, nil },
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return succeedingDeterministic{}, nil
		},
		NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  manager,
		ScratchDir: filepath.Join(scoped.WorkcopiesDir(), "scratch"),
		RunsDir:    scoped.RunsDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"}
	result, err := r.Start(t.Context(), runner.StartInput{RunID: parityRunID, Machine: machine, Gaggle: "example", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo})
	if err != nil || result.Phase != journal.PhaseEscalated {
		t.Fatalf("runner did not escalate: %+v %v", result, err)
	}
	key := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: machine.Def.Name}
	definitions := &testDefinitions{}
	definitions.Replace(Definitions{Runners: map[string]*runner.Runner{"example": r}, Machines: map[localscheduler.WorkflowIdentity]*workflow.Machine{key: machine},
		GooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""}, RepoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{key: repo}})
	service := New(Config{Definitions: definitions.Snapshot, Runners: &testRunnerRegistry{owners: map[string]*runner.Runner{}}, LocateRun: locateTestRun(layout),
		Claims: noClaims{}, EngineDrivenRefusal: testEngineDrivenRefusal})
	service.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: machine.Def.Name, Gaggle: "example", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}}}, instanceLog))
	if err := act(service); err != nil {
		t.Fatalf("runner intervention: %v", err)
	}
	reader, err := journal.OpenRead(filepath.Join(scoped.RunsDir(), parityRunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// localFixtureRepo is a bare one-commit repository on disk.
func localFixtureRepo(t *testing.T) string {
	t.Helper()
	work, bare := t.TempDir(), filepath.Join(t.TempDir(), "fixture.git")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := testgit.Command(args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(work, "init", "--initial-branch=main")
	git(work, "config", "user.email", "test@example.com")
	git(work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "README.md")
	git(work, "commit", "-m", "initial")
	git(work, "clone", "--bare", work, bare)
	return bare
}

type updateOutcome struct{ err error }

func (u *updateOutcome) Accept()          {}
func (u *updateOutcome) Reject(err error) { u.err = err }
func (u *updateOutcome) Complete(_ any, err error) {
	if err != nil {
		u.err = err
	}
}

// engineSide escalates the same workflow on the engine and delivers the
// intent the service's engine route built for the same operator request.
func engineSide(t *testing.T, intent engine.HITLIntent) []journal.Event {
	t.Helper()
	machine := parityMachine(t)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.SetStartTime(time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC))
	env.RegisterWorkflow(engine.Run)
	runsDir := filepath.Join(t.TempDir(), "runs")
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runsDir, gaggle == "example" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	env.RegisterActivity((&engine.Activities{Journal: writer}).EmitJournal)
	reviewer := &scriptedReviewer{}
	env.RegisterActivityWithOptions(func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun, string, string) (engine.DispatchStageResult, error) {
		return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"}}, nil
	}, activity.RegisterOptions{Name: engine.ActRunDeterministic})
	env.RegisterActivityWithOptions(func(context.Context, apiv1.InvocationEnvelope, string, string, apiv1.WorkspaceMode, string) (engine.DispatchStageResult, error) {
		return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	}, activity.RegisterOptions{Name: engine.ActInvokeGoober})
	env.RegisterActivityWithOptions(func(context.Context, apiv1.InvocationEnvelope, string, string, apiv1.WorkspaceMode, string, bool) (engine.GateReviewResult, error) {
		return engine.GateReviewResult{Verdict: reviewer.verdict()}, nil
	}, activity.RegisterOptions{Name: engine.ActReviewGoober})
	preview := true
	in := engine.RunInput{RunID: parityRunID, Gaggle: "example", WorkflowName: machine.Def.Name, Version: 1, DSLVersion: "3.1", WorkflowDigest: machine.Digest(),
		Spec: machine.Def.Spec, PreviewFeaturesEnabled: &preview, TriggerKind: string(journal.TriggerManual),
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"}, HITL: &engine.HITLPolicy{Enabled: true, WaitSeconds: 3600},
		LiveJournal: true}
	intent.RunID = parityRunID
	outcome := &updateOutcome{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(engine.HITLUpdateName, engine.HITLIntentUpdateID(intent), outcome, intent)
	}, time.Minute)
	env.ExecuteWorkflow(engine.Run, in)
	if outcome.err != nil {
		t.Fatalf("engine refused the routed intent: %v", outcome.err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, parityRunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// routedIntent is what this service sends the engine for request: the same
// production mapping, captured instead of delivered.
func routedIntent(t *testing.T, act func(*Service, string) error) engine.HITLIntent {
	t.Helper()
	decisions := &recordingEngine{hitl: engine.HITLState{Enabled: true, Phase: "awaiting-operator", TerminalGeneration: 1}}
	if err := act(engineRunService(t, decisions), "engine-run"); err != nil {
		t.Fatal(err)
	}
	if len(decisions.intents) != 1 {
		t.Fatalf("engine route sent %d intents", len(decisions.intents))
	}
	return decisions.intents[0]
}

func TestParityRowHITLIntentsJournalTheSameRecord(t *testing.T) {
	cases := map[string]func(*Service, string) error{
		"override": func(s *Service, run string) error {
			_, err := s.Override(t.Context(), httpapi.InterventionRequest{RunID: run, Stage: "review", Actor: "ops@example.com", IdempotencyKey: "key-override", Decision: "pass", Rationale: "false positive"})
			return err
		},
		"rerun": func(s *Service, run string) error {
			_, err := s.RerunStage(t.Context(), httpapi.InterventionRequest{RunID: run, Stage: "implement", Actor: "ops@example.com", IdempotencyKey: "key-rerun", InstructionAddendum: "address the review"})
			return err
		},
		"deny": func(s *Service, run string) error {
			_, err := s.AcceptDenyEscalation(t.Context(), t.Context(), httpapi.InterventionRequest{RunID: run, Stage: "review", Actor: "ops@example.com", IdempotencyKey: "key-deny", Rationale: "not safe to ship"})
			return err
		},
	}
	for name, act := range cases {
		t.Run(name, func(t *testing.T) {
			local := runnerSide(t, func(s *Service) error { return act(s, parityRunID) })
			remote := engineSide(t, routedIntent(t, act))
			lv, ev := journal.ConformanceView(local), journal.ConformanceView(remote)
			for i := 0; i < max(len(lv), len(ev)); i++ {
				var l, e journal.NormativeEvent
				if i < len(lv) {
					l = lv[i]
				}
				if i < len(ev) {
					e = ev[i]
				}
				if l != e {
					t.Fatalf("%s journals diverge at normative event %d of %d/%d:\n runner: %+v\n engine: %+v", name, i, len(lv), len(ev), l, e)
				}
			}
			// Premise: the intervention actually happened on the runner.
			if len(lv) < 2 || len(local) <= len(lv) {
				t.Fatalf("premise: runner journal too short to show an intervention: %v", lv)
			}
		})
	}
}
