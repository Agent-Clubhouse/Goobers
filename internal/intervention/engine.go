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

// EngineDecisions delivers operator decisions to a run the engine drives.
// Implementations derive payload-bound Temporal UpdateIDs and refuse any
// acknowledgement that does not answer the exact request
// (enginedecision.Router). The workflow, not this service, owns the journal.
type EngineDecisions interface {
	GateState(context.Context, string) (engine.GateDecisionState, error)
	HITLState(context.Context, string) (engine.HITLState, error)
	DecideGate(context.Context, engine.GateDecisionRequest) (engine.GateDecisionAck, error)
	Intend(context.Context, engine.HITLIntent) (engine.HITLAck, error)
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

func (s *Service) engineIntent(ctx context.Context, resolved resolvedInterventionRun, action string, input httpapi.InterventionRequest, intent engine.HITLIntent) (httpapi.InterventionResult, error) {
	state, err := s.engineDecisions.HITLState(ctx, resolved.runID)
	if err != nil {
		return httpapi.InterventionResult{}, engineRefusal(action, err)
	}
	intent.Protocol, intent.Version = engine.HITLProtocol, engine.HITLProtocolVersion
	intent.RunID, intent.RequestID, intent.Actor = resolved.runID, engineRequestID(action, input), input.Actor
	intent.ExpectedTerminalGeneration = state.TerminalGeneration
	if _, err := s.engineDecisions.Intend(ctx, intent); err != nil {
		return httpapi.InterventionResult{}, engineRefusal(action, err)
	}
	return currentInterventionResult(resolved)
}

// engineApprove decides the pending human gate when the run holds one, and
// otherwise resolves the terminal the operator is approving past.
func (s *Service) engineApprove(ctx context.Context, resolved resolvedInterventionRun, input httpapi.InterventionRequest, decision string) (httpapi.InterventionResult, error) {
	gate, err := s.engineDecisions.GateState(ctx, resolved.runID)
	if err != nil {
		return httpapi.InterventionResult{}, engineRefusal("approve", err)
	}
	if !gate.Pending || gate.Gate != input.Stage {
		return s.engineIntent(ctx, resolved, "approve", input, engine.HITLIntent{Kind: engine.HITLResolveEscalation,
			Gate: input.Stage, Resolution: engine.HITLResolutionApprove, Decision: decision, Rationale: input.Rationale})
	}
	request := engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: resolved.runID,
		RequestID: engineRequestID("approve", input), Actor: input.Actor, Gate: input.Stage, GateOccurrence: gate.GateOccurrence,
		ExpectedTerminalGeneration: gate.TerminalGeneration, Decision: decision}
	if _, err := s.engineDecisions.DecideGate(ctx, request); err != nil {
		return httpapi.InterventionResult{}, engineRefusal("approve", err)
	}
	return currentInterventionResult(resolved)
}
