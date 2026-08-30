package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// hitl.go is decision 005 R8: the versioned Temporal protocol through which an
// operator resolves an escalation, reruns a stage, or resumes from a terminal
// on an ENGINE-DRIVEN run (#3883).
//
// # Why an Update and not a Signal
//
// A Signal is fire-and-forget: the client learns the server accepted the
// signal, never that the workflow accepted the INTENT. Every one of the three
// operator intents can be legitimately refused by the run — the gate has no
// such branch, the stage is deterministic, the run is still executing, the
// terminal generation moved under the operator — and a fire-and-forget
// delivery would have the daemon answer 202 to an intent the workflow is
// about to drop on the floor. That is precisely the "acknowledge success
// before durable workflow acceptance" failure this protocol must not have.
//
// A workflow Update carries a return value and an error back to the caller,
// and its acceptance and outcome are both written to workflow history before
// the caller's handle.Get returns. The daemon therefore reports success only
// after the run has durably accepted the intent, and reports the workflow's
// own refusal verbatim otherwise. The schedule reconciler already establishes
// this idiom in-tree (schedule.go's scheduleReconcileUpdate).
//
// # Rejection vs failure
//
// Malformed, unauthorized, mis-addressed and wrong-phase intents are refused
// by the update VALIDATOR, which by Temporal's contract leaves no trace in
// history: a spray of unauthorized attempts cannot grow a run's history, and
// a rejected intent cannot perturb replay. Only an intent the validator
// admitted reaches the handler, and only the handler mutates workflow state.
//
// # Containment
//
// Every intent names the run it is for. A workflow refuses an intent addressed
// to another run even if Temporal routed it here, so a workflow-id collision
// or a daemon bug can never apply an operator's verdict to the wrong run.
//
// # Replay determinism
//
// The handler and the validator are pure functions of pinned RunInput, the
// compiled machine, and workflow state that is itself derived from history.
// The wait is workflow.AwaitWithTimeout over a pinned duration (a Temporal
// timer, not a wall-clock read). A run whose input does not enable HITL never
// registers a wait and never appends an event, so every history recorded
// before this file existed replays byte for byte.

const (
	// HITLProtocol names the wire contract. It is carried in every intent and
	// checked by the validator: a client that speaks a different protocol is
	// refused rather than silently reinterpreted.
	HITLProtocol = "goobers.hitl.v1"
	// HITLProtocolVersion is the protocol's revision within HITLProtocol.
	// Additive fields keep the version; any change to the meaning of an
	// existing field mints a new HITLProtocol name (and a new update name),
	// because a rolling worker fleet always has both versions live.
	HITLProtocolVersion = 1
	// HITLUpdateName is the Temporal update name the Run workflow handles.
	// Versioned in the name itself so a v2 protocol can be registered
	// alongside v1 during a rollout instead of replacing it.
	HITLUpdateName = "goobers.hitl-intent.v1"
	// HITLStateQuery reports whether a run is currently accepting operator
	// intents, and under which terminal generation. The daemon uses it to
	// render "awaiting operator" without guessing from the journal.
	HITLStateQuery = "goobers.hitl-state.v1"
)

// HITLIntentKind is the operator intent being delivered. The three kinds are
// the three in-process runner paths #3847 refuses for an engine-driven run.
type HITLIntentKind string

const (
	// HITLResolveEscalation resolves an escalation with a verdict, mirroring
	// the daemon's approve/override/deny intervention verbs over
	// Runner.ResumeFromTerminal.
	HITLResolveEscalation HITLIntentKind = "resolve-escalation"
	// HITLRerunStage reruns one agentic stage with an instruction addendum,
	// mirroring Runner.RerunStage.
	HITLRerunStage HITLIntentKind = "rerun-stage"
	// HITLResumeFromTerminal resumes the walk at an explicit workflow state
	// (or completes it), mirroring Runner.ResumeFromTerminal's raw form.
	HITLResumeFromTerminal HITLIntentKind = "resume-from-terminal"
)

// HITL resolutions for HITLResolveEscalation, matching the daemon's
// intervention verbs one for one (cmd/goobers/interventions.go).
const (
	// HITLResolutionApprove clears the escalation on the gate's own branch.
	// Permitted for human and agentic gates.
	HITLResolutionApprove = "approve"
	// HITLResolutionOverride clears it against the evaluator's verdict, and
	// therefore requires a rationale.
	HITLResolutionOverride = "override"
	// HITLResolutionDeny records that the escalation was reviewed and the run
	// deliberately stays terminal. It resumes nothing.
	HITLResolutionDeny = "deny"
)

// HITL refusal codes. They are stable strings the daemon maps onto HTTP
// statuses, so an operator sees the same code whichever driver refused.
const (
	HITLErrProtocol       = "hitl_protocol_unsupported"
	HITLErrRunMismatch    = "hitl_run_mismatch"
	HITLErrUnauthorized   = "hitl_unauthorized"
	HITLErrInvalidIntent  = "hitl_invalid_intent"
	HITLErrRunExecuting   = "hitl_run_executing"
	HITLErrRunSettled     = "hitl_run_settled"
	HITLErrGeneration     = "hitl_terminal_generation_changed"
	HITLErrNotResumable   = "hitl_not_resumable"
	HITLErrKeyReused      = "hitl_request_id_reused"
	HITLErrNotAcceptingUp = "hitl_not_enabled"
)

