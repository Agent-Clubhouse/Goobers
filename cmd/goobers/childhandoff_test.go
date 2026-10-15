package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

type handoffDaemonFixture struct {
	host    *daemonChildHandoff
	service *daemonCredentialService
	queue   *triggerqueue.Store
	run     *journal.Run
	env     apiv1.InvocationEnvelope
	child   triggerqueue.ChildRecord
	grant   triggerqueue.ChildAuthority
}

func newHandoffDaemonFixture(t *testing.T) handoffDaemonFixture {
	t.Helper()
	return newHandoffDaemonFixtureConfig(t, nil)
}

func newHandoffDaemonFixtureConfig(t *testing.T, configure func(*instance.Config)) handoffDaemonFixture {
	t.Helper()
	f := newPinnedChildFixture(t)
	if configure != nil {
		configure(f.cfg)
	}
	run, env := configuredChildStage(t, f)
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, service) })
	if err := service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	service.childDispatch = newDaemonTriggerService()
	access, closeGrant, err := service.children.Acquire(t.Context(), env, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeGrant() })
	if _, err := service.children.HTTPService().StartChildWorkflow(t.Context(), access.BearerToken, env.RunID, "check", []byte(childValidationProposal)); err != nil {
		t.Fatal(err)
	}
	child, err := queue.GetChild(t.Context(), triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}, StageOccurrence: env.ChildWorkflowOrigin.StageOccurrence, InvocationKey: "check"})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := service.grants.key.VerifyChildWorkflowGrant(access.BearerToken)
	if err != nil {
		t.Fatal(err)
	}
	origin := childworkflow.Origin{GrantID: grant.ID, Gaggle: grant.Gaggle, RunID: grant.RunID, StageOccurrence: grant.StageOccurrence, AttemptID: grant.AttemptID, ConfigDigest: grant.ConfigDigest, PolicyDigest: grant.PolicyDigest}
	project := f.applied.Gaggles[0].Spec.Project
	host := &daemonChildHandoff{layout: f.layout.ForGaggle(env.Gaggle), project: project, repoCloneURL: func(apiv1.RepoRef) (string, error) { return "unused-repository", nil }}
	return handoffDaemonFixture{host, service, queue, run, env, child, origin.Binding(grant.ExpiresAt)}
}

