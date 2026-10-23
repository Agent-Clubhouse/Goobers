package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	wf "github.com/goobers/goobers/internal/workflow"
)

const (
	// GateDecisionProtocol is the versioned human-gate decision contract.
	GateDecisionProtocol = "goobers.gate-decision.v1"
	// GateDecisionProtocolVersion is the current contract revision.
	GateDecisionProtocolVersion = 1
	// GateDecisionUpdateName is registered beside older update handlers during
	// rolling worker upgrades.
	GateDecisionUpdateName = GateDecisionProtocol
	// HumanGateWindowExpiredCode is the fail-closed terminal error produced
	// when no decision arrives within the pinned HITL window.
	HumanGateWindowExpiredCode = "human_gate_window_expired"
)

const (
	// GateDecisionErrProtocol refuses an incompatible protocol.
	GateDecisionErrProtocol = "gate_decision_protocol_unsupported"
	// GateDecisionErrRunMismatch refuses a request addressed to another run.
	GateDecisionErrRunMismatch = "gate_decision_run_mismatch"
	// GateDecisionErrInvalid refuses malformed or invalid decision content.
	GateDecisionErrInvalid = "gate_decision_invalid"
	// GateDecisionErrUnauthorized refuses an actor outside the pinned policy.
	GateDecisionErrUnauthorized = "gate_decision_unauthorized"
	// GateDecisionErrNotEnabled refuses a run without an enabled HITL policy.
	GateDecisionErrNotEnabled = "gate_decision_not_enabled"
	// GateDecisionErrNotPending refuses a run outside a human-gate hold.
	GateDecisionErrNotPending = "gate_decision_not_pending"
	// GateDecisionErrOccurrence refuses a stale or mismatched gate occurrence.
	GateDecisionErrOccurrence = "gate_decision_occurrence_changed"
	// GateDecisionErrGeneration refuses a stale terminal generation.
	GateDecisionErrGeneration = "gate_decision_terminal_generation_changed"
	// GateDecisionErrKeyReused refuses changed content under a used request ID.
	GateDecisionErrKeyReused = "gate_decision_request_id_reused"
)

var gateDecisionRefusalCodes = map[string]bool{
	GateDecisionErrProtocol:     true,
	GateDecisionErrRunMismatch:  true,
	GateDecisionErrInvalid:      true,
	GateDecisionErrUnauthorized: true,
	GateDecisionErrNotEnabled:   true,
	GateDecisionErrNotPending:   true,
	GateDecisionErrOccurrence:   true,
	GateDecisionErrGeneration:   true,
	GateDecisionErrKeyReused:    true,
}

// GateDecisionRequest resolves one committed human-gate occurrence.
type GateDecisionRequest struct {
	Protocol string `json:"protocol"`
	Version  int    `json:"version"`
	RunID    string `json:"runId"`
	// RequestID is also the Temporal UpdateID.
	RequestID string `json:"requestId"`
	Actor     string `json:"actor"`
	Gate      string `json:"gate"`
	// GateOccurrence is the committed gate.paused journal sequence.
	GateOccurrence uint64 `json:"gateOccurrence"`
	// ExpectedTerminalGeneration prevents an earlier run segment's decision
	// from landing after a terminal intervention resumed the workflow.
	ExpectedTerminalGeneration uint64 `json:"expectedTerminalGeneration"`
	Decision                   string `json:"decision"`
}

// GateDecisionAck confirms the workflow consumed a decision for the named
// occurrence. Duplicate is set only for an exact replay.
type GateDecisionAck struct {
	Protocol           string `json:"protocol"`
	Version            int    `json:"version"`
	RunID              string `json:"runId"`
	RequestID          string `json:"requestId"`
	Actor              string `json:"actor"`
	Gate               string `json:"gate"`
	GateOccurrence     uint64 `json:"gateOccurrence"`
	TerminalGeneration uint64 `json:"terminalGeneration"`
	Decision           string `json:"decision"`
	Duplicate          bool   `json:"duplicate,omitempty"`
}

// GateDecisionRefusalCode extracts a stable gate-decision refusal.
func GateDecisionRefusalCode(err error) (code, message string, ok bool) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !gateDecisionRefusalCodes[appErr.Type()] {
		return "", "", false
	}
	return appErr.Type(), appErr.Message(), true
}

type gateDecision struct {
	requestID          string
	actor              string
	gate               string
	occurrence         uint64
	terminalGeneration uint64
	decision           string
}

type gateDecisionOccurrence struct {
	gate       string
	occurrence uint64
	decision   *gateDecision
}

