package intervention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type childRestartAgent struct {
	calls int
	env   apiv1.InvocationEnvelope
}

func (a *childRestartAgent) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	a.calls++
	a.env = env
	if env.InstructionAddendum == "" {
		return apiv1.ResultEnvelope{Status: apiv1.ResultBlocked}, nil
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (*childRestartAgent) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("unexpected reviewer")
}

type childRestartFixture struct {
	service     *Service
	queue       *triggerqueue.Store
	child       triggerqueue.ChildRecord
	plan        runner.StageRestartPlan
	entry       localscheduler.WorkflowEntry
	agent       *childRestartAgent
	runs        string
	wg          sync.WaitGroup
	fences      int
	beforeFence func(int)
}

func newChildRestartFixture(t *testing.T) *childRestartFixture {
	t.Helper()
	f := &childRestartFixture{agent: &childRestartAgent{}}
	layout := instance.NewLayout(t.TempDir())
	f.runs = layout.ForGaggle("example").RunsDir()
	queue, err := triggerqueue.Open(filepath.Join(layout.Root, "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.queue = queue
	t.Cleanup(func() { f.wg.Wait(); _ = queue.Close() })
	child, _, err := queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "example", ParentRunID: "parent"}, StageOccurrence: "plan/visit-1", InvocationKey: "work"}, Actor: "parent-stage", Payload: []byte(`{"kind":"child-test"}`), MaxChildren: 1}, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	f.child = child
	machine, err := workflow.Compile(workflow.Definition{Name: "generated", Version: 1, DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "implement", Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Workspace: apiv1.WorkspaceScratch, Next: workflow.TerminalComplete}}}}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(layout.ForGaggle("example").WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	agentic := func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
		return f.agent, nil
	}
	deterministic := func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
		return nil, errors.New("unexpected deterministic")
	}
	base, err := runner.New(runner.Config{RunsDir: f.runs, ScratchDir: t.TempDir(), Worktrees: manager, ConfigGeneration: journal.Digest([]byte("archive")), NewAgentic: agentic})
	if err != nil {
		t.Fatal(err)
	}
	lineage := &journal.ChildLineage{Gaggle: "example", ParentRunID: "parent", ParentWorkflow: "parent-workflow", StageOccurrence: child.Identity.StageOccurrence, InvocationKey: child.Identity.InvocationKey, AcceptanceID: child.AcceptanceID, SourceDigest: journal.Digest([]byte("proposal")), EnvelopeDigest: journal.Digest([]byte("envelope"))}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	gooberDigest := journal.Digest([]byte("goober"))
	if result, err := base.Start(t.Context(), runner.StartInput{RunID: child.RunID, Gaggle: "example", Machine: machine, GooberDigest: gooberDigest, RepoRef: repo, Child: lineage, Trigger: journal.Trigger{Kind: journal.TriggerManual}}); err != nil || result.Phase != journal.PhaseEscalated {
		t.Fatal(result, err)
	}
	plan := prepareChildRestartFixturePlan(t, f, machine)
	custody := childworkflow.WorkspaceCoordinator{Queue: queue}
	result, err := custody.CaptureResult(t.Context(), child, nil, childworkflow.TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: time.Now(), Summary: "sealed source"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: result.ResultRef}, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := runner.MarshalStageRestartPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := queue.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: child.Identity, RunID: plan.Continuation.RunID, SourceRunID: child.RunID, SourceTerminalSeq: plan.Continuation.ExpectedTerminalSeq, SourceResultRef: result.ResultRef, Actor: plan.Continuation.Operator, Stage: plan.Continuation.Target, Plan: raw, PlanDigest: journal.Digest(raw)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	continuedLineage := *lineage
	continuedLineage.ExecutionEpoch, continuedLineage.PriorResultRef, continuedLineage.RestartDigest = epoch.Epoch, epoch.SourceResultRef, epoch.RequestDigest
	plan.Continuation.ChildContinuation = &continuedLineage
	f.plan = plan
	id := plan.Source
	id.RunID, id.ContinuedFromRunID, id.SourceTerminalSeq, id.Operator, id.RequestedTarget, id.Child = epoch.RunID, child.RunID, epoch.SourceTerminalSeq, epoch.Actor, epoch.Stage, &continuedLineage
	contained, err := base.ForChildExecution(id, runner.ChildExecutionFactories{NewAgentic: agentic, NewDeterministic: deterministic})
	if err != nil {
		t.Fatal(err)
	}
	human, err := contained.ForStageRestartExecution(id, runner.StageRestartExecutionFactories{NewAgentic: agentic, NewDeterministic: deterministic, Context: func(ctx context.Context, _ journal.RunIdentity, _ runner.SecretRegistrar) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.entry = localscheduler.WorkflowEntry{Gaggle: "example", Workflow: machine.Def.Name, RepoRef: repo}
	f.service = New(Config{Definitions: func() Definitions { return Definitions{Runners: map[string]*runner.Runner{"example": base}} }, Runners: &testRunnerRegistry{owners: map[string]*runner.Runner{}}, LocateRun: locateTestRun(layout), WaitGroup: &f.wg,
		PinnedExecution: func(context.Context, journal.RunIdentity) (Execution, error) {
			return Execution{Runner: base, Machine: machine, GooberDigest: gooberDigest, RepoRef: repo}, nil
		},
		PinnedInspection: func(context.Context, journal.RunIdentity) (Execution, error) {
			return Execution{Machine: machine, GooberDigest: gooberDigest, RepoRef: repo}, nil
		},
		StageRestartExecution: func(context.Context, runner.StageRestartPlan) (Execution, error) {
			return Execution{Runner: human, Machine: machine, GooberDigest: gooberDigest, RepoRef: repo, ChildRestart: &ChildStageRestartAdmission{Entry: f.entry, Fence: func(ctx context.Context, callback func() error) error {
				f.fences++
				if f.beforeFence != nil {
					f.beforeFence(f.fences)
				}
				return queue.WithChildExecutionResume(ctx, child.Identity, epoch.RunID, callback)
			}}}, nil
		},
	})
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	f.service.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: "example", Workflow: "parent-workflow", RepoRef: repo, Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}}}, log))
	return f
}

