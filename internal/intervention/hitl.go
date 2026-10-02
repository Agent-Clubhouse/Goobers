package main

// enginehitl.go wires the daemon's operator-intervention surface to #3883's
// versioned Temporal HITL protocol.
//
// Before this file, every intervention on an engine-driven run was refused at
// one place (runInterventionService.resolve) because all four verbs —
// approve, override, rerun, deny — reach for an in-process runner that has
// never executed a stage of an engine run and whose journal has a live writer
// on the other side of a Temporal workflow (#3847). The refusal was correct;
// it was also the reason merge-review and pr-remediation could not cut over.
//
// The fix is not to make the runner work on engine runs. It is to send the
// operator's INTENT to the workflow that owns the run and let it decide. So:
//
//   - Runner-driven runs take exactly the path they took before. Nothing in
//     this file is on their critical path.
//   - Engine-driven runs are translated into an engine.HITLIntent and
//     delivered as a Temporal Update. The daemon reports success only after
//     the workflow has durably accepted and acted on it.
//   - A daemon with no engine client configured keeps the old refusal
//     verbatim, so a type-1/type-2 install's behaviour is unchanged.
//
// Validation lives in the workflow, not here. This side deliberately does not
// re-derive gates, branches, or attempt numbers from the on-disk journal: the
// workflow holds the authoritative in-memory walk, and a second opinion
// computed from a snapshot is exactly how an operator ends up applying a
// verdict to a state that has since moved.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

// hitlDeliverer is the slice of engine.HITLDeliverer the intervention service
// needs, kept as an interface so tests can substitute a double that records
// what was delivered — and so a test can assert that NO runner method was
// called for an engine-driven run.
type hitlDeliverer interface {
	Deliver(ctx context.Context, intent engine.HITLIntent) (engine.HITLAck, error)
}

// hitlAction names the operator verb being translated. It is carried into the
// intent's fingerprint and into the journal's provenance so an engine-run
// intervention is greppable by the same verb as a runner-run one.
type hitlAction string

const (
	hitlActionApprove  hitlAction = "approve"
	hitlActionOverride hitlAction = "override"
	hitlActionRerun    hitlAction = "rerun"
	hitlActionDeny     hitlAction = "deny"
	hitlActionResume   hitlAction = "resume"
)

// terminalGeneration counts the terminals a run has produced. It is the
// engine's compare-and-set token, and it is derived here the same way the
// workflow derives it: one per run.finished event. An operator who read the
// run and quoted generation N is refused if the run has since produced N+1.
//
// This mirrors latestTerminalSequence's role on the runner side, but counts
// rather than reading a sequence number: the engine's journal sequence is
// assigned by the workflow's own writer and is not something the daemon can
// predict, whereas the number of terminals is identical on both sides of the
// projection by construction.
func terminalGeneration(events []journal.Event) uint64 {
	var generation uint64
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			generation++
		}
	}
	return generation
}

// hitlRequestID picks the idempotency key the protocol deduplicates on.
//
// An explicit Idempotency-Key is used verbatim, so a client retrying the same
// HTTP request gets the same answer without a second resolution. When none was
// supplied the payload's own fingerprint is used: an identical retry still
// deduplicates, and a genuinely different decision gets a different key rather
// than silently replaying the first one.
func hitlRequestID(action hitlAction, input httpapi.InterventionRequest) (string, error) {
	if key := strings.TrimSpace(input.IdempotencyKey); key != "" {
		return key, nil
	}
	fingerprint, err := interventionFingerprint(string(action), input)
	if err != nil {
		return "", fmt.Errorf("fingerprint operator intent: %w", err)
	}
	return fingerprint, nil
}

