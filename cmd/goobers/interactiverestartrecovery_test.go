package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInteractiveRestartRecoverySelectsHumanAuthorityAndCurrentPolicy(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	machine := interventionTestMachine(t, apiv1.EvaluatorHuman)
	principal := httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: principal.Issuer, Subject: principal.Subject}}}, Actions: []apiv1.InteractiveAction{"run.restartStage"}}}}
	access, err := interactiveaccess.New([]apiv1.Gaggle{gaggle}, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.WorkflowIdentity{Gaggle: gaggle.Name, Workflow: machine.Def.Name}
	definitions := interventionDefinitionSet{machines: map[localscheduler.WorkflowIdentity]*workflow.Machine{key: machine}}
	setup := &schedulerSetup{InteractiveAccess: access, Interventions: newInterventionDefinitionRegistry(definitions)}
	id := createRecoveryEpoch(t, layout, principal, machine)
	calls := 0
	build := func(_ context.Context, actual journal.RunIdentity) (intervention.Execution, error) {
		calls++
		if actual.RunID != id.RunID || actual.ContinuedFromRunID != "source" {
			t.Fatal("recovery created another restart")
		}
		return intervention.Execution{Runner: &runner.Runner{}, Machine: machine, GooberDigest: id.GooberDigest}, nil
	}
	registry := newDaemonRunnerRegistry()
	registry.setGenerationResolver(func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error) {
		t.Fatal("human epoch selected automation")
		return executionGenerationRuntime{}, nil
	})
	if _, err := registry.executionGeneration(t.Context(), id); err == nil {
		t.Fatal("missing human resolver fell back")
	}
	registry.setInteractiveGenerationResolver(interactiveGenerationResolver(layout, setup, build))
	if _, err := registry.executionGeneration(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("human builder not selected")
	}
	// A new current definition may supply enablement only; it never replaces pins.
	disabled := false
	current := *machine
	current.Def.Spec.Enabled = &disabled
	setup.Interventions.Replace(interventionDefinitionSet{machines: map[localscheduler.WorkflowIdentity]*workflow.Machine{key: &current}})
	if _, err := registry.executionGeneration(t.Context(), id); err == nil {
		t.Fatal("disabled workflow resumed")
	}
	setup.Interventions.Replace(definitions)
	gaggle.Spec.InteractiveAccess.Humans.Operators = nil
	if err := access.Apply([]apiv1.Gaggle{gaggle}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.executionGeneration(t.Context(), id); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatalf("revoked=%v", err)
	}
	if calls != 1 {
		t.Fatal("refused recovery constructed execution")
	}
	released := false
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	_, available, err := resolveInterruptedRuntime(t.Context(), id, interruptedRuntimeInput{registry: registry, log: log, release: func(runID, _ string) { released = runID == id.RunID }})
	if err != nil || available || !released {
		t.Fatalf("defer available=%v released=%v err=%v", available, released, err)
	}
}

func createRecoveryEpoch(t *testing.T, layout instance.Layout, p httpapi.Principal, machine *workflow.Machine) journal.RunIdentity {
	t.Helper()
	source := journal.RunIdentity{RunID: "source", Gaggle: "example", Workflow: machine.Def.Name, WorkflowDigest: machine.Digest(), GooberDigest: journal.Digest([]byte("goobers")), ConfigGeneration: journal.Digest([]byte("generation"))}
	runs := layout.ForGaggle(source.Gaggle).RunsDir()
	run, err := journal.Create(runs, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, source.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	plan := runner.StageRestartPlan{Source: source, Continuation: journal.ContinuationRequest{RunID: "epoch", SourceRunID: source.RunID, ExpectedTerminalSeq: events[len(events)-1].Seq, Operator: p.Issuer + ":" + p.Subject, Target: "implement"}}
	if err = stampRestartAuthority(layout, p, &plan); err != nil {
		t.Fatal(err)
	}
	plan.Continuation.Inputs[runner.StageRestartInputName] = []byte("resolver selection marker; runtime verifies full manifest")
	plan.Continuation.InputIntegrity[runner.StageRestartInputName] = apiv1.IntegrityTrusted
	plan.Continuation.InputSource[runner.StageRestartInputName] = plan.Continuation.Operator
	run, err = journal.CreateContinuation(runs, plan.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err = journal.OpenReadOnly(filepath.Join(runs, "epoch"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
