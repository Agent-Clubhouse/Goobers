package journal

import "errors"

// TerminalCauseSchema versions the additive diagnostic record independently of
// the event envelope. The record is committed atomically with run.finished.
const TerminalCauseSchema = "goobers.dev/journal/terminal-cause/v1"

// TerminalClassification distinguishes a deliberate terminal decision from a
// policy stop or a failure of the execution infrastructure.
type TerminalClassification string

// Stable terminal classifications distinguish policy, operator and infrastructure causes.
const (
	TerminalEscalation            TerminalClassification = "escalation"
	TerminalDefinedAbort          TerminalClassification = "defined-abort"
	TerminalOperatorAbort         TerminalClassification = "operator-abort"
	TerminalDrain                 TerminalClassification = "drain"
	TerminalSignal                TerminalClassification = "signal"
	TerminalResumeRefused         TerminalClassification = "resume-refused"
	TerminalRetryExhaustion       TerminalClassification = "retry-exhaustion"
	TerminalPolicyExhaustion      TerminalClassification = "policy-exhaustion"
	TerminalInfrastructureFailure TerminalClassification = "infrastructure-failure"
	TerminalStageFailure          TerminalClassification = "stage-failure"
)

// TerminalBudget counts additional executions consumed and allowed. A nil
// budget means unavailable or not applicable, not an unlimited allowance.
type TerminalBudget struct {
	Consumed int `json:"consumed"`
	Allowed  int `json:"allowed"`
}

// TerminalCause is an immutable explanation of one terminal generation. It is
// diagnostic (excluded from cross-runner conformance), and all human text goes
// through the containing event's normal journal scrubber. CausalEventSeq is the
// event that made the decision, not a later cleanup or notification failure.
type TerminalCause struct {
	Schema         string                 `json:"schema"`
	Phase          RunPhase               `json:"phase"`
	Classification TerminalClassification `json:"classification"`
	SelectorKind   string                 `json:"selectorKind"`
	Selector       string                 `json:"selector,omitempty"`
	Branch         int                    `json:"branch,omitempty"`
	Verdict        string                 `json:"verdict,omitempty"`
	Target         string                 `json:"target,omitempty"`
	Retry          *TerminalBudget        `json:"retry,omitempty"`
	Poll           *TerminalBudget        `json:"poll,omitempty"`
	Repass         *TerminalBudget        `json:"repass,omitempty"`
	Code           string                 `json:"code"`
	Message        string                 `json:"message,omitempty"`
	// CausalEmitKey lets live writers resolve sequence numbers when side-channel
	// events interleave with a deterministic engine projection.
	CausalEmitKey  string `json:"causalEmitKey,omitempty"`
	CausalEventSeq uint64 `json:"causalEventSeq,omitempty"`
}

// ErrTerminalCauseUnavailable explicitly covers old journals, non-terminal
// generations, and terminal records with a schema this reader does not own.
var ErrTerminalCauseUnavailable = errors.New("journal: terminal cause unavailable")

// TerminalCause returns only a durably recorded cause for the current
// generation. It never fabricates a cause from an older journal's log text.
func (r *Reader) TerminalCause() (*TerminalCause, error) {
	events, err := r.Events()
	if err != nil {
		return nil, err
	}
	return TerminalCauseFromEvents(events)
}

// TerminalCauseFromEvents projects a single event snapshot, avoiding a race
// between separate phase and cause reads while a run is being resumed.
func TerminalCauseFromEvents(events []Event) (*TerminalCause, error) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if !e.KnownSchema() {
			continue
		}
		switch e.Type {
		case EventRunResumed, EventGateOverridden, EventStageRerunRequested:
			return nil, ErrTerminalCauseUnavailable
		case EventRunFinished:
			if e.TerminalCause == nil || e.TerminalCause.Schema != TerminalCauseSchema {
				return nil, ErrTerminalCauseUnavailable
			}
			copy := *e.TerminalCause
			if copy.Retry != nil {
				b := *copy.Retry
				copy.Retry = &b
			}
			if copy.Poll != nil {
				b := *copy.Poll
				copy.Poll = &b
			}
			if copy.Repass != nil {
				b := *copy.Repass
				copy.Repass = &b
			}
			return &copy, nil
		}
	}
	return nil, ErrTerminalCauseUnavailable
}

// OperatorAbortEvent records an offline operator abort. The terminal event is
// itself the causal fact; nextSeq is the sequence the exclusive run writer will
// assign to this append, after any best-effort cleanup events.
func OperatorAbortEvent(nextSeq uint64) Event {
	return Event{Type: EventRunFinished, Status: string(PhaseAborted), Disposition: RunDispositionProduced,
		TerminalCause: &TerminalCause{Schema: TerminalCauseSchema, Phase: PhaseAborted,
			Classification: TerminalOperatorAbort, SelectorKind: "condition", Code: "run_canceled",
			Message: "run aborted by operator request", CausalEventSeq: nextSeq}}
}
