package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
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
	for _, action := range []string{"merge", "replace", "discard"} {
		unsupported := request
		unsupported.Action = action
		unsupported.RequestID = childHandoffRequestDigest(unsupported)
		if err := f.host.Yield(t.Context(), unsupported, runner.ChildWorkspaceCustody{Path: t.TempDir(), RepoRef: f.host.project}); err == nil {
			t.Fatalf("unavailable disposition %q accepted", action)
		}
	}
	child, err := f.queue.GetChild(t.Context(), f.child.Identity)
	if err != nil || !child.AcknowledgedAt.IsZero() {
		t.Fatal("completion released unresolved custody", child, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.host.Await(ctx, f.env); err == nil {
		t.Fatal("empty cancelled observer did not stop")
	}
}
