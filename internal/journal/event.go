package journal

import "time"

// EventType is the kind of an orchestration event. The taxonomy is the
// conformance surface (§3.3): the runner, telemetry, portal, and conformance
// harness all switch on it. Values are dotted and versioned with the envelope.
type EventType string

// The event taxonomy (issue #8). Every run's journal is a sequence of these.
const (
	// EventRunStarted opens a run; carries the pinned identity echoed from run.yaml.
	EventRunStarted EventType = "run.started"
	// EventRunFinished closes a run with a terminal status.
	EventRunFinished EventType = "run.finished"
	// EventStageStarted marks a stage attempt beginning.
	EventStageStarted EventType = "stage.started"
	// EventStageFinished marks a stage attempt ending with a result.
	EventStageFinished EventType = "stage.finished"
	// EventGateEvaluated records a gate verdict and the branch it selected.
	EventGateEvaluated EventType = "gate.evaluated"
	// EventArtifactRecorded records an artifact committed by content digest.
	EventArtifactRecorded EventType = "artifact.recorded"
	// EventInputSnapshot records an immutable input snapshot by content digest.
	EventInputSnapshot EventType = "input.snapshot"
	// EventRefTouched records an external reference touched (issue/PR).
	EventRefTouched EventType = "ref.touched"
	// EventError records an error surfaced during the run.
	EventError EventType = "error"
	// EventRedaction records the sanctioned secret-remediation repair
	// (old→new digests), the one edit the append-only rule allows.
	EventRedaction EventType = "redaction"
	// EventRepaired records crash-recovery repair of a torn final write.
	EventRepaired EventType = "repaired"
)

// AttemptClass tags why a stage attempt exists. Only policy attempts are
// conformance-normative; infra attempts (an infrastructure failure retried by
// the runner) are excluded from the conformance set (§3.3). The initial attempt
// carries no class and is always included.
type AttemptClass string

const (
	// AttemptPolicy is a retry driven by the stage's declared retry policy.
	AttemptPolicy AttemptClass = "policy"
	// AttemptInfra is a retry driven by infrastructure failure. Excluded.
	AttemptInfra AttemptClass = "infra"
)

// Event is the versioned journal envelope: one JSON object per line in
// events.jsonl. It is deliberately flat and omitempty-heavy so `cat`/`jq`/`grep`
// are first-class debugging tools (§4).
//
// Conformance classification (§3.3) is attached to each field below. The
// conformance set is computed by ConformanceView; anything not listed there is
// excluded. The runner.* namespace is the ONLY sanctioned runner-specific
// divergence and is always excluded.
type Event struct {
	// Schema is the envelope version. Normative (readers branch on it).
	Schema string `json:"schema"`
	// Seq is the monotonic per-run sequence number (from 1). Normative — the
	// ordering key; at tier 3, events order by (Branch, Seq).
	Seq uint64 `json:"seq"`
	// Type is the event kind. Normative.
	Type EventType `json:"type"`
	// Branch is the parallel-branch id. 0 at tiers 1–2; reserved for tier-3
	// parallel branches. Normative (secondary ordering key).
	Branch int `json:"branch"`
	// Time is when the event was recorded. EXCLUDED from conformance.
	Time time.Time `json:"time"`

	// --- orchestration payload (normative unless noted) ---

	// Stage is the stage name for stage.* events. Normative.
	Stage string `json:"stage,omitempty"`
	// Attempt is the 1-based attempt number for stage.* events. Normative.
	Attempt int `json:"attempt,omitempty"`
	// AttemptClass tags a retry attempt. Normative iff not "infra".
	AttemptClass AttemptClass `json:"attemptClass,omitempty"`
	// Gate is the gate name for gate.evaluated. Normative.
	Gate string `json:"gate,omitempty"`
	// Verdict is the gate decision for gate.evaluated. Normative.
	Verdict string `json:"verdict,omitempty"`
	// Target is the branch/state the gate selected. Normative.
	Target string `json:"target,omitempty"`
	// Status is the terminal status for run.finished / stage.finished. Normative.
	Status string `json:"status,omitempty"`

	// Ref points at in-journal content (artifact.recorded, input.snapshot). Its
	// Digest is normative; Path/Size are not (see Ref).
	Ref *Ref `json:"ref,omitempty"`
	// Name labels the Ref (artifact/input name). Normative.
	Name string `json:"name,omitempty"`
	// ExternalRef identifies an external reference touched. Normative — by
	// (Provider, Kind, ID), not by URL.
	ExternalRef *ExternalRef `json:"externalRef,omitempty"`
	// Error carries failure detail for error events. Its Code is normative; the
	// human Message is not compared.
	Error *ErrorDetail `json:"error,omitempty"`
	// Redaction records an old→new digest remediation. Normative.
	Redaction *RedactionInfo `json:"redaction,omitempty"`

	// Runner holds runner-specific annotations. The ONLY sanctioned
	// runner-specific divergence and ALWAYS EXCLUDED from conformance.
	Runner map[string]any `json:"runner,omitempty"`
}

// ExternalRef identifies an external reference the run touched — an issue or PR
// in a provider. The normative identity is (Provider, Kind, ID); URL is a
// convenience for humans and is not compared across runners.
type ExternalRef struct {
	Provider string `json:"provider"`      // e.g. "github"
	Kind     string `json:"kind"`          // e.g. "issue", "pr"
	ID       string `json:"id"`            // e.g. "123"
	URL      string `json:"url,omitempty"` // not normative
}

// ErrorDetail is the failure detail on an error event. Code is a stable,
// machine-readable classifier (normative); Message is human-facing (excluded).
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// RedactionInfo records a sanctioned secret remediation: the leaked blob at
// OldDigest was replaced by scrubbed content at NewDigest.
type RedactionInfo struct {
	Target    string `json:"target"` // Ref.Path of the remediated blob
	OldDigest string `json:"oldDigest"`
	NewDigest string `json:"newDigest"`
	Reason    string `json:"reason,omitempty"`
}

// IsConformanceNormative reports whether this event participates in the
// cross-runner conformance set (§3.3). Excluded: infra-retry attempts, and the
// recovery/repair bookkeeping events that are local-runner mechanics.
func (e Event) IsConformanceNormative() bool {
	if e.AttemptClass == AttemptInfra {
		return false
	}
	switch e.Type {
	case EventRepaired:
		// Torn-write repair is a durability mechanic, not orchestration.
		return false
	default:
		return true
	}
}