// HITLPolicy is the run's pinned HITL posture. It is part of RunInput — pinned
// at start like the definition itself (WF-016) — rather than worker
// configuration, because the wait it authorizes is a workflow timer and a
// timer whose duration came from mutable config is a replay-nondeterminism
// bug. A nil policy (every run started before this protocol existed, and every
// lane that declares no human gate) disables the whole mechanism: no handler
// wait, no extra journal event, no behavioural change whatsoever.
type HITLPolicy struct {
	// Enabled turns the terminal-hold on. False behaves exactly as the engine
	// did before #3883: an escalated or failed run settles immediately.
	Enabled bool `json:"enabled,omitempty"`
	// WaitSeconds bounds how long a resumable terminal is held open for an
	// operator. Zero means DefaultHITLWaitSeconds. The bound is mandatory:
	// an unbounded hold turns every escalation into a workflow that never
	// closes, retains history forever, and holds the scheduler's concurrency
	// slot for the life of the instance.
	WaitSeconds int `json:"waitSeconds,omitempty"`
	// Actors is the closed set of caller identities permitted to deliver an
	// intent to this run. Empty means "any actor the daemon authenticated",
	// which is the posture the in-process runner has today (it validates the
	// actor is non-empty and lets gate policy decide the rest). A non-empty
	// set is enforced IN THE WORKFLOW, so a compromised or buggy daemon
	// cannot resolve a run the operator was never entitled to.
	Actors []string `json:"actors,omitempty"`
}

// DefaultHITLWaitSeconds is the terminal hold applied when a policy enables
// HITL without naming a window: one working day, long enough for a real
// operator rotation and short enough that a forgotten escalation settles
// rather than pinning a slot indefinitely.
const DefaultHITLWaitSeconds = 24 * 60 * 60

func (p *HITLPolicy) enabled() bool { return p != nil && p.Enabled }

func (p *HITLPolicy) wait() time.Duration {
	if p == nil || p.WaitSeconds <= 0 {
		return time.Duration(DefaultHITLWaitSeconds) * time.Second
	}
	return time.Duration(p.WaitSeconds) * time.Second
}

func (p *HITLPolicy) authorizes(actor string) bool {
	if p == nil || len(p.Actors) == 0 {
		return true
	}
	for _, a := range p.Actors {
		if strings.EqualFold(strings.TrimSpace(a), actor) {
			return true
		}
	}
	return false
}