func prepareChildRestartFixturePlan(t *testing.T, f *childRestartFixture, machine *workflow.Machine) runner.StageRestartPlan {
	t.Helper()
	run, _, err := journal.Recover(filepath.Join(f.runs, f.child.RunID))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = run.AcceptOperatorMessage(apiv1.OperatorMessageRequest{Schema: apiv1.OperatorMessageRequestSchema, RequestID: "guidance", IdempotencyKey: "guidance", TargetAddress: "stage:implement", PrincipalRef: "issuer:human", RequestedAt: time.Now(), Purpose: "restart", Content: apiv1.OperatorMessageContent{Text: "Use sealed child context"}, DeliveryMode: "shared-guidance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(f.runs, f.child.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	_, scrubber := journal.DefaultScrubber()
	plan, err := runner.PrepareChildStageRestart(reader, machine, runner.StageRestartRequest{EpochID: strings.Repeat("a", 32), Stage: "implement", PrincipalRef: "issuer:human", ExpectedTerminalSeq: latestTerminalSequence(events), GuidanceIDs: []string{"guidance"}, Rationale: "Apply reviewed guidance"}, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestCommonChildRestartUsesParentCapacityAndQueueFence(t *testing.T) {
	f := newChildRestartFixture(t)
	scheduler := f.service.scheduler.Load()
	release, ok, reason := scheduler.ReserveContinuation("parent", "example", "parent-workflow")
	if !ok {
		t.Fatal(reason)
	}
	preflight := func(context.Context, *runner.StageRestartPlan) ([]localscheduler.ClaimEntry, error) { return nil, nil }
	if _, err := f.service.LaunchStageRestart(t.Context(), t.Context(), f.plan, preflight); err == nil {
		t.Fatal("child bypassed parent concurrency")
	}
	if _, err := os.Stat(filepath.Join(f.runs, f.plan.Continuation.RunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capacity refusal published epoch", err)
	}
	release()
	accepted, err := f.service.LaunchStageRestart(t.Context(), t.Context(), f.plan, preflight)
	if err != nil || accepted.RunID != f.plan.Continuation.RunID {
		t.Fatal(accepted, err)
	}
	f.wg.Wait()
	if f.agent.calls != 2 || f.fences != 2 || !strings.Contains(f.agent.env.InstructionAddendum, "Use sealed child context") {
		t.Fatal("restart bypassed common guidance or fences", f.agent.calls, f.fences, f.agent.env)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(f.runs, accepted.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseCompleted {
		t.Fatal(phase, err)
	}
	if _, err := f.service.LaunchStageRestart(t.Context(), t.Context(), f.plan, preflight); err != nil {
		t.Fatal(err)
	}
	if f.agent.calls != 2 {
		t.Fatal("receipt replay started another child")
	}
}

func TestCommonChildRestartCancellationAfterPublicationPreventsEffects(t *testing.T) {
	f := newChildRestartFixture(t)
	f.beforeFence = func(n int) {
		if n == 2 {
			if err := f.queue.FenceChildParent(t.Context(), f.child.Identity.ChildParent, "operator", time.Now()); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := f.service.LaunchStageRestart(t.Context(), t.Context(), f.plan, func(context.Context, *runner.StageRestartPlan) ([]localscheduler.ClaimEntry, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	if f.fences != 2 || f.agent.calls != 1 {
		t.Fatal("cancelled parent resumed effects", f.fences, f.agent.calls)
	}
}

func TestSealedChildInspectionDoesNotAcquireExecutableGeneration(t *testing.T) {
	f := newChildRestartFixture(t)
	f.service.pinnedExecution = func(context.Context, journal.RunIdentity) (Execution, error) {
		return Execution{}, errors.New("source execution sealed")
	}
	inspected, err := f.service.inspect(f.child.RunID)
	if err != nil || inspected.machine == nil || inspected.runner != nil {
		t.Fatal(inspected, err)
	}
	if _, err = f.service.resolve(f.child.RunID); err == nil {
		t.Fatal("inspection authority escaped into legacy execution")
	}
	existing := f.service.pinnedInspection
	f.service.pinnedInspection = func(ctx context.Context, id journal.RunIdentity) (Execution, error) {
		out, err := existing(ctx, id)
		out.Runner = &runner.Runner{}
		return out, err
	}
	if _, err = f.service.inspect(f.child.RunID); err == nil {
		t.Fatal("inspection accepted executable runner")
	}
}