type gateDecisionSession struct {
	policy             *HITLPolicy
	runID              string
	m                  *wf.Machine
	terminalGeneration func() uint64

	mu workflow.Mutex

	active       *gateDecisionOccurrence
	consumed     map[string]bool
	acks         map[string]GateDecisionAck
	fingerprints map[string]string
}

func newGateDecisionSession(in RunInput, m *wf.Machine, terminalGeneration func() uint64) *gateDecisionSession {
	if terminalGeneration == nil {
		terminalGeneration = func() uint64 { return 0 }
	}
	return &gateDecisionSession{
		policy: in.HITL, runID: in.RunID, m: m, terminalGeneration: terminalGeneration,
		consumed: map[string]bool{}, acks: map[string]GateDecisionAck{}, fingerprints: map[string]string{},
	}
}

func (s *gateDecisionSession) register(ctx workflow.Context) error {
	s.mu = workflow.NewMutex(ctx)
	return workflow.SetUpdateHandlerWithOptions(
		ctx,
		GateDecisionUpdateName,
		s.handle,
		workflow.UpdateHandlerOptions{Validator: s.validate},
	)
}

// activate opens exactly one committed gate occurrence for decisions.
func (s *gateDecisionSession) activate(g apiv1.Gate, occurrence uint64) error {
	if s == nil {
		return gateDecisionRefusal(GateDecisionErrNotEnabled,
			"human-gate decisions are not configured")
	}
	if !s.policy.enabled() {
		return gateDecisionRefusal(GateDecisionErrNotEnabled,
			"run %s did not enable human-gate decisions", s.runID)
	}
	if occurrence == 0 {
		return gateDecisionRefusal(GateDecisionErrInvalid,
			"gate %q has no committed gate.paused occurrence", g.Name)
	}
	if g.Evaluator != apiv1.EvaluatorHuman {
		return gateDecisionRefusal(GateDecisionErrInvalid,
			"gate %q is not a human gate", g.Name)
	}
	if s.active != nil {
		return gateDecisionRefusal(GateDecisionErrNotPending,
			"run %s is already awaiting gate %q occurrence %d",
			s.runID, s.active.gate, s.active.occurrence)
	}
	s.active = &gateDecisionOccurrence{gate: g.Name, occurrence: occurrence}
	return nil
}

// await blocks on the pinned HITL window. The caller journals the returned
// decision through gate.evaluated or the returned expiry against occurrence.
func (s *gateDecisionSession) await(ctx workflow.Context) (gateDecision, error) {
	if s == nil {
		return gateDecision{}, gateDecisionRefusal(GateDecisionErrNotPending,
			"run is not awaiting a human-gate decision")
	}
	if s.active == nil {
		return gateDecision{}, gateDecisionRefusal(GateDecisionErrNotPending,
			"run %s is not awaiting a human-gate decision", s.runID)
	}
	active := s.active
	ok, err := workflow.AwaitWithTimeout(ctx, s.policy.wait(), func() bool {
		return active.decision != nil
	})
	if err != nil {
		return gateDecision{}, err
	}
	if !ok {
		s.active = nil
		return gateDecision{}, humanGateWindowExpired(active.gate, active.occurrence)
	}
	decision := *active.decision
	ack := GateDecisionAck{
		Protocol: GateDecisionProtocol, Version: GateDecisionProtocolVersion,
		RunID: s.runID, RequestID: decision.requestID, Actor: decision.actor,
		Gate: decision.gate, GateOccurrence: decision.occurrence,
		TerminalGeneration: decision.terminalGeneration, Decision: decision.decision,
	}
	s.acks[decision.requestID] = ack
	s.consumed[decision.requestID] = true
	s.active = nil
	return decision, nil
}

func (s *gateDecisionSession) validate(request GateDecisionRequest) error {
	if request.Protocol != GateDecisionProtocol || request.Version != GateDecisionProtocolVersion {
		return gateDecisionRefusal(GateDecisionErrProtocol,
			"unsupported gate-decision protocol %q version %d; this run speaks %s version %d",
			request.Protocol, request.Version, GateDecisionProtocol, GateDecisionProtocolVersion)
	}
	if strings.TrimSpace(request.RunID) != s.runID {
		return gateDecisionRefusal(GateDecisionErrRunMismatch,
			"decision addresses run %q but this workflow is run %q", request.RunID, s.runID)
	}
	if strings.TrimSpace(request.RequestID) == "" {
		return gateDecisionRefusal(GateDecisionErrInvalid, "requestId is required")
	}
	actor := strings.TrimSpace(request.Actor)
	if actor == "" {
		return gateDecisionRefusal(GateDecisionErrInvalid, "actor is required")
	}
	if !s.policy.authorizes(actor) {
		return gateDecisionRefusal(GateDecisionErrUnauthorized,
			"actor %q is not permitted to decide a gate on run %s", actor, s.runID)
	}
	if !s.policy.enabled() {
		return gateDecisionRefusal(GateDecisionErrNotEnabled,
			"run %s did not enable human-gate decisions", s.runID)
	}
	if request.GateOccurrence == 0 || strings.TrimSpace(request.Gate) == "" {
		return gateDecisionRefusal(GateDecisionErrInvalid,
			"gate and committed gateOccurrence are required")
	}
	if _, seen := s.fingerprints[request.RequestID]; seen {
		return nil
	}
	return s.validateCurrent(request, actor)
}