// HITLIntent is one operator intent delivered to a run. It is the update's
// single argument, so the whole contract is one serializable struct.
type HITLIntent struct {
	// Protocol and Version identify the contract. Both are checked before
	// anything else: a mismatch is refused, never coerced.
	Protocol string `json:"protocol"`
	Version  int    `json:"version"`
	// Kind selects the intent.
	Kind HITLIntentKind `json:"kind"`
	// RunID is the run this intent is for — the containment check. It must
	// equal the workflow's own pinned RunID.
	RunID string `json:"runId"`
	// RequestID is the idempotency key. The daemon also uses it as the
	// Temporal UpdateID, so the server deduplicates a retried delivery before
	// it ever reaches the workflow; the workflow deduplicates again against
	// its own record so a key reused with a DIFFERENT payload is refused
	// rather than silently applied (the same rule the runner-side
	// escalation-resolution marker enforces).
	RequestID string `json:"requestId"`
	// Actor is the caller identity. Required, journaled, and matched against
	// the pinned HITLPolicy.Actors when one is declared.
	Actor string `json:"actor"`
	// ExpectedTerminalGeneration is the compare-and-set guard, the engine's
	// analogue of the runner's ExpectedTerminalSeq. It is the number of
	// run.finished events the run had produced when the operator read it. A
	// mismatch means the terminal the operator was looking at is not the one
	// they would be resolving, which is exactly the case
	// ErrTerminalGenerationChanged exists to refuse.
	ExpectedTerminalGeneration uint64 `json:"expectedTerminalGeneration"`

	// Gate, Resolution, Decision and Rationale carry HITLResolveEscalation.
	Gate       string `json:"gate,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Decision   string `json:"decision,omitempty"`
	Rationale  string `json:"rationale,omitempty"`

	// Stage and InstructionAddendum carry HITLRerunStage.
	Stage               string `json:"stage,omitempty"`
	InstructionAddendum string `json:"instructionAddendum,omitempty"`

	// Target and Complete carry HITLResumeFromTerminal. They are mutually
	// exclusive and exactly one is required, the same rule
	// ResumeFromTerminalInput enforces.
	Target   string `json:"target,omitempty"`
	Complete bool   `json:"complete,omitempty"`
}

// HITLAck is what the workflow returns once it has durably accepted an intent.
// The daemon reports success only on receiving one.
type HITLAck struct {
	Protocol string `json:"protocol"`
	Version  int    `json:"version"`
	RunID    string `json:"runId"`
	// RequestID echoes the intent's key so a caller correlating out-of-order
	// replies cannot mistake one ack for another.
	RequestID string         `json:"requestId"`
	Kind      HITLIntentKind `json:"kind"`
	// Resumed is false for a deny (and only for a deny): the intent was
	// accepted and journaled, and the run stays terminal.
	Resumed bool `json:"resumed"`
	// ResumeState is the workflow state the walk re-entered, or "@complete"
	// when the intent completed the run.
	ResumeState string `json:"resumeState,omitempty"`
	// Attempt is the human attempt number journaled on a rerun request.
	Attempt int `json:"attempt,omitempty"`
	// TerminalGeneration is the generation the intent was applied against.
	TerminalGeneration uint64 `json:"terminalGeneration"`
	// Duplicate is true when this ack replays an earlier, identical intent
	// rather than describing a new acceptance.
	Duplicate bool `json:"duplicate,omitempty"`
}

// HITLState is the HITLStateQuery answer.
type HITLState struct {
	Protocol string `json:"protocol"`
	Version  int    `json:"version"`
	Enabled  bool   `json:"enabled"`
	// Phase is "executing", "awaiting-operator", or "settled".
	Phase string `json:"phase"`
	// TerminalGeneration is the number of terminals the run has produced. An
	// operator reads it here (or counts run.finished in the journal, which is
	// the same number by construction) and echoes it back as
	// ExpectedTerminalGeneration.
	TerminalGeneration uint64 `json:"terminalGeneration"`
	// TerminalStatus/TerminalState describe the terminal currently held open.
	TerminalStatus string `json:"terminalStatus,omitempty"`
	TerminalState  string `json:"terminalState,omitempty"`
	// DeadlineUnix is the workflow-deterministic instant the hold expires.
	DeadlineUnix int64 `json:"deadlineUnix,omitempty"`
}

// HITL phases.
const (
	hitlPhaseExecuting = "executing"
	hitlPhaseAwaiting  = "awaiting-operator"
	hitlPhaseSettled   = "settled"
)

// hitlRefusal is a refusal the operator should see verbatim. It is returned as
// a Temporal ApplicationError with a stable type so the daemon can map the
// code onto an HTTP status without string matching.
func hitlRefusal(code, format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), code, nil)
}

// HITLRefusalCode extracts the stable refusal code from an error a HITL update
// returned, or "" when the error is not a protocol refusal.
func HITLRefusalCode(err error) (code, message string, ok bool) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return "", "", false
	}
	if !hitlRefusalCodes[appErr.Type()] {
		// Some other application error surfaced through the same update — a
		// journal write that failed, say. It is not a considered refusal and
		// must not be reported to an operator as one.
		return "", "", false
	}
	return appErr.Type(), appErr.Message(), true
}

// hitlRefusalCodes is the closed set of codes this protocol version refuses
// with. Membership is what separates "the run said no, and here is why" from
// "something broke", which are different HTTP statuses and different operator
// instructions.
var hitlRefusalCodes = map[string]bool{
	HITLErrProtocol:       true,
	HITLErrRunMismatch:    true,
	HITLErrUnauthorized:   true,
	HITLErrInvalidIntent:  true,
	HITLErrRunExecuting:   true,
	HITLErrRunSettled:     true,
	HITLErrGeneration:     true,
	HITLErrNotResumable:   true,
	HITLErrKeyReused:      true,
	HITLErrNotAcceptingUp: true,
}

// hitlResumePlan is the walk-visible result of an accepted intent.
type hitlResumePlan struct {
	requestID string
	kind      HITLIntentKind
	// state is the workflow state to re-enter, or wf.TerminalComplete.
	state string
	// stage/addendum are set for a rerun so walk can hand the re-dispatched
	// stage its instruction addendum through the same `addenda` channel the
	// #3374 re-dispatch uses.
	stage    string
	addendum string
	attempt  int
}

// hitlSession is the workflow-state half of the protocol. Every field is
// derived from history: pinned input, the compiled machine, and the walk's own
// deterministic progress.
type hitlSession struct {
	policy *HITLPolicy
	runID  string
	m      *wf.Machine
	rec    *runJournal

	mu workflow.Mutex

	phase string
	// generation counts terminals this run has produced. It is incremented
	// when a terminal is journaled, which is exactly when the journal gains a
	// run.finished event — so an operator who counted run.finished events on
	// disk and the workflow are looking at the same number.
	generation uint64
	// terminal describes the terminal currently held open.
	terminal RunResult
	deadline time.Time

	// pending is an accepted intent the walk has not consumed yet, and
	// consumed records which request ids the walk has taken up. Both are
	// plain workflow state.
	pending  *hitlResumePlan
	consumed map[string]bool
	// acks records every accepted intent's outcome, keyed by request id, for
	// in-workflow deduplication.
	acks map[string]HITLAck
	// fingerprints pins the payload each request id was first seen with, so a
	// key reused for a DIFFERENT intent is refused instead of replaying the
	// first one's ack.
	fingerprints map[string]string

	// wroteTerminal reports that settle journaled the run's FINAL terminal,
	// so run() does not write a second one for the same outcome.
	wroteTerminal bool
}

func newHITLSession(in RunInput, m *wf.Machine, rec *runJournal) *hitlSession {
	return &hitlSession{
		policy:       in.HITL,
		runID:        in.RunID,
		m:            m,
		rec:          rec,
		phase:        hitlPhaseExecuting,
		consumed:     map[string]bool{},
		acks:         map[string]HITLAck{},
		fingerprints: map[string]string{},
	}
}

// register binds the update and query handlers. It is called unconditionally —
// even for a run whose policy disables HITL — so that an intent addressed to a
// run that cannot accept one is REFUSED with a named reason rather than
// buffered by Temporal against a handler that will never exist. Neither
// registration emits a history event, so this is invisible to replay of
// histories recorded before the protocol existed.
func (s *hitlSession) register(ctx workflow.Context) error {
	s.mu = workflow.NewMutex(ctx)
	err := workflow.SetUpdateHandlerWithOptions(
		ctx, HITLUpdateName, s.handle,
		workflow.UpdateHandlerOptions{Validator: s.validate},
	)
	if err != nil {
		return fmt.Errorf("engine: register HITL update handler: %w", err)
	}
	if err := workflow.SetQueryHandler(ctx, HITLStateQuery, s.state); err != nil {
		return fmt.Errorf("engine: register HITL state query: %w", err)
	}
	return nil
}

func (s *hitlSession) state() (HITLState, error) {
	st := HITLState{
		Protocol:           HITLProtocol,
		Version:            HITLProtocolVersion,
		Enabled:            s.policy.enabled(),
		Phase:              s.phase,
		TerminalGeneration: s.generation,
	}
	if s.phase == hitlPhaseAwaiting {
		st.TerminalStatus = s.terminal.Status
		st.TerminalState = s.terminal.FinalState
		st.DeadlineUnix = s.deadline.Unix()
	}
	return st, nil
}

// validate is the update validator. Temporal guarantees it runs before the
// handler and that its rejection leaves no history behind, so it carries every
// check that can be made against state the workflow already holds: protocol,
// containment, authorization, well-formedness, and phase.
//
// It must not mutate workflow state, and it does not.
func (s *hitlSession) validate(intent HITLIntent) error {
	if intent.Protocol != HITLProtocol || intent.Version != HITLProtocolVersion {
		return hitlRefusal(HITLErrProtocol,
			"unsupported HITL protocol %q version %d; this run speaks %s version %d",
			intent.Protocol, intent.Version, HITLProtocol, HITLProtocolVersion)
	}
	if strings.TrimSpace(intent.RunID) != s.runID {
		return hitlRefusal(HITLErrRunMismatch,
			"intent addresses run %q but this workflow is run %q", intent.RunID, s.runID)
	}
	if strings.TrimSpace(intent.RequestID) == "" {
		return hitlRefusal(HITLErrInvalidIntent, "requestId is required")
	}
	actor := strings.TrimSpace(intent.Actor)
	if actor == "" {
		return hitlRefusal(HITLErrInvalidIntent, "actor is required")
	}
	if !s.policy.authorizes(actor) {
		return hitlRefusal(HITLErrUnauthorized,
			"actor %q is not permitted to intervene on run %s", actor, s.runID)
	}
	// The not-enabled refusal is answered before well-formedness: a run that
	// never opted into the protocol has no opinion about the shape of an
	// intent it will never take up, and the operator needs to be told the run
	// is not listening rather than sent to fix a payload that cannot land.
	if !s.policy.enabled() {
		return hitlRefusal(HITLErrNotAcceptingUp,
			"run %s did not enable the HITL protocol; its escalations settle without an operator hold", s.runID)
	}
	if err := validateHITLShape(intent); err != nil {
		return err
	}
	// An intent whose request id this run has already seen is admitted so the
	// handler can answer it idempotently — replaying the first delivery's ack,
	// or refusing a key reused for a different payload. Everything else must
	// be in the accepting phase.
	if _, seen := s.fingerprints[intent.RequestID]; seen {
		return nil
	}
	return s.acceptingNow()
}

// acceptingNow is the phase gate, stated explicitly in both directions.
//
// An intent for a run that is still EXECUTING is refused rather than queued.
// Queueing it would mean holding an operator's verdict against a gate that has
// not been evaluated yet, a stage rerun for an attempt still in flight, or a
// resume for a terminal the run has not reached — three ways to apply a
// decision to a state the operator never saw. The refusal names the phase so
// the daemon can tell the operator to wait rather than retry blindly, and the
// operator re-reads the run and re-issues against the terminal generation they
// actually observed.
func (s *hitlSession) acceptingNow() error {
	switch s.phase {
	case hitlPhaseAwaiting:
		return nil
	case hitlPhaseExecuting:
		return hitlRefusal(HITLErrRunExecuting,
			"run %s is executing; operator intents are accepted only while it holds a resumable terminal", s.runID)
	default:
		return hitlRefusal(HITLErrRunSettled,
			"run %s has settled; its terminal is closed to operator intents", s.runID)
	}
}

func validateHITLShape(intent HITLIntent) error {
	switch intent.Kind {
	case HITLResolveEscalation:
		// A deny names no gate: it resumes nothing, so there is no branch to
		// resolve. It is journaled as a resolution against the run's terminal,
		// exactly as the in-process deny is. approve and override DO name one,
		// because the target they resume to comes from that gate's branches.
		if intent.Resolution != HITLResolutionDeny && strings.TrimSpace(intent.Gate) == "" {
			return hitlRefusal(HITLErrInvalidIntent, "gate is required to resolve an escalation")
		}
		switch intent.Resolution {
		case HITLResolutionApprove:
		case HITLResolutionOverride, HITLResolutionDeny:
			// Both are decisions taken against what the run concluded, so both
			// owe the operator's reason to the journal.
			if strings.TrimSpace(intent.Rationale) == "" {
				return hitlRefusal(HITLErrInvalidIntent, "%s rationale is required", intent.Resolution)
			}
		default:
			return hitlRefusal(HITLErrInvalidIntent,
				"resolution %q is not one of %s/%s/%s",
				intent.Resolution, HITLResolutionApprove, HITLResolutionOverride, HITLResolutionDeny)
		}
	case HITLRerunStage:
		if strings.TrimSpace(intent.Stage) == "" {
			return hitlRefusal(HITLErrInvalidIntent, "stage is required to rerun a stage")
		}
		if strings.TrimSpace(intent.InstructionAddendum) == "" {
			return hitlRefusal(HITLErrInvalidIntent, "instruction addendum is required to rerun a stage")
		}
	case HITLResumeFromTerminal:
		target := strings.TrimSpace(intent.Target)
		if target == "" && !intent.Complete {
			return hitlRefusal(HITLErrInvalidIntent, "resume requires a target state or complete")
		}
		if target != "" && target != journal.TargetComplete && intent.Complete {
			return hitlRefusal(HITLErrInvalidIntent, "resume target and complete are mutually exclusive")
		}
	default:
		return hitlRefusal(HITLErrInvalidIntent, "unknown intent kind %q", intent.Kind)
	}
	if intent.ExpectedTerminalGeneration == 0 {
		return hitlRefusal(HITLErrInvalidIntent, "expectedTerminalGeneration is required")
	}
	return nil
}

// handle is the update handler: the only place workflow state is mutated by an
// operator intent. It runs on its own coroutine, serialized against other
// intents by a workflow mutex, and it does not return until the walk has
// actually taken the intent up — so the ack the daemon reports success on
// means "this run has resumed", not "this run was told to".
func (s *hitlSession) handle(ctx workflow.Context, intent HITLIntent) (HITLAck, error) {
	fingerprint := hitlFingerprint(intent)
	if ack, err, decided := s.replay(intent.RequestID, fingerprint); decided {
		return ack, err
	}
	if err := s.mu.Lock(ctx); err != nil {
		return HITLAck{}, err
	}
	defer s.mu.Unlock()
	// Re-check under the lock: the validator ran against a snapshot taken
	// before this coroutine held the lock, and a concurrent intent may have
	// resumed the run in between. Without this, two operators racing on one
	// escalated run would both be told they resumed it. An in-flight duplicate
	// of THIS request blocked on the same lock and finds its ack here.
	if ack, err, decided := s.replay(intent.RequestID, fingerprint); decided {
		return ack, err
	}
	if err := s.acceptingNow(); err != nil {
		return HITLAck{}, err
	}
	if intent.ExpectedTerminalGeneration != s.generation {
		return HITLAck{}, hitlRefusal(HITLErrGeneration,
			"run %s is at terminal generation %d, not %d; re-read the run and reissue",
			s.runID, s.generation, intent.ExpectedTerminalGeneration)
	}

	plan, err := s.plan(intent)
	if err != nil {
		return HITLAck{}, err
	}
	s.fingerprints[intent.RequestID] = fingerprint

	if plan == nil {
		// A deny: journaled, and the run stays exactly as terminal as it was.
		s.recordDenied(ctx, intent)
		ack := HITLAck{
			Protocol: HITLProtocol, Version: HITLProtocolVersion,
			RunID: s.runID, RequestID: intent.RequestID, Kind: intent.Kind,
			Resumed: false, TerminalGeneration: s.generation,
		}
		s.acks[intent.RequestID] = ack
		return ack, nil
	}

	s.recordResumed(ctx, intent, *plan)
	s.pending = plan
	// Block until the walk has consumed the plan. The walk is parked in
	// AwaitWithTimeout on exactly this condition, so it wakes within this same
	// workflow task; if the run is cancelled while we wait, Await returns the
	// cancellation and the operator is told the intent did not land.
	if err := workflow.Await(ctx, func() bool { return s.consumed[intent.RequestID] }); err != nil {
		s.pending = nil
		delete(s.fingerprints, intent.RequestID)
		return HITLAck{}, err
	}
	return s.acks[intent.RequestID], nil
}

// replay answers a request id that has been seen before. decided=false means
// the id is new and the caller must go on to accept it. A key reused with a
// different payload is refused rather than replaying the first intent's ack —
// the same rule the runner-side escalation-resolution marker enforces.
func (s *hitlSession) replay(requestID, fingerprint string) (HITLAck, error, bool) {
	prior, seen := s.fingerprints[requestID]
	if !seen {
		return HITLAck{}, nil, false
	}
	if prior != fingerprint {
		return HITLAck{}, hitlRefusal(HITLErrKeyReused,
			"requestId %q was already used for a different intent on run %s", requestID, s.runID), true
	}
	ack, ok := s.acks[requestID]
	if !ok {
		// Seen but not yet acked: the first delivery is still in flight, and
		// it holds the lock. Fall through so this coroutine blocks on it and
		// finds the ack on the far side.
		return HITLAck{}, nil, false
	}
	ack.Duplicate = true
	return ack, nil, true
}

// plan resolves an admitted intent into the walk's next move. A nil plan with
// a nil error is a deny: accepted, journaled, resumes nothing.
func (s *hitlSession) plan(intent HITLIntent) (*hitlResumePlan, error) {
	switch intent.Kind {
	case HITLResolveEscalation:
		return s.planResolve(intent)
	case HITLRerunStage:
		return s.planRerun(intent)
	case HITLResumeFromTerminal:
		return s.planResume(intent)
	default:
		return nil, hitlRefusal(HITLErrInvalidIntent, "unknown intent kind %q", intent.Kind)
	}
}

func (s *hitlSession) planResolve(intent HITLIntent) (*hitlResumePlan, error) {
	if intent.Resolution == HITLResolutionDeny {
		return nil, nil
	}
	gateName := strings.TrimSpace(intent.Gate)
	g, ok := s.m.Gate(gateName)
	if !ok {
		return nil, hitlRefusal(HITLErrInvalidIntent,
			"stage %q is not a gate in workflow %q", gateName, s.m.Def.Name)
	}
	// The runner's rule, verbatim: a deterministic gate's verdict is a
	// computation, not a judgement, so there is nothing for an operator to
	// approve or override.
	if g.Evaluator == apiv1.EvaluatorAutomated {
		return nil, hitlRefusal(HITLErrNotResumable,
			"gate %q is deterministic and cannot be %sd by an operator", gateName, intent.Resolution)
	}
	if !s.evaluatedInSegment(gateName) {
		return nil, hitlRefusal(HITLErrNotResumable,
			"gate %q was not evaluated in the current run segment", gateName)
	}
	decision := strings.TrimSpace(intent.Decision)
	if decision == "" {
		decision = string(gateDefaultDecision)
	}
	target, ok := wf.BranchTarget(g, decision)
	if !ok {
		return nil, hitlRefusal(HITLErrNotResumable,
			"gate %q has no %q decision branch", gateName, decision)
	}
	state, err := s.resumableState(target, "gate "+gateName+" decision "+decision)
	if err != nil {
		return nil, err
	}
	return &hitlResumePlan{requestID: intent.RequestID, kind: intent.Kind, state: state}, nil
}

// gateDefaultDecision is the decision an approve carries when the operator
// names none — the same "pass" default the daemon's approve verb applies.
const gateDefaultDecision = "pass"

func (s *hitlSession) planRerun(intent HITLIntent) (*hitlResumePlan, error) {
	stage := strings.TrimSpace(intent.Stage)
	// A rerun re-enters a stage to do work again, which only means anything
	// for an escalated run: a run that FAILED has an unresolved stage failure
	// whose resolution is a resume decision, not another attempt at the same
	// stage. The runner draws the line in the same place (RerunStage requires
	// PhaseEscalated; ResumeFromTerminal accepts escalated or failed).
	if s.terminal.Status != StatusEscalated {
		return nil, hitlRefusal(HITLErrNotResumable,
			"run %s is %s; only an escalated run can rerun a stage", s.runID, s.terminal.Status)
	}
	if task, ok := s.m.Task(stage); ok {
		if task.Type != apiv1.TaskAgentic {
			return nil, hitlRefusal(HITLErrNotResumable,
				"stage %q is deterministic; instruction addenda require an agentic stage", stage)
		}
	} else if g, ok := s.m.Gate(stage); ok {
		if g.Evaluator != apiv1.EvaluatorAgentic {
			return nil, hitlRefusal(HITLErrNotResumable,
				"stage %q is not an agentic reviewer gate", stage)
		}
	} else {
		return nil, hitlRefusal(HITLErrInvalidIntent,
			"stage %q is not defined by workflow %q", stage, s.m.Def.Name)
	}
	starts := s.startsOf(stage)
	if starts == 0 {
		return nil, hitlRefusal(HITLErrNotResumable, "stage %q has not previously run", stage)
	}
	return &hitlResumePlan{
		requestID: intent.RequestID,
		kind:      intent.Kind,
		state:     stage,
		stage:     stage,
		addendum:  strings.TrimSpace(intent.InstructionAddendum),
		// nextRerunAttempt's arithmetic: one past every start this stage has
		// already had.
		attempt: starts + 1,
	}, nil
}

func (s *hitlSession) planResume(intent HITLIntent) (*hitlResumePlan, error) {
	if intent.Complete || strings.TrimSpace(intent.Target) == journal.TargetComplete {
		return &hitlResumePlan{requestID: intent.RequestID, kind: intent.Kind, state: wf.TerminalComplete}, nil
	}
	target := strings.TrimSpace(intent.Target)
	state, err := s.resumableState(target, "resume target")
	if err != nil {
		return nil, err
	}
	return &hitlResumePlan{requestID: intent.RequestID, kind: intent.Kind, state: state}, nil
}

// resumableState maps a branch target onto a state the walk can re-enter.
//
// @complete is the one reserved target a resume may land on: it is a real
// terminal the walk knows how to reach. Every other reserved target
// (@escalate, @abort, @join) either re-terminalizes the run at the terminal
// the operator is trying to leave or needs parallel-branch context the walk no
// longer holds, so it is refused by name rather than silently reinterpreted.
func (s *hitlSession) resumableState(target, what string) (string, error) {
	if target == wf.TerminalComplete {
		return wf.TerminalComplete, nil
	}
	if wf.IsReservedAnyTarget(target) || !s.m.Has(target) {
		return "", hitlRefusal(HITLErrNotResumable,
			"%s does not continue at a resumable workflow state (%q)", what, target)
	}
	return target, nil
}

// recordResumed journals the acceptance. The event is the runner's own
// run.resumed / stage.rerun.requested — same type, same fields, same actor and
// rationale — so the projected journal of an engine run an operator resolved
// is indistinguishable from a local run's on the conformance surface.
func (s *hitlSession) recordResumed(ctx workflow.Context, intent HITLIntent, plan hitlResumePlan) {
	if plan.kind == HITLRerunStage {
		s.rec.append(ctx, journal.Event{
			Type:                journal.EventStageRerunRequested,
			Stage:               plan.stage,
			Attempt:             plan.attempt,
			AttemptClass:        journal.AttemptHuman,
			Actor:               strings.TrimSpace(intent.Actor),
			InstructionAddendum: plan.addendum,
			Runner:              hitlProvenance(intent),
		})
		return
	}
	action := "resume"
	if intent.Kind == HITLResolveEscalation {
		action = intent.Resolution
	}
	ev := journal.Event{
		Type:            journal.EventRunResumed,
		Status:          s.terminal.Status,
		Target:          plan.state,
		Complete:        plan.state == wf.TerminalComplete,
		Actor:           strings.TrimSpace(intent.Actor),
		Action:          action,
		Gate:            strings.TrimSpace(intent.Gate),
		Decision:        strings.TrimSpace(intent.Decision),
		Rationale:       strings.TrimSpace(intent.Rationale),
		WorkflowVersion: s.rec.proj.Identity.WorkflowVersion,
		WorkflowDigest:  s.rec.proj.Identity.WorkflowDigest,
		Runner:          hitlProvenance(intent),
	}
	if ev.Complete {
		ev.Target = ""
	}
	s.rec.append(ctx, ev)
}

// recordDenied journals a deny exactly as the daemon's in-process path does:
// an escalation.resolution annotation, carrying the actor, rationale and the
// idempotency key, on a run that keeps its terminal phase.
func (s *hitlSession) recordDenied(ctx workflow.Context, intent HITLIntent) {
	provenance := hitlProvenance(intent)
	provenance["kind"] = HITLEscalationResolutionMarker
	provenance["resolution"] = HITLResolutionDeny
	provenance["rationale"] = strings.TrimSpace(intent.Rationale)
	provenance["gate"] = strings.TrimSpace(intent.Gate)
	s.rec.append(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Runner: provenance})
}

// HITLEscalationResolutionMarker is the runner.annotation kind a denied
// escalation is recorded under. It is deliberately the same string the
// daemon's in-process deny path uses, so one grep finds both drivers'
// resolutions.
const HITLEscalationResolutionMarker = "escalation.resolution"

func hitlProvenance(intent HITLIntent) map[string]any {
	return map[string]any{
		"protocol":       HITLProtocol,
		"version":        HITLProtocolVersion,
		"intent":         string(intent.Kind),
		"idempotencyKey": intent.RequestID,
		"actor":          strings.TrimSpace(intent.Actor),
	}
}

// hitlFingerprint is the payload identity a request id is pinned to. Built
// from the trimmed decision fields only — never from transport metadata — so
// a genuine retry of the same intent fingerprints identically and a different
// intent under a reused key does not.
func hitlFingerprint(intent HITLIntent) string {
	fields := []string{
		"kind=" + string(intent.Kind),
		"actor=" + strings.TrimSpace(intent.Actor),
		"gate=" + strings.TrimSpace(intent.Gate),
		"resolution=" + intent.Resolution,
		"decision=" + strings.TrimSpace(intent.Decision),
		"rationale=" + strings.TrimSpace(intent.Rationale),
		"stage=" + strings.TrimSpace(intent.Stage),
		"addendum=" + strings.TrimSpace(intent.InstructionAddendum),
		"target=" + strings.TrimSpace(intent.Target),
		fmt.Sprintf("complete=%t", intent.Complete),
		fmt.Sprintf("generation=%d", intent.ExpectedTerminalGeneration),
	}
	sort.Strings(fields)
	return strings.Join(fields, "\x00")
}

// startsOf counts every stage.started / gate.started this run has journaled
// for one stage. It is nextRerunAttempt's arithmetic (internal/runner's
// rerun.go) read off the workflow's own projection rather than a re-read
// journal: same event stream, same count, and deterministic under replay
// because the projection is itself derived from history.
func (s *hitlSession) startsOf(stage string) int {
	starts := 0
	for _, op := range s.rec.proj.Ops {
		if op.Kind != opAppend || op.Event == nil {
			continue
		}
		switch op.Event.Type {
		case journal.EventStageStarted:
			if op.Event.Stage == stage {
				starts++
			}
		case journal.EventGateStarted:
			if op.Event.Gate == stage {
				starts++
			}
		}
	}
	return starts
}

// evaluatedInSegment reports whether a gate produced a verdict in the CURRENT
// run segment — since the last operator resume, or since the run started if
// there has not been one. It is gateEvaluatedInCurrentSegment's rule
// (cmd/goobers/interventions.go): an operator may only resolve a gate whose
// verdict belongs to the escalation in front of them, never one from a
// segment an earlier intervention already closed.
func (s *hitlSession) evaluatedInSegment(gate string) bool {
	ops := s.rec.proj.Ops
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i]
		if op.Kind != opAppend || op.Event == nil {
			continue
		}
		switch op.Event.Type {
		case journal.EventRunResumed, journal.EventStageRerunRequested:
			return false
		case journal.EventGateEvaluated:
			if op.Event.Gate == gate {
				return true
			}
		}
	}
	return false
}

// hitlResumable reports whether a terminal is one an operator may act on. It
// is the runner's rule: escalated and failed runs are intervenable; a
// completed or aborted run is finished.
func hitlResumable(status string) bool {
	return status == StatusEscalated || status == StatusFailed
}

// settle is the walk's terminal hook.
//
// For a run with no HITL policy it is a pure no-op and returns immediately —
// that is what keeps every pre-#3883 history replaying byte for byte. For an
// enabled run reaching a resumable terminal it journals the terminal FIRST
// (so the escalation is visible to the operator who is about to resolve it,
// and so the terminal generation the operator quotes back is a number that
// exists on disk), then holds the run open for the pinned window.
//
// It returns resumed=true with the state the walk should re-enter. journaled
// reports that this hook already wrote the run's terminal, so run() does not
// write a second one for the same outcome.
func (s *hitlSession) settle(ctx workflow.Context, out RunResult) (plan hitlResumePlan, resumed bool, journaled bool, err error) {
	if s == nil || !s.policy.enabled() || !hitlResumable(out.Status) {
		return hitlResumePlan{}, false, false, nil
	}
	s.terminal = out
	s.recordTerminal(ctx, out)
	s.wroteTerminal = true
	s.phase = hitlPhaseAwaiting
	s.deadline = workflow.Now(ctx).Add(s.policy.wait())

	ok, awaitErr := workflow.AwaitWithTimeout(ctx, s.policy.wait(), func() bool { return s.pending != nil })
	if awaitErr != nil {
		// Cancellation while holding a terminal open. The run's terminal is
		// already journaled; run()'s cancellation arm records the abort cause
		// on top of it, which is the honest sequence — the run escalated, an
		// operator was given a window, and the window was cut short.
		s.phase = hitlPhaseSettled
		return hitlResumePlan{}, false, true, awaitErr
	}
	if !ok || s.pending == nil {
		// The window expired with no operator. The terminal already written
		// stands; nothing further is journaled, so a HITL-enabled run nobody
		// resolved ends with exactly the journal a HITL-disabled one would.
		s.phase = hitlPhaseSettled
		return hitlResumePlan{}, false, true, nil
	}

	accepted := *s.pending
	s.pending = nil
	s.phase = hitlPhaseExecuting
	// The run has left its terminal: the next one it reaches is a different
	// outcome, and run() owns writing it unless this hook holds that one open
	// too.
	s.wroteTerminal = false
	s.acks[accepted.requestID] = HITLAck{
		Protocol: HITLProtocol, Version: HITLProtocolVersion,
		RunID: s.runID, RequestID: accepted.requestID, Kind: accepted.kind,
		Resumed: true, ResumeState: accepted.state, Attempt: accepted.attempt,
		TerminalGeneration: s.generation,
	}
	s.consumed[accepted.requestID] = true
	return accepted, true, true, nil
}

// recordTerminal writes the run's terminal into the projection and bumps the
// generation. It is the same pair of writes run() makes, lifted here so a
// terminal that is about to be held open is journaled before the hold rather
// than after it.
func (s *hitlSession) recordTerminal(ctx workflow.Context, out RunResult) {
	if out.Status == StatusFailed {
		s.rec.runFailedCause(ctx, out.FinalState, out.FailureCode, out.FailureMessage)
	}
	phase, err := PhaseForStatus(out.Status)
	if err != nil {
		// hitlResumable already restricted this to escalated/failed, both of
		// which map. Defensive only.
		phase = journal.PhaseFailed
	}
	s.rec.runFinished(ctx, phase)
	s.generation++
	s.rec.emitTerminal(ctx)
}

// noteTerminal records a terminal run() wrote itself, so the generation the
// protocol reports stays equal to the number of run.finished events the
// journal holds even for the terminals this session did not write.
func (s *hitlSession) noteTerminal() {
	if s == nil {
		return
	}
	s.generation++
	s.phase = hitlPhaseSettled
}
