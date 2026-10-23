package intervention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
)

// EngineDecisions delivers human-gate decisions to a run the engine drives.
// Implementations derive payload-bound Temporal UpdateIDs and refuse any
// acknowledgement that does not answer the exact request
// (enginedecision.Router). The workflow, not this service, owns the journal.
type EngineDecisions interface {
	GateState(context.Context, string) (engine.GateDecisionState, error)
	DecideGate(context.Context, engine.GateDecisionRequest) (engine.GateDecisionAck, error)
}

// engineRequestID is the caller's idempotency key, or a key derived from the
// full intervention content so an identical retry reaches the same request.
func engineRequestID(action string, input httpapi.InterventionRequest) string {
	if key := strings.TrimSpace(input.IdempotencyKey); key != "" {
		return key
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{action, input.RunID, input.Stage, input.Actor, input.Decision, input.Rationale, input.InstructionAddendum}, "\x00")))
	return "intervention:" + hex.EncodeToString(sum[:])
}

// engineRefusal maps the workflow's typed refusal to a conflict, keeping its
// stable code; any other delivery failure is reported as unavailable.
func engineRefusal(action string, err error) error {
	var refusal *temporal.ApplicationError
	if errors.As(err, &refusal) && refusal.Type() != "" {
		return interventionConflict(refusal.Type(), refusal.Message())
	}
	return httpapi.NewInterventionError(http.StatusBadGateway, "engine_unreachable", fmt.Sprintf("the engine did not accept the %s", action), err)
}

// EngineTerminalInterventionUnsupported protects histories whose workflow
// has not advertised the fenced terminal-journal capability.
const EngineTerminalInterventionUnsupported = "engine_terminal_intervention_unsupported"

// engineTerminalRefusal keeps N-1 histories and unwired workers fail-closed.
func engineTerminalRefusal(action string, resolved resolvedInterventionRun) error {
	return interventionConflict(EngineTerminalInterventionUnsupported, fmt.Sprintf(
		"%s would reopen engine run %q after its terminal, which its workflow cannot durably journal", action, resolved.runID))
}

// engineApprove decides the pending human gate when the run holds one, and
// otherwise resolves the terminal the operator is approving past.
func (s *Service) engineApprove(ctx context.Context, resolved resolvedInterventionRun, input httpapi.InterventionRequest, decision string) (httpapi.InterventionResult, error) {
	gate, err := s.engineDecisions.GateState(ctx, resolved.runID)
	if err != nil {
		return httpapi.InterventionResult{}, engineRefusal("approve", err)
	}
	if !gate.Pending || gate.Gate != input.Stage {
		return s.engineTerminal(ctx, "approve", resolved, input)
	}
	request := engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: resolved.runID,
		RequestID: engineRequestID("approve", input), Actor: input.Actor, Gate: input.Stage, GateOccurrence: gate.GateOccurrence,
		ExpectedTerminalGeneration: gate.TerminalGeneration, Decision: decision}
	if _, err := s.engineDecisions.DecideGate(ctx, request); err != nil {
		return httpapi.InterventionResult{}, engineRefusal("approve", err)
	}
	return currentInterventionResult(resolved)
}

// terminalDecisions is deliberately optional: an older gate-only composition
// cannot admit terminal interventions until it advertises the new capability.
type terminalDecisions interface {
	HITLState(context.Context, string) (engine.HITLState, error)
	Intend(context.Context, engine.HITLIntent) (engine.HITLAck, error)
}

func (s *Service) engineTerminal(ctx context.Context, action string, resolved resolvedInterventionRun, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	router, ok := s.engineDecisions.(terminalDecisions)
	if !ok {
		return httpapi.InterventionResult{}, engineTerminalRefusal(action, resolved)
	}
	state, err := router.HITLState(ctx, resolved.runID)
	if err != nil {
		return httpapi.InterventionResult{}, engineRefusal(action, err)
	}
	if !state.LiveTerminalInterventions {
		return httpapi.InterventionResult{}, engineTerminalRefusal(action, resolved)
	}
	intent := engine.HITLIntent{Protocol: engine.HITLProtocol, Version: engine.HITLProtocolVersion,
		RunID: resolved.runID, RequestID: engineRequestID(action, input), Actor: input.Actor,
		ExpectedTerminalGeneration: state.TerminalGeneration, Kind: engine.HITLResolveEscalation,
		Gate: input.Stage, Resolution: action, Decision: input.Decision, Rationale: input.Rationale,
		InstructionAddendum: input.InstructionAddendum}
	if action == "rerun" {
		intent.Kind, intent.Stage, intent.Gate, intent.Resolution = engine.HITLRerunStage, input.Stage, "", ""
	}
	// Keep the caller's raw fields in the payload-bound identity. The workflow
	// defaults an absent decision to pass when choosing the branch and recording
	// run.resumed, just as the local runner does after hashing the request.
	if _, err := router.Intend(ctx, intent); err != nil {
		return httpapi.InterventionResult{}, engineRefusal(action, err)
	}
	return currentInterventionResult(resolved)
}
