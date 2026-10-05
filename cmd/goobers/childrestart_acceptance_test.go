package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type childRestartCapture struct {
	mu        sync.Mutex
	envelopes []apiv1.InvocationEnvelope
}

func (d *childRestartCapture) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.envelopes = append(d.envelopes, env)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (d *childRestartCapture) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return d.Run(ctx, env, apiv1.DeterministicRun{})
}
func (d *childRestartCapture) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, nil
}

type daemonChildRestartFixture struct {
	*actualChildFixture
	human     *intervention.HumanService
	policy    *interactiveaccess.Service
	principal httpapi.Principal
	source    journal.RunIdentity
	sequence  uint64
	before    []byte
	capture   *childRestartCapture
	gaggle    apiv1.Gaggle
}

func newDaemonChildRestartFixture(t *testing.T, phases ...string) *daemonChildRestartFixture {
	t.Helper()
	f := &daemonChildRestartFixture{actualChildFixture: actualChildLaunchFixture(t, strings.Replace(strings.Replace(dispatchChildSource, "      type: deterministic", "      type: agentic\n      goober: coder\n      capabilities: [agent:model]\n      workspace: scratch", 1), "      run:\n        command: [\"true\"]\n", "", 1)), capture: &childRestartCapture{}}
	phase := "failed"
	if len(phases) > 0 {
		phase = phases[0]
	}
	f.source = publishInterruptedChild(t, f.actualChildFixture)
	dir, err := f.launcher.layout.FindRunDir(f.source.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := journal.TryRecover(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{{Type: journal.EventStageStarted, Stage: "check", Attempt: 1}, {Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: "failed"}, {Type: journal.EventRunFinished, Status: phase}} {
		if err = writer.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	f.principal = httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	_, _, err = writer.AcceptOperatorMessage(apiv1.OperatorMessageRequest{Schema: apiv1.OperatorMessageRequestSchema, RequestID: "selected", IdempotencyKey: "saved", TargetAddress: "stage:check", PrincipalRef: f.principal.Issuer + ":" + f.principal.Subject, RequestedAt: time.Now(), Purpose: "restart", DeliveryMode: "shared-guidance", Content: apiv1.OperatorMessageContent{Text: "Use the retained child context and finish the check"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			f.sequence = event.Seq
		}
	}
	f.before, err = os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f.launcher.result = f.launcher.captureTerminal
	if err = f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.gaggle = *f.authority.authority.Admission.Gaggle.DeepCopy()
	f.gaggle.Spec.InteractiveAccess = &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: f.principal.Issuer, Subject: f.principal.Subject}}}, Actions: []apiv1.InteractiveAction{"run.intervene", "run.restartStage"}}
	f.policy, err = interactiveaccess.New([]apiv1.Gaggle{f.gaggle}, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	plane := &daemonCredentialService{layout: f.launcher.layout, childQueue: f.service.queue, interactive: f.policy}
	defs := credentialPlaneDefinitions{Scopes: map[string]credentialGaggleScope{f.source.Gaggle: {Project: f.gaggle.Spec.Project, Backlog: f.gaggle.Spec.Backlog, AdditionalRepos: f.gaggle.Spec.AdditionalRepos}}}
	build := f.launcher.build
	f.launcher.build = func(ctx context.Context, start childExecutionStart) (preparedChildRuntime, error) {
		runtime, err := build(ctx, start)
		if err != nil {
			return runtime, err
		}
		id := start.executionIdentity(runtime.gooberDigest)
		runtime.runner, err = runtime.runner.ForChildExecution(id, runner.ChildExecutionFactories{NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return f.capture, nil
		}, NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
			return f.capture, nil
		}})
		if err == nil && start.Execution != nil {
			runtime.runner, err = runtime.runner.ForChildStageRestartExecution(id, func(ctx context.Context, actual journal.RunIdentity, _ runner.SecretRegistrar) (context.Context, func(), error) {
				owned, done, err := plane.beginChildRestartAuthority(ctx, actual, defs)
				if err != nil {
					return nil, nil, err
				}
				return owned, func() {
					if err := done(); err != nil {
						t.Error(err)
					}
				}, nil
			})
		}
		return runtime, err
	}
	setup := &schedulerSetup{Config: &instance.Config{}, Root: f.launcher.layout.Root, InteractiveAccess: f.policy, ChildRestarts: f.launcher, CredentialPlane: plane, RunnerRegistry: f.launcher.runners, Interventions: newInterventionDefinitionRegistry(interventionDefinitionSet{runners: map[string]*runner.Runner{f.source.Gaggle: nil}})}
	setup.InteractiveRestartExecution = setup.buildInteractiveRestartExecution
	interventions := newRunInterventionService(f.launcher.layout, setup, &f.wg, nil)
	interventions.AttachScheduler(f.launcher.dispatch.sched.Load())
	f.human, err = intervention.NewHumanService(interventions, f.policy, newDaemonRunJournalService(f.launcher.layout, nil), journal.NewPatternScrubber())
	if err != nil {
		t.Fatal(err)
	}
	adapter := &interactiveStageRestart{layout: f.launcher.layout, setup: setup, service: interventions}
	f.launcher.restart = adapter.reconcileChildRestart
	f.human.AttachStageRestarts(adapter)
	return f
}

