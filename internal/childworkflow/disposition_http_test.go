package childworkflow

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildResolutionHTTPOnlyRequestsUntilHostApplies(t *testing.T) {
	handler, service, authority, token := childHTTPFixture(t, true)
	origin := authority.current.Origin
	submission, err := service.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "inspect", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	c := WorkspaceCoordinator{Queue: service.Queue}
	result, err := c.CaptureResult(t.Context(), submission.Child, nil, TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: service.now(), Summary: "No workspace effects"})
	if err != nil {
		t.Fatal(err)
	}
	body := apicontract.ChildWorkflowResolveRequest{InvocationKey: "inspect", Action: "discard", ResultRef: result.ResultRef}
	response := childHTTPCall(t, handler, token, origin.RunID, "resolve", "", body)
	if response.Code != http.StatusConflict {
		t.Fatal("resolved before terminal observation", response.Code, response.Body)
	}
	if err := service.Queue.SetChildState(t.Context(), submission.Child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: result.ResultRef}, service.now()); err != nil {
		t.Fatal(err)
	}
	response = childHTTPCall(t, handler, token, origin.RunID, "resolve", "", body)
	var received apicontract.ChildWorkflowResolutionResponse
	if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &received) != nil || received.Applied || received.ResultRef != result.ResultRef {
		t.Fatal(response.Code, response.Body)
	}
	child, err := service.Queue.GetChild(t.Context(), submission.Child.Identity)
	if err != nil || !child.AcknowledgedAt.IsZero() {
		t.Fatal("request released child", err)
	}
	if _, err := c.ApplyDisposition(t.Context(), child, nil, service.now()); err != nil {
		t.Fatal(err)
	}
	response = childHTTPCall(t, handler, token, origin.RunID, "resolve", "", body)
	if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &received) != nil || !received.Applied || received.AppliedAt == nil {
		t.Fatal(response.Code, response.Body)
	}
	if err := service.Queue.RevokeChildAuthority(t.Context(), origin.Binding(service.now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	response = childHTTPCall(t, handler, token, origin.RunID, "resolve", "", body)
	if response.Code != http.StatusForbidden {
		t.Fatal("revoked grant read disposition", response.Code, response.Body)
	}
}
