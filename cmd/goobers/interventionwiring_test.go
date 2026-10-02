package main

// interventionwiring_test.go drives internal/intervention through the
// daemon's real collaborators: the claim ledger and its history, the run-owner
// registry, the owned-run locator and the engine-driven refusal. The service's
// own behaviour is tested in internal/intervention.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/intervention"
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

func interventionTwoGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "two-gate-intervention", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "implement",
			Tasks: []apiv1.Task{
				{
					Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
				},
				{
					Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
				},
			},
			Gates: []apiv1.Gate{
				{
					Name: "review", Evaluator: apiv1.EvaluatorAgentic,
					Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
					Branches: map[string]string{
						"pass":          "approval",
						"fail":          workflow.TargetEscalate,
						"needs-changes": workflow.TargetEscalate,
					},
				},
				{
					Name: "approval", Evaluator: apiv1.EvaluatorHuman,
					Human: &apiv1.HumanGate{},
					Branches: map[string]string{
						"pass": "finish",
						"fail": workflow.TargetEscalate,
					},
				},
			},
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

type interventionWiringFixture struct {
	service        *intervention.Service
	runDir         string
	layout         instance.Layout
	instanceLog    *journal.InstanceLog
	scheduler      *localscheduler.Scheduler
	runnerRegistry *daemonRunnerRegistry
	definitions    *interventionDefinitionRegistry
}

func newInterventionServiceTestRun(
	t *testing.T,
	machine *workflow.Machine,
	runID string,
	events []journal.Event,
) (*intervention.Service, string) {
	t.Helper()
	fixture := newInterventionWiringFixture(t, machine, runID, events, interventionDeterministic{}, nil)
	return fixture.service, fixture.runDir
}

// newInterventionWiringFixture builds the service the way up.go does, over a
// retained run of machine. configure, when set, adjusts the daemon-bound
// config before the service is constructed.
func newInterventionWiringFixture(
	t *testing.T,
	machine *workflow.Machine,
	runID string,
	events []journal.Event,
	deterministic invoke.Deterministic,
	configure func(*intervention.Config),
) *interventionWiringFixture {
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
		FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
			return releaseClaimsForRun(layout, instanceLog, runID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(scoped.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
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
	key := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: machine.Def.Name}
	runners := map[string]*runner.Runner{"example": runRunner}
	runnerRegistry := newDaemonRunnerRegistry()
	runnerRegistry.Replace(runners)
	definitions := newInterventionDefinitionRegistry(interventionDefinitionSet{
		runners:       runners,
		machines:      map[localscheduler.WorkflowIdentity]*workflow.Machine{key: machine},
		gooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""},
		repoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{
			key: {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"},
		},
	})
	cfg := interventionServiceConfig(layout, definitions, runnerRegistry, instanceLog, nil, nil)
	if configure != nil {
		configure(&cfg)
	}
	service := intervention.New(cfg)
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: machine.Def.Name,
		Gaggle:   "example",
		Readiness: apiv1.ReadinessConditions{
			MaxConcurrentRuns: 1,
		},
	}}, instanceLog)
	service.AttachScheduler(scheduler)
	return &interventionWiringFixture{
		service: service, runDir: filepath.Join(scoped.RunsDir(), runID),
		layout: layout, instanceLog: instanceLog, scheduler: scheduler,
		runnerRegistry: runnerRegistry, definitions: definitions,
	}
}

type blockingInterventionDeterministic struct {
	started chan struct{}
}

func (d *blockingInterventionDeterministic) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	close(d.started)
	<-ctx.Done()
	return apiv1.ResultEnvelope{}, ctx.Err()
}

type releasableInterventionDeterministic struct {
	started chan struct{}
	release chan struct{}
}

