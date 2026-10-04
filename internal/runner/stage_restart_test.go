package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func restartMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	def := rerunTaskMachine(t).Def
	def.DSLVersion = "3.1"
	for i := range def.Spec.Tasks {
		def.Spec.Tasks[i].Workspace = apiv1.WorkspaceScratch
	}
	m, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func saveRestartGuidance(t *testing.T, runsDir, runID, key, text string) {
	t.Helper()
	run, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = run.AcceptOperatorMessage(apiv1.OperatorMessageRequest{Schema: apiv1.OperatorMessageRequestSchema, RequestID: key, IdempotencyKey: key, TargetAddress: "stage:implement", PrincipalRef: "issuer:human", RequestedAt: time.Now(), Purpose: "stage-restart-guidance", Content: apiv1.OperatorMessageContent{Text: text}, DeliveryMode: "shared-guidance"})
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestStageRestartExecutesPinnedGuidanceInDistinctContinuation(t *testing.T) {
	machine := restartMachine(t)
	implementer, finisher := &rerunTaskGoober{}, &capturingSuccessGoober{}
	r, runsDir := newRerunTestRunner(t, func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
		if name == "implementer" {
			return implementer, nil
		}
		return finisher, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()
	r.cfg.AdditionalRepos = []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "repo", Branch: "main"}}
	r.cfg.StageRestartContext = func(ctx context.Context, _ journal.RunIdentity, _ SecretRegistrar) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	source, err := r.Start(context.Background(), StartInput{RunID: "restart-source", Machine: machine, Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo})
	if err != nil {
		t.Fatal(err)
	}
	if source.Phase != journal.PhaseEscalated {
		t.Fatalf("source = %+v", source)
	}
	saveRestartGuidance(t, runsDir, "restart-source", "selected", "Use the existing boundary. secret-that-must-be-redacted")
	saveRestartGuidance(t, runsDir, "restart-source", "unselected", "Do not deliver this note.")
	before, err := os.ReadFile(filepath.Join(runsDir, "restart-source", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runsDir, "restart-source"))
	if err != nil {
		t.Fatal(err)
	}
	scrubber := journal.NewRegistryScrubber()
	scrubber.Register([]byte("secret-that-must-be-redacted"))
	request := StageRestartRequest{EpochID: "restart-epoch", Stage: "implement", PrincipalRef: "issuer:human", ExpectedTerminalSeq: terminalRunSequence(t, runsDir, "restart-source"), GuidanceIDs: []string{"selected"}, Rationale: "Fresh allowance with reviewed guidance"}
	plan, err := PrepareStageRestart(reader, machine, request, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	retainedPlan, err := MarshalStageRestartPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	restoredPlan, err := ParseStageRestartPlan(retainedPlan)
	if err != nil || !reflect.DeepEqual(plan, restoredPlan) {
		t.Fatal("durable restart plan changed", err)
	}
	plan = restoredPlan
	continued, err := journal.CreateContinuation(runsDir, plan.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	if err = continued.Close(); err != nil {
		t.Fatal(err)
	}
	automationConfig := r.cfg
	automationConfig.StageRestartContext = nil
	automation, err := New(automationConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = automation.Resume(context.Background(), ResumeInput{RunID: request.EpochID, Machine: machine, RepoRef: repo}); err == nil {
		t.Fatal("human restart used automation fallback")
	}
	crashed, _, err := journal.Recover(filepath.Join(runsDir, request.EpochID))
	if err != nil {
		t.Fatal(err)
	}
	if err = crashed.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptHuman}); err != nil {
		t.Fatal(err)
	}
	if err = crashed.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := r.Resume(context.Background(), ResumeInput{RunID: request.EpochID, Machine: machine, RepoRef: repo})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("restart = %+v", result)
	}
	if len(implementer.invocations) != 2 || len(finisher.invocations) != 1 {
		t.Fatalf("invocations = %d,%d", len(implementer.invocations), len(finisher.invocations))
	}
	got := implementer.invocations[1]
	if got.Attempt != 3 || !strings.Contains(got.InstructionAddendum, "Use the existing boundary") || strings.Contains(got.InstructionAddendum, "secret-that") || strings.Contains(got.InstructionAddendum, "Do not deliver") {
		t.Fatalf("restart invocation = %+v", got)
	}
	if finisher.invocations[0].InstructionAddendum != "" {
		t.Fatal("guidance leaked to later stage")
	}
	after, err := os.ReadFile(filepath.Join(runsDir, "restart-source", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("source journal changed")
	}
	epochReader, _ := journal.OpenReadOnly(filepath.Join(runsDir, request.EpochID))
	identity, _ := epochReader.Identity()
	sourceIdentity, _ := reader.Identity()
	if !identity.StartedAt.Equal(sourceIdentity.StartedAt) || !reflect.DeepEqual(identity.RunControls, sourceIdentity.RunControls) {
		t.Fatal("restart reset source duration or run controls")
	}
	manifest, err := readStageRestartManifest(epochReader, identity, machine)
	if err != nil {
		t.Fatal(err)
	}
	if manifest == nil || pendingStageRestart(manifest, readRerunEvents(t, runsDir, request.EpochID)) != nil {
		t.Fatal("completed restart guidance was replayed")
	}
	request.ExpectedTerminalSeq++
	if _, err = PrepareStageRestart(reader, machine, request, scrubber); err == nil {
		t.Fatal("stale terminal generation accepted")
	}
}

func TestRestartBudgetsResetOnlyAffectedScopeAndKeepEpochConsumption(t *testing.T) {
	machine := restartMachine(t)
	machine.Def.Spec.Gates = []apiv1.Gate{{Name: "review", Branches: map[string]string{"changes": "implement"}}, {Name: "other", Branches: map[string]string{"changes": "elsewhere"}}}
	history := []journal.Event{{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Target: "implement", Runner: map[string]any{"gateAttempt": float64(3), "repassAttempt": float64(3)}}, {Type: journal.EventGateEvaluated, Gate: "other", Verdict: "needs-changes", Target: "elsewhere", Runner: map[string]any{"gateAttempt": float64(1), "repassAttempt": float64(1)}}}
	f := &resumeFrame{restart: &stageRestartManifest{Stage: "implement", BudgetHistory: history}, ws: &walkState{gateAttempts: map[string]int{"review": 1}, repassAttempts: map[string]int{"implement": 1}}}
	seedRestartGateBudgets(f, machine)
	if f.ws.gateAttempts["review"] != 1 || f.ws.gateAttempts["other"] != 1 || f.ws.repassAttempts["implement"] != 1 || f.ws.repassAttempts["elsewhere"] != 1 {
		t.Fatalf("budget seeds = %+v %+v", f.ws.gateAttempts, f.ws.repassAttempts)
	}
}

func TestStageRestartRejectsFrozenAndSealedChildIdentities(t *testing.T) {
	machine := restartMachine(t)
	identity := journal.RunIdentity{RunID: "source", WorkflowDigest: machine.Digest()}
	identity.Child = &journal.ChildLineage{}
	if err := ValidateStageRestartTarget(identity, machine, "implement"); err == nil {
		t.Fatal("settled child custody bypassed")
	}
	identity.Child = nil
	def := machine.Def
	def.DSLVersion = "3.0"
	frozen, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	identity.WorkflowDigest = frozen.Digest()
	if err := ValidateStageRestartTarget(identity, frozen, "implement"); err == nil {
		t.Fatal("frozen interpreter restart admitted")
	}
}

type restartUpstream struct {
	calls    int
	revision *apiv1.WorkspaceRevision
}

func (s *restartUpstream) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"ticket": "retained-upstream-value"}, WorkspaceRevision: s.revision.DeepCopy()}, nil
}
func TestStageRestartRestoresUpstreamWithoutExecutingItAgain(t *testing.T) {
	def := restartMachine(t).Def
	def.Spec.Start = "prepare"
	def.Spec.Tasks[0].InputsFrom = map[string]string{"ticket": "prepare.ticket"}
	def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{Name: "prepare", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, ExpectedOutputs: []string{"ticket"}, Next: "implement"})
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	revision := runnerWorkspaceRevision("other", "repo", strings.Repeat("a", 40))
	revision.BaseRepository = &apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	upstream, agent := &restartUpstream{revision: revision}, &rerunTaskGoober{}
	r, runsDir := newRerunTestRunner(t, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return agent, nil }, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return upstream, nil })
	r.cfg.ScratchDir = t.TempDir()
	r.cfg.AdditionalRepos = []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "repo", Branch: "main"}}
	r.cfg.StageRestartContext = func(ctx context.Context, _ journal.RunIdentity, _ SecretRegistrar) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	if _, err = r.Start(context.Background(), StartInput{RunID: "context-source", Machine: machine, Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo}); err != nil {
		t.Fatal(err)
	}
	saveRestartGuidance(t, runsDir, "context-source", "context-note", "Use the retained upstream input.")
	reader, _ := journal.OpenReadOnly(filepath.Join(runsDir, "context-source"))
	plan, err := PrepareStageRestart(reader, machine, StageRestartRequest{EpochID: "context-epoch", Stage: "implement", PrincipalRef: "issuer:human", ExpectedTerminalSeq: terminalRunSequence(t, runsDir, "context-source"), GuidanceIDs: []string{"context-note"}, Rationale: "Retry with context"}, journal.NewPatternScrubber())
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := journal.CreateContinuation(runsDir, plan.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	if err = epoch.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Resume(context.Background(), ResumeInput{RunID: "context-epoch", Machine: machine, RepoRef: repo}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.SourceWorkspaceRevision, revision) || !reflect.DeepEqual(agent.invocations[1].WorkspaceRevision, revision) {
		t.Fatal("restart lost selected repository authority")
	}
	if upstream.calls != 1 {
		t.Fatalf("upstream executed %d times", upstream.calls)
	}
	if len(agent.invocations) < 2 || agent.invocations[1].Inputs["ticket"] != "retained-upstream-value" {
		t.Fatalf("restart lost upstream: %+v", agent.invocations)
	}
}
