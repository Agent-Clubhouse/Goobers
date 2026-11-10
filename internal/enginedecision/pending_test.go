package enginedecision

import (
	"context"
	"errors"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	failurepb "go.temporal.io/api/failure/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/engine"
)

type pendingServer struct {
	dedupingServer
	state      engine.HITLState
	response   *workflowservice.DescribeWorkflowExecutionResponse
	err        error
	calls      int
	workflowID string
	queryName  string
	queryArgs  []interface{}
}

func (s *pendingServer) QueryWorkflow(_ context.Context, _, _, name string, args ...interface{}) (converter.EncodedValue, error) {
	s.queryName, s.queryArgs = name, args
	return stateValue{s.state}, nil
}
func (s *pendingServer) DescribeWorkflowExecution(_ context.Context, id, run string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	s.calls++
	s.workflowID = id
	return s.response, s.err
}
func TestHITLStateReportsServerSideJournalRetries(t *testing.T) {
	server := &pendingServer{state: engine.HITLState{Delivery: &engine.HITLDelivery{RequestID: "intent-1", Status: "intervention pending (journal delivery retrying)"}}, response: &workflowservice.DescribeWorkflowExecutionResponse{PendingActivities: []*workflowpb.PendingActivityInfo{
		{ActivityType: &commonpb.ActivityType{Name: "unrelated"}, Attempt: 99},
		{ActivityType: &commonpb.ActivityType{Name: engine.ActCommitTerminalIntervention}, Attempt: 7, LastFailure: &failurepb.Failure{Message: "journal reply lost"}},
	}}}
	state, err := (Router{Client: server}).HITLState(t.Context(), "run-1")
	if err != nil || state.Delivery == nil || state.Delivery.Attempt != 7 || state.Delivery.LastError != "journal reply lost" || state.Delivery.RequestID != "intent-1" || server.workflowID != WorkflowID("run-1") {
		t.Fatalf("state=%+v delivery=%+v error=%v", state, state.Delivery, err)
	}
	if len(server.delivered) != 0 {
		t.Fatal("status added an Update instead of reading Temporal retry evidence")
	}
	server.state.Delivery = nil
	if _, err := (Router{Client: server}).HITLState(t.Context(), "run-1"); err != nil || server.calls != 1 {
		t.Fatalf("settled state described pending activities: calls=%d err=%v", server.calls, err)
	}
	server.state.Delivery = &engine.HITLDelivery{RequestID: "intent-1"}
	server.err = errors.New("describe unavailable")
	if _, err := (Router{Client: server}).HITLState(t.Context(), "run-1"); !errors.Is(err, server.err) {
		t.Fatalf("lost status error: %v", err)
	}
}

type stateValue struct{ state engine.HITLState }

func (v stateValue) HasValue() bool { return true }
func (v stateValue) Get(out interface{}) error {
	payload, err := converter.GetDefaultDataConverter().ToPayload(v.state)
	if err != nil {
		return err
	}
	return converter.GetDefaultDataConverter().FromPayload(payload, out)
}

func TestHITLRequestStateRoutesTheOriginalRequestKey(t *testing.T) {
	server := &pendingServer{state: engine.HITLState{RequestKnown: true, RequestGeneration: 3}}
	router := Router{Client: server}
	state, err := router.HITLState(t.Context(), "run-1", "original-key")
	if err != nil || !state.RequestKnown || state.RequestGeneration != 3 || server.queryName != engine.HITLRequestStateQuery || len(server.queryArgs) != 1 || server.queryArgs[0] != "original-key" {
		t.Fatalf("state=%+v query=%s args=%v err=%v", state, server.queryName, server.queryArgs, err)
	}
	if _, err := router.HITLState(t.Context(), "run-1"); err != nil || server.queryName != engine.HITLStateQuery || len(server.queryArgs) != 0 {
		t.Fatalf("legacy query=%s args=%v err=%v", server.queryName, server.queryArgs, err)
	}
}
