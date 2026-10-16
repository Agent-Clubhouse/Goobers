package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// This joins the actual queue/factory result to the host handoff and authenticated
// parent decision. The configured stage journal is a fixture, not Runner.Start;
// worker transport is simulated. Public parent admission remains gated.
func TestProductionChildFactoryReturnsResultThroughParentDecision(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "normal"
		if lost {
			name = "recovered-worker"
		}
		t.Run(name, func(t *testing.T) {
			testProductionChildFactory(t, childFactoryTestOptions{queued: true, handoff: true, lost: lost})
		})
	}
}

func prepareQueuedParentReturn(t *testing.T, f childKitFixture, endpoint string) func() {
	t.Helper()
	s := f.writer.service
	s.childDispatch = newDaemonTriggerService()
	host := &daemonChildHandoff{layout: s.layout.ForGaggle(f.parentEnv.Gaggle), project: f.parent.applied.Gaggles[0].Spec.Project,
		repoCloneURL: func(apiv1.RepoRef) (string, error) { return "unused-scratch-repository", nil }}
	request, err := host.Await(t.Context(), f.parentEnv)
	if err != nil || request.Action != "wait" || request.ChildRunID != f.child.RunID {
		t.Fatal("parent did not observe durable acceptance", request, err)
	}
	fixture := handoffDaemonFixture{host: host, service: s, queue: s.childQueue, run: f.parentRun, env: f.parentEnv, child: f.child}
	recordDaemonHandoffWait(t, fixture, request)
	return func() {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result, err := host.Wait(ctx, request)
		if err != nil || result.State != string(triggerqueue.ChildCompleted) || result.ResultRef == "" {
			t.Fatal("host did not read the actual worker result", result, err)
		}
		continueQueuedParent(t, f, request.RequestID)
		status := queuedParentStatus(t, f, endpoint)
		if status.ResultRef != result.ResultRef || status.Acknowledged || status.Disposition != nil {
			t.Fatal("returning a result released custody before a decision", status)
		}
		choice := apicontract.ChildWorkflowResolveRequest{InvocationKey: f.child.Identity.InvocationKey, Action: "discard", ResultRef: result.ResultRef}
		var accepted apicontract.ChildWorkflowResolutionResponse
		queuedParentPOST(t, f, endpoint, "resolve", choice, &accepted)
		if accepted.Applied || accepted.RequestDigest == "" {
			t.Fatal("accepting a parent decision claimed it was applied", accepted)
		}
		disposition, err := host.Await(ctx, f.parentEnv)
		if err != nil || disposition.Action != "discard" || disposition.DispositionDigest != accepted.RequestDigest {
			t.Fatal("host did not observe exact authenticated decision", disposition, err)
		}
		custody := runner.ChildWorkspaceCustody{Path: f.parentEnv.Workspace, RepoRef: host.project}
		if err := host.Yield(ctx, disposition, custody); err == nil {
			t.Fatal("decision applied before the parent recorded its yield")
		}
		recordDaemonHandoffWait(t, fixture, disposition)
		if err := host.Yield(ctx, disposition, custody); err != nil {
			t.Fatal("parent could not apply verified child disposition", err)
		}
		if err := host.Yield(ctx, disposition, custody); err != nil {
			t.Fatal("replaying applied disposition lost custody", err)
		}
		continueQueuedParent(t, f, disposition.RequestID)
		status = queuedParentStatus(t, f, endpoint)
		if !status.Acknowledged || status.Disposition == nil || !status.Disposition.Applied || status.ResultRef != result.ResultRef {
			t.Fatal("parent continuation lost applied decision or result", status)
		}
		current, err := s.childQueue.CurrentChild(ctx, f.child.Identity.ChildParent, f.child.Identity.StageOccurrence)
		if err != nil || current.RunID != "" {
			t.Fatal("applied disposition retained the unfinished child slot", current, err)
		}
	}
}

func continueQueuedParent(t *testing.T, f childKitFixture, requestID string) {
	t.Helper()
	if err := f.parentRun.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1,
		Runner: map[string]any{"kind": runner.ChildContinuedKind, "requestId": requestID}}); err != nil {
		t.Fatal(err)
	}
}

func queuedParentStatus(t *testing.T, f childKitFixture, endpoint string) apicontract.ChildWorkflowResponse {
	t.Helper()
	var status apicontract.ChildWorkflowResponse
	queuedParentPOST(t, f, endpoint, "status", apicontract.ChildWorkflowStatusRequest{InvocationKey: f.child.Identity.InvocationKey}, &status)
	return status
}

func queuedParentPOST(t *testing.T, f childKitFixture, endpoint, operation string, body, out any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint+apicontract.RunsPath+"/"+f.parentEnv.RunID+"/child-workflows/"+operation, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.parentToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("parent %s: status %d: %s", operation, response.StatusCode, message)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