func (f *daemonChildRestartFixture) request() apicontract.InteractiveRunCommand {
	return apicontract.InteractiveRunCommand{Kind: "restart", Stage: "check", ExpectedSubjectSequence: f.sequence, GuidanceIDs: []string{"selected"}, Rationale: "Human reviewed the failed check"}
}

func TestDaemonChildHumanRestartQueuesThenRunsExactCommonEpoch(t *testing.T) {
	f := newDaemonChildRestartFixture(t)
	scheduler := f.launcher.dispatch.sched.Load()
	release, ok, reason := scheduler.ReserveContinuation(f.source.Child.ParentRunID, f.source.Gaggle, f.source.Child.ParentWorkflow)
	if !ok {
		t.Fatal(reason)
	}
	release = sync.OnceFunc(release)
	defer release()
	accepted, err := f.human.AcceptInteractiveRun(t.Context(), t.Context(), f.principal, f.source.RunID, "restart-1", f.request())
	if err != nil || !accepted.Accepted || accepted.Status != "pending" || len(accepted.ContinuationRunID) != 32 {
		t.Fatal(accepted, err)
	}
	epochID := accepted.ContinuationRunID
	if _, err = f.launcher.layout.FindRunDir(epochID); err == nil {
		t.Fatal("capacity-blocked child published journal")
	}
	changed := f.request()
	changed.Rationale = "different request"
	if _, err = f.human.AcceptInteractiveRun(t.Context(), t.Context(), f.principal, f.source.RunID, "restart-1", changed); err == nil {
		t.Fatal("changed request accepted")
	}
	release()
	for range 4 {
		if err = f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.wg.Wait()
	}
	child, err := f.service.queue.ChildForExecutionRun(t.Context(), epochID)
	if err != nil || child.State != triggerqueue.ChildCompleted || child.RunID != f.source.RunID || child.ActiveRunID() != epochID || child.ResultRef == "" {
		t.Fatal(child, err)
	}
	f.capture.mu.Lock()
	defer f.capture.mu.Unlock()
	if len(f.capture.envelopes) != 1 || f.capture.envelopes[0].Attempt != 2 || f.capture.envelopes[0].InstructionAddendum == "" {
		t.Fatal("guidance/attempt not restored", f.capture.envelopes)
	}
	dir, err := f.launcher.layout.FindRunDir(f.source.RunID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || string(after) != string(f.before) {
		t.Fatal("sealed source changed", err)
	}
	replay, err := f.human.AcceptInteractiveRun(t.Context(), t.Context(), f.principal, f.source.RunID, "restart-1", f.request())
	if err != nil || replay.ContinuationRunID != epochID {
		t.Fatal(replay, err)
	}
}

func TestDaemonChildHumanRestartPolicyRevocationAndParentCancellationFenceQueuedEpoch(t *testing.T) {
	f := newDaemonChildRestartFixture(t)
	release, ok, reason := f.launcher.dispatch.sched.Load().ReserveContinuation(f.source.Child.ParentRunID, f.source.Gaggle, f.source.Child.ParentWorkflow)
	if !ok {
		t.Fatal(reason)
	}
	release = sync.OnceFunc(release)
	defer release()
	accepted, err := f.human.AcceptInteractiveRun(t.Context(), t.Context(), f.principal, f.source.RunID, "restart-1", f.request())
	if err != nil || accepted.Status != "pending" {
		t.Fatal(accepted, err)
	}
	f.gaggle.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"run.intervene"}
	if err = f.policy.Apply([]apiv1.Gaggle{f.gaggle}, nil); err != nil {
		t.Fatal(err)
	}
	release()
	var denied bool
	for range 2 {
		if err = f.service.Drain(t.Context()); err != nil {
			denied = true
		}
	}
	if !denied {
		t.Fatal("revoked queued human authority was not rechecked")
	}
	if _, err = f.launcher.layout.FindRunDir(accepted.ContinuationRunID); err == nil {
		t.Fatal("revoked epoch published")
	}
	child, err := f.service.queue.ChildForExecutionRun(t.Context(), accepted.ContinuationRunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.queue.FenceChildParent(t.Context(), child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	child, err = f.service.queue.GetChild(t.Context(), child.Identity)
	if err != nil || child.State != triggerqueue.ChildCancelled {
		t.Fatal(child, err)
	}
	f.capture.mu.Lock()
	defer f.capture.mu.Unlock()
	if len(f.capture.envelopes) != 0 {
		t.Fatal("revoked/cancelled queued execution reached worker")
	}
}

func TestDaemonChildHumanRestartRefusesRevokedParentPolicyBeforeAcceptance(t *testing.T) {
	f := newDaemonChildRestartFixture(t)
	f.authority.revoked.Store(true)
	if _, err := f.human.AcceptInteractiveRun(t.Context(), t.Context(), f.principal, f.source.RunID, "restart-1", f.request()); err == nil {
		t.Fatal("revoked child policy accepted new epoch")
	}
	child, err := f.service.queue.GetChild(t.Context(), f.submission.Child.Identity)
	if err != nil || child.ExecutionEpoch != 0 || child.State != triggerqueue.ChildFailed {
		t.Fatal(child, err)
	}
}

func TestDaemonChildHumanRestartSealsEscalationBeforeNewEpoch(t *testing.T) {
	f := newDaemonChildRestartFixture(t, "escalated")
	before, err := f.service.queue.GetChild(t.Context(), f.submission.Child.Identity)
	if err != nil || before.State != triggerqueue.ChildAwaitingHuman || before.ResultRef != "" {
		t.Fatal(before, err)
	}
	view, err := f.human.InspectInteractiveRun(t.Context(), f.principal, f.source.RunID)
	if err != nil {
		t.Fatal(err)
	}
	available := false
	for _, action := range view.Actions {
		if action.Kind == "restart" && action.Stage == "check" && action.Available {
			available = true
		}
	}
	if !available {
		t.Fatal("current sealed-capable child restart not projected", view)
	}
	accepted, err := f.human.AcceptInteractiveRun(t.Context(), t.Context(), f.principal, f.source.RunID, "restart-escalated", f.request())
	if err != nil || !accepted.Accepted {
		t.Fatal(accepted, err)
	}
	f.wg.Wait()
	for range 4 {
		if err = f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.wg.Wait()
	}
	history, err := f.service.queue.ChildExecutionHistory(t.Context(), before.Identity)
	if err != nil || len(history) != 2 || history[0].State != triggerqueue.ChildAwaitingHuman || history[0].ResultRef == "" || history[1].State != triggerqueue.ChildCompleted {
		t.Fatal(history, err)
	}
}
