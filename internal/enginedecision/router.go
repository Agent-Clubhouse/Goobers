// Package enginedecision delivers operator decisions to engine-driven runs
// over Temporal Updates (HA-P4-12, #7760). Human-gate decisions use the
// occurrence-bound goobers.gate-decision.v1 protocol; terminal interventions
// use goobers.hitl-intent.v1. Every UpdateID binds the request payload, and
// every acknowledgement is checked against the request it answers, so a
// request ID reused with changed content can never accept a stale ack.
package enginedecision

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/engine"
)

// ErrStaleAcknowledgement refuses an acknowledgement for different content.
var ErrStaleAcknowledgement = errors.New("enginedecision: acknowledgement does not answer this request")

// Temporal is the narrow client surface decisions need.
type Temporal interface {
	UpdateWorkflow(context.Context, client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error)
	QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...interface{}) (converter.EncodedValue, error)
}

type pendingActivityReader interface {
	DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// Router implements intervention.EngineDecisions over Temporal.
type Router struct {
	Client Temporal
}

// WorkflowID is an engine run's Temporal workflow ID. The cloud start fence
// starts each run under its RunID with duplicate IDs rejected.
func WorkflowID(runID string) string { return runID }

// GateState reads the run's committed pending human-gate occurrence.
func (r Router) GateState(ctx context.Context, runID string) (engine.GateDecisionState, error) {
	var state engine.GateDecisionState
	return state, r.query(ctx, runID, engine.GateDecisionStateQuery, &state)
}

// HITLState reads the run's terminal hold and generation.
func (r Router) HITLState(ctx context.Context, runID string, requestID ...string) (engine.HITLState, error) {
	var state engine.HITLState
	name := engine.HITLStateQuery
	var args []interface{}
	if len(requestID) > 0 {
		name = engine.HITLRequestStateQuery
		args = []interface{}{requestID[0]}
	}
	if err := r.query(ctx, runID, name, &state, args...); err != nil {
		return state, err
	}
	// Read retry evidence maintained by Temporal; no per-attempt workflow
	// signal or command is needed to make uncertain delivery observable.
	if state.Delivery == nil {
		return state, nil
	}
	reader, ok := r.Client.(pendingActivityReader)
	if !ok {
		state.Delivery.LastError = "pending activity details unavailable"
		return state, nil
	}
	response, err := reader.DescribeWorkflowExecution(ctx, WorkflowID(runID), "")
	if err != nil {
		return state, fmt.Errorf("enginedecision: read pending intervention delivery: %w", err)
	}
	for _, pending := range response.PendingActivities {
		if pending.GetActivityType().GetName() != engine.ActCommitTerminalIntervention {
			continue
		}
		state.Delivery.Attempt = pending.GetAttempt()
		state.Delivery.LastError = pending.GetLastFailure().GetMessage()
		break
	}
	return state, nil
}

// DecideGate delivers one occurrence-bound human-gate decision.
func (r Router) DecideGate(ctx context.Context, request engine.GateDecisionRequest) (engine.GateDecisionAck, error) {
	var ack engine.GateDecisionAck
	if err := r.update(ctx, request.RunID, engine.GateDecisionUpdateName, engine.GateDecisionUpdateID(request), request, &ack); err != nil {
		return engine.GateDecisionAck{}, err
	}
	if err := ack.ValidateRequest(request); err != nil {
		return engine.GateDecisionAck{}, errors.Join(ErrStaleAcknowledgement, err)
	}
	return ack, nil
}

// Intend delivers one terminal intervention intent.
func (r Router) Intend(ctx context.Context, intent engine.HITLIntent) (engine.HITLAck, error) {
	var ack engine.HITLAck
	if err := r.update(ctx, intent.RunID, engine.HITLUpdateName, engine.HITLIntentUpdateID(intent), intent, &ack); err != nil {
		return engine.HITLAck{}, err
	}
	// The digest binds resolution, gate, decision and every other intent
	// field; the remaining checks keep the envelope honest on its own.
	if ack.IntentDigest != engine.HITLIntentDigest(intent) || ack.Protocol != intent.Protocol || ack.Version != intent.Version || ack.RunID != intent.RunID ||
		ack.RequestID != intent.RequestID || ack.Kind != intent.Kind || ack.TerminalGeneration != intent.ExpectedTerminalGeneration {
		return engine.HITLAck{}, ErrStaleAcknowledgement
	}
	return ack, nil
}

func (r Router) update(ctx context.Context, runID, name, id string, arg, out any) error {
	if r.Client == nil {
		return errors.New("enginedecision: no Temporal client")
	}
	handle, err := r.Client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{WorkflowID: WorkflowID(runID), UpdateID: id, UpdateName: name,
		Args: []interface{}{arg}, WaitForStage: client.WorkflowUpdateStageCompleted})
	if err != nil {
		return fmt.Errorf("enginedecision: deliver %s: %w", name, err)
	}
	return handle.Get(ctx, out)
}

func (r Router) query(ctx context.Context, runID, name string, out any, args ...interface{}) error {
	if r.Client == nil {
		return errors.New("enginedecision: no Temporal client")
	}
	value, err := r.Client.QueryWorkflow(ctx, WorkflowID(runID), "", name, args...)
	if err != nil {
		return fmt.Errorf("enginedecision: query %s: %w", name, err)
	}
	return value.Get(out)
}