func (s *gateDecisionSession) validateCurrent(request GateDecisionRequest, actor string) error {
	if s.active == nil {
		return gateDecisionRefusal(GateDecisionErrNotPending,
			"run %s is not awaiting a human-gate decision", s.runID)
	}
	gateName := strings.TrimSpace(request.Gate)
	if gateName != s.active.gate || request.GateOccurrence != s.active.occurrence {
		return gateDecisionRefusal(GateDecisionErrOccurrence,
			"run %s is awaiting gate %q occurrence %d, not gate %q occurrence %d",
			s.runID, s.active.gate, s.active.occurrence, gateName, request.GateOccurrence)
	}
	generation := s.terminalGeneration()
	if request.ExpectedTerminalGeneration != generation {
		return gateDecisionRefusal(GateDecisionErrGeneration,
			"run %s is at terminal generation %d, not %d; re-read the run and reissue",
			s.runID, generation, request.ExpectedTerminalGeneration)
	}
	g, ok := s.m.Gate(gateName)
	if !ok || g.Evaluator != apiv1.EvaluatorHuman {
		return gateDecisionRefusal(GateDecisionErrInvalid,
			"gate %q is not a human gate in workflow %q", gateName, s.m.Def.Name)
	}
	if err := gate.ValidateHumanDecision(g, request.Decision, actor); err != nil {
		return gateDecisionRefusal(GateDecisionErrInvalid, "%v", err)
	}
	return nil
}

func (s *gateDecisionSession) handle(ctx workflow.Context, request GateDecisionRequest) (GateDecisionAck, error) {
	fingerprint := gateDecisionFingerprint(request)
	if ack, err, done := s.replay(request.RequestID, fingerprint); done {
		return ack, err
	}
	if err := s.mu.Lock(ctx); err != nil {
		return GateDecisionAck{}, err
	}
	defer s.mu.Unlock()
	if ack, err, done := s.replay(request.RequestID, fingerprint); done {
		return ack, err
	}
	actor := strings.TrimSpace(request.Actor)
	if err := s.validateCurrent(request, actor); err != nil {
		return GateDecisionAck{}, err
	}
	s.fingerprints[request.RequestID] = fingerprint
	s.active.decision = &gateDecision{
		requestID: request.RequestID, actor: actor, gate: strings.TrimSpace(request.Gate),
		occurrence: request.GateOccurrence, terminalGeneration: request.ExpectedTerminalGeneration,
		decision: request.Decision,
	}
	if err := workflow.Await(ctx, func() bool { return s.consumed[request.RequestID] }); err != nil {
		s.active.decision = nil
		delete(s.fingerprints, request.RequestID)
		return GateDecisionAck{}, err
	}
	return s.acks[request.RequestID], nil
}

func (s *gateDecisionSession) replay(requestID, fingerprint string) (GateDecisionAck, error, bool) {
	prior, seen := s.fingerprints[requestID]
	if !seen {
		return GateDecisionAck{}, nil, false
	}
	if prior != fingerprint {
		return GateDecisionAck{}, gateDecisionRefusal(GateDecisionErrKeyReused,
			"requestId %q was already used for a different gate decision on run %s",
			requestID, s.runID), true
	}
	ack, ok := s.acks[requestID]
	if !ok {
		return GateDecisionAck{}, nil, false
	}
	ack.Duplicate = true
	return ack, nil, true
}

func gateDecisionFingerprint(request GateDecisionRequest) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"actor=" + strings.TrimSpace(request.Actor),
		"gate=" + strings.TrimSpace(request.Gate),
		"occurrence=" + strconv.FormatUint(request.GateOccurrence, 10),
		"generation=" + strconv.FormatUint(request.ExpectedTerminalGeneration, 10),
		"decision=" + request.Decision,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func gateDecisionRefusal(code, format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), code, nil)
}

func humanGateWindowExpired(gateName string, occurrence uint64) error {
	return temporal.NewNonRetryableApplicationError(
		fmt.Sprintf("human gate %q occurrence %d received no decision before the HITL window expired", gateName, occurrence),
		HumanGateWindowExpiredCode,
		nil,
	)
}