func recordDaemonHandoffWait(t *testing.T, f handoffDaemonFixture, request runner.ChildHandoffRequest) {
	t.Helper()
	if err := f.run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": runner.ChildWaitKind, "childWait": map[string]any{"version": 1, "parentRunId": f.env.RunID, "request": request, "policyAttempts": 0, "infrastructureFailures": 0}}}); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonChildHandoffSelectsAcceptedWaitAndVerifiedCompletion(t *testing.T) {
	f := newHandoffDaemonFixture(t)
	request, err := f.host.Await(t.Context(), f.env)
	if err != nil || request.Action != "wait" || request.ChildRunID != f.child.RunID {
		t.Fatal(request, err)
	}
	closed, closeObserver := context.WithCancel(t.Context())
	closeObserver()
	if final, err := f.host.Await(closed, f.env); err != nil || final != request {
		t.Fatal("invocation exit lost already-accepted child", final, err)
	}
	wrong := request
	wrong.Gaggle = "other"
	wrong.RequestID = childHandoffRequestDigest(wrong)
	if _, err := f.host.child(t.Context(), f.service, wrong); err == nil {
		t.Fatal("cross-gaggle handoff accepted")
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.queue}
	result, err := coordinator.CaptureResult(t.Context(), f.child, nil, childworkflow.TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: time.Now(), Summary: "verified bounded summary", References: []string{"artifact:report"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.queue.SetChildState(t.Context(), f.child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: result.ResultRef}, time.Now()); err != nil {
		t.Fatal(err)
	}
	completion, err := f.host.Wait(t.Context(), request)
	if err != nil || completion.ResultRef != result.ResultRef || completion.Summary != "verified bounded summary" {
		t.Fatal(completion, err)
	}
	if request, err := f.host.observe(t.Context(), f.service, f.env); err != nil || request.RequestID != "" {
		t.Fatal("terminal child without disposition caused repeated yield", request, err)
	}
	if _, err := f.queue.RequestChildDisposition(t.Context(), triggerqueue.ChildDispositionRequest{Identity: f.child.Identity, Action: "discard", ResultRef: result.ResultRef, Authority: f.grant}, time.Now()); err != nil {
		t.Fatal(err)
	}
	disposition, err := f.host.Await(t.Context(), f.env)
	if err != nil || disposition.Action != "discard" {
		t.Fatal(disposition, err)
	}
	if err := f.host.Yield(t.Context(), disposition, runner.ChildWorkspaceCustody{Path: t.TempDir(), RepoRef: f.host.project}); err == nil {
		t.Fatal("custody effect accepted before durable wait marker")
	}
	recordDaemonHandoffWait(t, f, disposition)
	changed := disposition
	changed.Action = "merge"
	changed.RequestID = childHandoffRequestDigest(changed)
	if _, _, err := f.host.parkedParent(changed); err == nil {
		t.Fatal("different receipt accepted under parked origin")
	}
	if err := f.host.Yield(t.Context(), disposition, runner.ChildWorkspaceCustody{Path: t.TempDir(), RepoRef: f.host.project}); err != nil {
		t.Fatal(err)
	}
	child, err := f.queue.GetChild(t.Context(), f.child.Identity)
	if err != nil || child.AcknowledgedAt.IsZero() {
		t.Fatal("verified disposition did not release slot", child, err)
	}
	if err := f.host.Yield(t.Context(), disposition, runner.ChildWorkspaceCustody{Path: t.TempDir(), RepoRef: f.host.project}); err != nil {
		t.Fatal("disposition retry lost custody", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.host.Await(ctx, f.env); err == nil {
		t.Fatal("empty cancelled observer did not stop")
	}
}

func TestDaemonChildHandoffIncludesUncertainPublicationWithoutChangingResult(t *testing.T) {
	f := newHandoffDaemonFixture(t)
	request, err := f.host.Await(t.Context(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	lineage := journal.ChildLineage{Gaggle: f.child.Identity.Gaggle, ParentRunID: f.child.Identity.ParentRunID, StageOccurrence: f.child.Identity.StageOccurrence, InvocationKey: f.child.Identity.InvocationKey, AcceptanceID: f.child.AcceptanceID, SourceDigest: f.child.ProposalDigest, EnvelopeDigest: journal.Digest([]byte("envelope"))}
	intent := childpublication.BranchIntent{Version: 1, RunID: f.child.RunID, Lineage: lineage, Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}, Remote: "https://github.com/owner/repo.git", Head: "factory/children/" + f.child.RunID, Base: "main", Commit: strings.Repeat("a", 40)}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := f.queue.PrepareChildExecutionPublication(t.Context(), f.child.Identity, f.child.RunID, "branch", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.queue.BeginChildExecutionPublicationEffect(t.Context(), publication, f.child.RunID); err != nil {
		t.Fatal(err)
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.queue}
	result, err := coordinator.CaptureResult(t.Context(), f.child, nil, childworkflow.TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: time.Now(), Summary: "publication reply unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.queue.SetChildState(t.Context(), f.child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: result.ResultRef}, time.Now()); err != nil {
		t.Fatal(err)
	}
	completion, err := f.host.Wait(t.Context(), request)
	if err != nil || completion.ResultRef != result.ResultRef || len(completion.Publications) != 1 {
		t.Fatal(completion, err)
	}
	status := completion.Publications[0]
	if status.State != "effect_pending" || !status.NeedsHuman || status.IntentDigest != publication.Digest || status.Head != intent.Head {
		t.Fatal("parent lost uncertain publication evidence", status)
	}
	retained, err := f.queue.ChildResult(t.Context(), f.child.Identity)
	if err != nil || retained.ReceiptDigest != result.ResultRef {
		t.Fatal("projection changed immutable child result", retained, err)
	}
}

// The daemon must select the requested branch even when a sibling starts later.
// A single parked branch must not make the whole run appear suspended.
func TestDaemonChildHandoffSelectsExactParallelOrigin(t *testing.T) {
	f := newHandoffDaemonFixture(t)
	if err := f.run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := f.run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{{Branch: 1}, {Branch: 2}}}); err != nil {
		t.Fatal(err)
	}
	var requests []runner.ChildHandoffRequest
	for branch := 1; branch <= 2; branch++ {
		_, origin, err := f.run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Branch: branch, Stage: "plan", Attempt: 1}, false)
		if err != nil {
			t.Fatal(err)
		}
		child := fmt.Sprintf("child-branch-%d", branch)
		request := runner.ChildHandoffRequest{Gaggle: f.env.Gaggle, ParentRunID: f.env.RunID, Action: "wait", ChildRunID: child, AcceptanceID: "trigger-" + child, InvocationKey: child, SourceDigest: journal.Digest([]byte("source")), Origin: *origin}
		request.RequestID = childHandoffRequestDigest(request)
		if branch == 2 {
			if _, stage, err := f.host.parkedParent(requests[0]); err != nil || stage != "plan" {
				t.Fatal("later sibling hid first wait", stage, err)
			}
			if _, _, err := f.host.parkedParent(request); err == nil {
				t.Fatal("unparked sibling borrowed first custody")
			}
			reader, err := journal.OpenReadOnly(f.run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			if runner.ParkedOnChild(events) {
				t.Fatal("running sibling lost whole-run capacity")
			}
		}
		if err := f.run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Branch: branch, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": runner.ChildWaitKind, "childWait": journal.ChildWaitHeader{Version: 1, ParentRunID: f.env.RunID, Request: journal.ChildHandoffRequest(request)}}}); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
	}
	for _, request := range requests {
		if _, stage, err := f.host.parkedParent(request); err != nil || stage != "plan" {
			t.Fatal("exact parked branch not selected", stage, err)
		}
	}
	substituted := requests[0]
	substituted.Origin = requests[1].Origin
	substituted.RequestID = childHandoffRequestDigest(substituted)
	if _, _, err := f.host.parkedParent(substituted); err == nil {
		t.Fatal("sibling origin substituted for another child receipt")
	}
}