// hitlIntentFor translates one intervention request into a protocol intent.
func hitlIntentFor(action hitlAction, resolved resolvedInterventionRun, input httpapi.InterventionRequest) (engine.HITLIntent, error) {
	requestID, err := hitlRequestID(action, input)
	if err != nil {
		return engine.HITLIntent{}, err
	}
	intent := engine.HITLIntent{
		Protocol:                   engine.HITLProtocol,
		Version:                    engine.HITLProtocolVersion,
		RunID:                      resolved.runID,
		RequestID:                  requestID,
		Actor:                      strings.TrimSpace(input.Actor),
		ExpectedTerminalGeneration: resolved.generation,
		Rationale:                  strings.TrimSpace(input.Rationale),
	}
	switch action {
	case hitlActionApprove:
		intent.Kind = engine.HITLResolveEscalation
		intent.Resolution = engine.HITLResolutionApprove
		intent.Gate = strings.TrimSpace(input.Stage)
		intent.Decision = strings.TrimSpace(input.Decision)
	case hitlActionOverride:
		intent.Kind = engine.HITLResolveEscalation
		intent.Resolution = engine.HITLResolutionOverride
		intent.Gate = strings.TrimSpace(input.Stage)
		intent.Decision = strings.TrimSpace(input.Decision)
	case hitlActionDeny:
		intent.Kind = engine.HITLResolveEscalation
		intent.Resolution = engine.HITLResolutionDeny
		intent.Gate = strings.TrimSpace(input.Stage)
	case hitlActionRerun:
		intent.Kind = engine.HITLRerunStage
		intent.Stage = strings.TrimSpace(input.Stage)
		intent.InstructionAddendum = strings.TrimSpace(input.InstructionAddendum)
	case hitlActionResume:
		intent.Kind = engine.HITLResumeFromTerminal
		intent.Target = strings.TrimSpace(input.Stage)
		intent.Complete = intent.Target == "" || intent.Target == journal.TargetComplete
	default:
		return engine.HITLIntent{}, fmt.Errorf("unknown operator action %q", action)
	}
	return intent, nil
}

// deliverHITL is the engine-run half of every intervention verb.
//
// It never fabricates a success: the result it returns is read back off the
// run's journal AFTER the workflow's ack, so what the operator sees is what
// the run durably recorded, not what the daemon hoped would happen.
func (s *runInterventionService) deliverHITL(
	ctx context.Context,
	action hitlAction,
	resolved resolvedInterventionRun,
	input httpapi.InterventionRequest,
) (httpapi.InterventionResult, error) {
	deliverer := s.hitlDelivery()
	if deliverer == nil {
		// Unreachable through resolve, which only marks a run engine-driven
		// when a deliverer exists. Kept because the alternative to an explicit
		// refusal here is a nil dereference that looks like a daemon crash.
		return httpapi.InterventionResult{}, interventionConflict(
			"run_engine_driven",
			engineDrivenRefusal(resolved.runID, "an operator intervention").Error(),
		)
	}
	intent, err := hitlIntentFor(action, resolved, input)
	if err != nil {
		return httpapi.InterventionResult{}, interventionBadRequest("invalid_intervention", err.Error())
	}
	if intent.Actor == "" {
		return httpapi.InterventionResult{}, interventionBadRequest("actor_required", "operator identity is required")
	}
	if _, err := deliverer.Deliver(ctx, intent); err != nil {
		return httpapi.InterventionResult{}, hitlInterventionError(string(action), resolved.runID, err)
	}
	return currentInterventionResult(resolved)
}

// hitlInterventionError maps the workflow's own refusal onto the HTTP contract
// the intervention API already speaks.
//
// The refusal codes are stable protocol strings carried on the Temporal
// application error, so this maps codes rather than matching message text. An
// unrecognised code is reported as a conflict with the workflow's sentence
// intact: a future protocol version adding a refusal must degrade into a
// truthful 409, never into a 500 that hides what the run said.
func hitlInterventionError(action, runID string, err error) error {
	if errors.Is(err, engine.ErrHITLRunNotFound) {
		return httpapi.NewInterventionError(
			http.StatusNotFound, "run_not_found",
			fmt.Sprintf("no engine workflow is running for run %q", runID), err,
		)
	}
	code, message, ok := engine.HITLRefusalCode(err)
	if !ok {
		return httpapi.NewInterventionError(
			http.StatusInternalServerError, "intervention_failed",
			action+" failed while delivering the operator intent to the engine", err,
		)
	}
	if message == "" {
		message = err.Error()
	}
	switch code {
	case engine.HITLErrUnauthorized:
		return interventionForbidden("intervention_forbidden", message)
	case engine.HITLErrInvalidIntent, engine.HITLErrProtocol:
		return interventionBadRequest(hitlRequestCode(code), message)
	case engine.HITLErrKeyReused:
		return interventionConflict("idempotency_key_reused", message)
	case engine.HITLErrGeneration:
		return interventionConflict("terminal_generation_changed", message)
	case engine.HITLErrRunExecuting, engine.HITLErrRunSettled:
		return interventionConflict("run_not_intervenable", message)
	case engine.HITLErrNotResumable:
		return interventionConflict("gate_not_approvable", message)
	case engine.HITLErrRunMismatch:
		return interventionConflict("run_identity_mismatch", message)
	case engine.HITLErrNotAcceptingUp:
		return interventionConflict("run_engine_driven", message)
	default:
		return interventionConflict(code, message)
	}
}

// hitlRequestCode keeps the 400 codes distinguishable in a client's logs.
func hitlRequestCode(code string) string {
	if code == engine.HITLErrProtocol {
		return "protocol_unsupported"
	}
	return "invalid_intervention"
}
