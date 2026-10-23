package enginedecision

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/engine"
)

// dedupingServer models Temporal's UpdateID deduplication: a repeated
// UpdateID returns the first outcome without reaching the workflow.
// answerFor stands in for the workflow's handler on first delivery.
type dedupingServer struct {
	outcomes  map[string]json.RawMessage
	delivered []client.UpdateWorkflowOptions
	answerFor func(any) any
}

type handle struct{ data json.RawMessage }

func (handle) WorkflowID() string                     { return "" }
func (handle) RunID() string                          { return "" }
func (handle) UpdateID() string                       { return "" }
func (h handle) Get(_ context.Context, out any) error { return json.Unmarshal(h.data, out) }

func (s *dedupingServer) UpdateWorkflow(_ context.Context, options client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	if data, ok := s.outcomes[options.UpdateID]; ok {
		return handle{data}, nil
	}
	s.delivered = append(s.delivered, options)
	data, err := json.Marshal(s.answerFor(options.Args[0]))
	if err != nil {
		return nil, err
	}
	s.outcomes[options.UpdateID] = data
	return handle{data}, nil
}

func (*dedupingServer) QueryWorkflow(context.Context, string, string, string, ...interface{}) (converter.EncodedValue, error) {
	return nil, errors.New("unused")
}

func ackFor(arg any) any {
	r := arg.(engine.GateDecisionRequest)
	return engine.GateDecisionAck{Protocol: r.Protocol, Version: r.Version, RunID: r.RunID, RequestID: r.RequestID, Actor: r.Actor, Gate: r.Gate,
		GateOccurrence: r.GateOccurrence, TerminalGeneration: r.ExpectedTerminalGeneration, Decision: r.Decision}
}

func gateRequest(decision string) engine.GateDecisionRequest {
	return engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: "run-1", RequestID: "key-1",
		Actor: "alice", Gate: "approve-release", GateOccurrence: 41, ExpectedTerminalGeneration: 0, Decision: decision}
}

func TestDecideGateBindsUpdateIDToThePayload(t *testing.T) {
	server := &dedupingServer{outcomes: map[string]json.RawMessage{}, answerFor: ackFor}
	router := Router{Client: server}
	pass := gateRequest("pass")
	ack, err := router.DecideGate(t.Context(), pass)
	if err != nil || ack.Decision != "pass" {
		t.Fatalf("decide = %+v %v", ack, err)
	}
	sent := server.delivered[0]
	if sent.UpdateID != engine.GateDecisionUpdateID(pass) || sent.UpdateName != engine.GateDecisionUpdateName || sent.WorkflowID != "run-1" || sent.WaitForStage != client.WorkflowUpdateStageCompleted {
		t.Fatalf("delivery options = %+v", sent)
	}
	// The same request ID with changed content is a new delivery the workflow
	// judges (and refuses as key reuse); it never shares the old outcome.
	if _, err := router.DecideGate(t.Context(), gateRequest("fail")); err != nil {
		t.Fatal(err)
	}
	if len(server.delivered) != 2 || server.delivered[1].UpdateID == sent.UpdateID {
		t.Fatalf("changed payload reused the transport identity: %+v", server.delivered)
	}
	// An exact retry is deduplicated by the server.
	if _, err := router.DecideGate(t.Context(), pass); err != nil || len(server.delivered) != 2 {
		t.Fatalf("exact retry was redelivered: %v (%d deliveries)", err, len(server.delivered))
	}
}

// REQUIRED regression (#7760): even if the transport replays an outcome for
// a request ID reused with changed content, the client refuses the stale
// acknowledgement instead of reporting the changed decision as accepted.
func TestChangedPayloadReuseCannotAcceptAStaleAck(t *testing.T) {
	stale, _ := json.Marshal(ackFor(gateRequest("pass")))
	server := &dedupingServer{outcomes: map[string]json.RawMessage{}, answerFor: ackFor}
	changed := gateRequest("fail")
	// A server (or an older client) keyed by request ID alone replays "pass".
	server.outcomes[engine.GateDecisionUpdateID(changed)] = stale
	if _, err := (Router{Client: server}).DecideGate(t.Context(), changed); !errors.Is(err, ErrStaleAcknowledgement) {
		t.Fatalf("stale gate ack accepted: %v", err)
	}

	intent := engine.HITLIntent{Protocol: engine.HITLProtocol, Version: engine.HITLProtocolVersion, Kind: engine.HITLResolveEscalation, RunID: "run-1",
		RequestID: "key-2", Actor: "alice", ExpectedTerminalGeneration: 2, Gate: "review", Resolution: engine.HITLResolutionApprove}
	id, err := HITLIntentUpdateID(intent)
	if err != nil {
		t.Fatal(err)
	}
	denied := intent
	denied.Resolution = engine.HITLResolutionDeny
	deniedID, _ := HITLIntentUpdateID(denied)
	if deniedID == id {
		t.Fatal("HITL UpdateID does not bind the payload")
	}
	staleIntent, _ := json.Marshal(engine.HITLAck{Protocol: intent.Protocol, Version: intent.Version, RunID: "run-1", RequestID: "key-2", Kind: engine.HITLRerunStage, TerminalGeneration: 1})
	server.outcomes[deniedID] = staleIntent
	if _, err := (Router{Client: server}).Intend(t.Context(), denied); !errors.Is(err, ErrStaleAcknowledgement) {
		t.Fatalf("stale HITL ack accepted: %v", err)
	}
}

func TestRouterRequiresAClient(t *testing.T) {
	if _, err := (Router{}).DecideGate(t.Context(), gateRequest("pass")); err == nil {
		t.Fatal("delivered without a client")
	}
	if _, err := (Router{}).GateState(t.Context(), "run-1"); err == nil {
		t.Fatal("queried without a client")
	}
}