func (d *releasableInterventionDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	close(d.started)
	<-d.release
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func TestRunInterventionRegistersLiveOwnerForCancellation(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	deterministic := &blockingInterventionDeterministic{started: make(chan struct{})}
	fixture := newInterventionWiringFixture(t, machine, "run-cancellable", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, deterministic, nil)
	service := fixture.service

	result, err := service.AcceptOverride(context.Background(), context.Background(), httpapi.InterventionRequest{
		RunID: "run-cancellable", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "continue under observation", IdempotencyKey: "cancellable-override",
	})
	if err != nil {
		t.Fatalf("AcceptOverride: %v", err)
	}
	if result.JournalSeq == 0 {
		t.Fatalf("accepted result = %+v, want durable journal position", result)
	}
	select {
	case <-deterministic.started:
	case <-time.After(5 * time.Second):
		t.Fatal("intervention did not start resumed stage")
	}

	response := executeCancelRequest(fixture.runnerRegistry, nil, cancelRequest{
		RunID: "run-cancellable", Gaggle: "example", Actor: "operator",
	}, time.Now())
	if response.Code != cancelCodeAborted || response.Phase != string(journal.PhaseAborted) {
		t.Fatalf("cancel response = %+v, want aborted", response)
	}
	deadline := time.Now().Add(5 * time.Second)
	for service.Active("run-cancellable") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if service.Active("run-cancellable") {
		t.Fatal("accepted intervention did not stop after cancellation")
	}
}

func TestRunInterventionReacquiresClaimsAndAdmissionUntilTerminal(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	deterministic := &releasableInterventionDeterministic{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	fixture := newInterventionWiringFixture(t, machine, "run-reserved", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, deterministic, nil)
	service := fixture.service
	if err := fixture.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-reserved", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.instanceLog.Append(journal.Event{
		Type: journal.EventClaimReleased, Name: "466", Gaggle: "example",
		RunID: "run-reserved", Workflow: machine.Def.Name,
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := service.Override(context.Background(), httpapi.InterventionRequest{
			RunID: "run-reserved", Stage: "review", Actor: "operator",
			Decision: "pass", Rationale: "resume safely",
		})
		done <- err
	}()
	select {
	case <-deterministic.started:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed stage did not start")
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	if entry, held := ledger.LookupScoped(key); !held || entry.RunID != "run-reserved" {
		t.Fatalf("reacquired claim = (%+v, %v)", entry, held)
	}
	if release, ok, reason := fixture.scheduler.ReserveContinuation("competing-run", "example", machine.Def.Name); ok {
		release()
		t.Fatal("competing run acquired admission while intervention was active")
	} else if reason != localscheduler.ReasonMaxParallel {
		t.Fatalf("competing admission refusal = %q", reason)
	}

	close(deterministic.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Override: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intervention did not finish")
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.LookupScoped(key); held {
		t.Fatalf("terminal run retained claim: %+v", entry)
	}
	if release, ok, reason := fixture.scheduler.ReserveContinuation("next-run", "example", machine.Def.Name); !ok {
		t.Fatalf("terminal run retained admission: %s", reason)
	} else {
		release()
	}
}

// afterReclaimClaims runs a probe once the real claim ledger has reacquired a
// run's claims: the window in which the intervention holds its slot and the
// claims but the run's journal still reads terminal.
type afterReclaimClaims struct {
	intervention.ClaimStore
	probe func()
}

func (c afterReclaimClaims) Reclaim(claims []localscheduler.ClaimEntry, gaggle, runID, workflowName string) (bool, string, error) {
	acquired, holder, err := c.ClaimStore.Reclaim(claims, gaggle, runID, workflowName)
	if err == nil && acquired {
		c.probe()
	}
	return acquired, holder, err
}

func TestRunInterventionProtectsReacquiredClaimsBeforeJournalResume(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	type recoveryResult struct {
		released []localscheduler.ClaimEntry
		holder   localscheduler.ClaimEntry
		held     bool
		err      error
	}
	done := make(chan recoveryResult, 1)
	var fixture *interventionWiringFixture
	fixture = newInterventionWiringFixture(t, machine, "run-recovery-window", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, interventionDeterministic{}, func(cfg *intervention.Config) {
		cfg.Claims = afterReclaimClaims{ClaimStore: cfg.Claims, probe: func() {
			released, err := recoverClaims(fixture.layout, fixture.instanceLog, time.Now(), fixture.service.Active, nil)
			result := recoveryResult{released: released, err: err}
			if err == nil {
				ledger, openErr := localscheduler.OpenClaimLedger(filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName))
				if openErr != nil {
					result.err = openErr
				} else {
					result.holder, result.held = ledger.LookupScoped(key)
				}
			}
			done <- result
		}}
	})
	if err := fixture.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-recovery-window", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}

	overridden := make(chan error, 1)
	go func() {
		_, err := fixture.service.Override(context.Background(), httpapi.InterventionRequest{
			RunID: "run-recovery-window", Stage: "review", Actor: "operator",
			Decision: "pass", Rationale: "resume safely",
		})
		overridden <- err
	}()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.released) != 0 {
			t.Fatalf("recovery released active intervention claims: %+v", result.released)
		}
		if !result.held || result.holder.RunID != "run-recovery-window" {
			t.Fatalf("claim in pre-resume window = (%+v, %v)", result.holder, result.held)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim recovery did not finish")
	}
	select {
	case err := <-overridden:
		if err != nil {
			t.Fatalf("Override: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intervention did not finish")
	}
}

func TestRunInterventionRetainsResourcesAcrossAnotherHumanPause(t *testing.T) {
	machine := interventionTwoGateMachine(t)
	fixture := newInterventionWiringFixture(t, machine, "run-repaused", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, interventionDeterministic{}, nil)
	service := fixture.service
	if err := fixture.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-repaused", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-repaused", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "continue to approval",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseRunning) || result.State != "approval" {
		t.Fatalf("override result = %+v, want paused at approval", result)
	}

	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.LookupScoped(key); !held || entry.RunID != "run-repaused" {
		t.Fatalf("claim while re-paused = (%+v, %v)", entry, held)
	}
	if release, ok, _ := fixture.scheduler.ReserveContinuation("competing-run", "example", machine.Def.Name); ok {
		release()
		t.Fatal("re-paused intervention released workflow admission")
	}

	result, err = service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-repaused", Stage: "approval", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("approval result = %+v, want completed", result)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.LookupScoped(key); held {
		t.Fatalf("completed re-paused run retained claim: %+v", entry)
	}
	if release, ok, reason := fixture.scheduler.ReserveContinuation("next-run", "example", machine.Def.Name); !ok {
		t.Fatalf("completed re-paused run retained admission: %s", reason)
	} else {
		release()
	}
}

func TestRunInterventionRejectsClaimOwnedByAnotherRun(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	fixture := newInterventionWiringFixture(t, machine, "run-conflicted", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, interventionDeterministic{}, nil)
	service, runDir := fixture.service, fixture.runDir
	if err := fixture.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-conflicted", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}

	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "other-run", machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed competing claim: ok=%v err=%v", ok, err)
	}

	_, err = service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-conflicted", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "resume safely",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Status != http.StatusConflict || interventionErr.Code != "claim_unavailable" {
		t.Fatalf("Override error = %#v, want claim_unavailable", err)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			t.Fatal("claim-conflicted run was resumed")
		}
	}

	if release, ok, reason := fixture.scheduler.ReserveContinuation("probe-run", "example", machine.Def.Name); !ok {
		t.Fatalf("failed intervention leaked admission: %s", reason)
	} else {
		release()
	}
}

func TestRunInterventionUsesDurableClaimHistoryWhenInstanceJournalMissesAcquisition(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	fixture := newInterventionWiringFixture(t, machine, "run-durable-history", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, interventionDeterministic{}, nil)
	service := fixture.service
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	ledgerPath := filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "run-durable-history", machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed original claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, "run-durable-history"); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "other-run", machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed competing claim: ok=%v err=%v", ok, err)
	}

	_, err = service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-durable-history", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "resume safely",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Code != "claim_unavailable" {
		t.Fatalf("Override error = %#v, want claim_unavailable", err)
	}
}

// lastIntentDeliverer stands in for the Temporal update client and keeps the
// last operator intent the service delivered.
type lastIntentDeliverer struct {
	intents []engine.HITLIntent
}

func (d *lastIntentDeliverer) Deliver(_ context.Context, intent engine.HITLIntent) (engine.HITLAck, error) {
	d.intents = append(d.intents, intent)
	return engine.HITLAck{Resumed: true, ResumeState: "implement"}, nil
}

// TestEngineHITLInterventionPrefersScopedRunOverLegacyProjection proves the
// intervention service uses the shared owned-run resolver (#4858). The
// authoritative scoped journal is escalated and carries generation one; the
// independent legacy projection is deliberately left running, making the
// selected generation a direct assertion that resolution preferred scoped.
func TestEngineHITLInterventionPrefersScopedRunOverLegacyProjection(t *testing.T) {
	deliverer := &lastIntentDeliverer{}
	const runID = "engine-hitl-dual-projection"
	machine := interventionTerminalTestMachine(t, apiv1.EvaluatorAgentic)
	fixture := newInterventionWiringFixture(t, machine, runID, []journal.Event{
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "escalate"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, interventionDeterministic{}, nil)
	markRunYAMLEngineDriven(t, fixture.runDir)
	fixture.service.AttachHITLDeliverer(deliverer)
	definitions := fixture.definitions.Snapshot()
	definitions.legacyRunner = definitions.runners["example"]
	fixture.definitions.Replace(definitions)
	createDriverRun(t, fixture.layout.RunsDir(), runID, "terminal-intervention", "example", journal.DriverEngine, time.Now(), nil)

	if _, err := fixture.service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "ops", Decision: "pass", IdempotencyKey: "dual-projection-key",
	}); err != nil {
		t.Fatalf("approve dual-projected engine run: %v", err)
	}
	if len(deliverer.intents) == 0 {
		t.Fatal("no operator intent was delivered to the engine")
	}
	if intent := deliverer.intents[len(deliverer.intents)-1]; intent.ExpectedTerminalGeneration != 1 {
		t.Fatalf("terminal generation = %d, want scoped journal generation 1", intent.ExpectedTerminalGeneration)
	}
}
