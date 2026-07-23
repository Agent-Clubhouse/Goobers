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
	// EventRunResumed records an explicit human intervention that reopens an
	// escalated or failed run at a chosen workflow state.
	EventRunResumed EventType = "run.resumed"
	// EventRunFinished closes a run with a terminal status.
	EventRunFinished EventType = "run.finished"
	// EventStageStarted marks a stage attempt beginning.
	EventStageStarted EventType = "stage.started"
	// EventStageHeartbeat records observable progress from an active stage
	// attempt. It is lightweight operational telemetry and excluded from
	// conformance.
	EventStageHeartbeat EventType = "stage.heartbeat"
	// EventStageFinished marks a stage attempt ending with a result.
	EventStageFinished EventType = "stage.finished"
	// EventStageRerunRequested records an operator-requested rerun of one
	// agentic task or gate with a one-off instruction addendum.
	EventStageRerunRequested EventType = "stage.rerun.requested"
	// EventGateStarted marks a gate evaluation beginning. It is recovery
	// bookkeeping, excluded from cross-runner conformance.
	EventGateStarted EventType = "gate.started"
	// EventGatePaused marks a run waiting at a gate for its verdict. It is
	// operational state, excluded from cross-runner conformance.
	EventGatePaused EventType = "gate.paused"
	// EventGateEvaluated records a gate verdict and the branch it selected.
	EventGateEvaluated EventType = "gate.evaluated"
	// EventArtifactRecorded records an artifact committed by content digest.
	EventArtifactRecorded EventType = "artifact.recorded"
	// EventSpanRecorded records a within-stage trace span (harness transcript,
	// tool events) committed by content digest under spans/ (GBO-020).
	EventSpanRecorded EventType = "span.recorded"
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
	// EventRunnerAnnotation records local-runner lifecycle bookkeeping. Its
	// payload lives entirely under Runner and is excluded from conformance.
	EventRunnerAnnotation EventType = "runner.annotation"

	// Instance-journal event types (§4/§6): scheduler decisions and
	// claim-ledger transitions recorded to scheduler/events.jsonl, the same
	// envelope as a run journal's events.jsonl. EventRunStarted/EventRunFinished
	// above are reused there to announce a run's start/end at the instance
	// level (with Workflow/RunID set); these are scheduler-only concepts.

	// EventTriggerFired records a cron/manual trigger firing for a workflow.
	EventTriggerFired EventType = "trigger.fired"
	// EventTickSkipped records a tick that did not start a run, with Reason set
	// (e.g. "conditions: max-parallel", "conditions: budget").
	EventTickSkipped EventType = "tick.skipped"
	// EventWorkflowStarved records a workflow crossing the scheduler's
	// consecutive shared-pool skip threshold.
	EventWorkflowStarved EventType = "workflow.starved"
	// EventClaimAcquired records the claim ledger granting a lease.
	EventClaimAcquired EventType = "claim.acquired"
	// EventClaimReleased records a lease release (run finished, expired, or
	// crash-recovered).
	EventClaimReleased EventType = "claim.released"
	// EventClaimForceReleased records an operator overriding a claim lease.
	EventClaimForceReleased EventType = "claim.force_released"
	// EventClaimLockSlow records claims-lock contention above the local runner's
	// diagnostic threshold. Timing, operation, and process details live under
	// Runner because they are runner-specific and excluded from conformance.
	EventClaimLockSlow EventType = "claim_lock_slow"
	// EventClaimLockTimeout records a bounded claims-lock acquisition expiring.
	// Error.Code carries claims_lock_timeout; retry classification and timing
	// details live under Runner.
	EventClaimLockTimeout EventType = "claims_lock_timeout"
	// EventConfigReloaded records an atomically-applied config directory change.
	EventConfigReloaded EventType = "config.reloaded"
	// EventConfigReloadRejected records a changed config directory that failed
	// validation and was not applied.
	EventConfigReloadRejected EventType = "config.reload.rejected"
	// EventDaemonStarted records a daemon lifetime beginning after it acquires
	// the instance lock.
	EventDaemonStarted EventType = "daemon.started"
	// EventDaemonCleanShutdown records a graceful drain completing before the
	// daemon releases its instance lock.
	EventDaemonCleanShutdown EventType = "daemon.clean_shutdown"
	// EventDaemonDirtyRestart records startup finding a previous daemon lock
	// without a subsequent clean-shutdown event.
	EventDaemonDirtyRestart EventType = "daemon.dirty_restart"
)

// AttemptClass tags why a non-initial stage attempt exists. Policy and human
// attempts are conformance-normative; infra attempts (an infrastructure
// failure retried by the runner) are excluded from the conformance set
// (§3.3). The initial attempt carries no class and is always included.
type AttemptClass string

const (
	// AttemptPolicy is a retry driven by the stage's declared retry policy.
	AttemptPolicy AttemptClass = "policy"
	// AttemptInfra is a retry driven by infrastructure failure. Excluded.
	AttemptInfra AttemptClass = "infra"
	// AttemptHuman is an explicit operator-requested rerun. Normative.
	AttemptHuman AttemptClass = "human"
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

	// Stage is the stage name for stage.* events and stage-scoped artifacts.
	// Normative except on stage.heartbeat, which is excluded as a whole.
	Stage string `json:"stage,omitempty"`
	// Attempt is the 1-based attempt number for stage.* events and
	// stage-scoped artifacts. Normative except on stage.heartbeat.
	Attempt int `json:"attempt,omitempty"`
	// AttemptClass tags why a non-initial attempt exists. Normative iff the
	// event is not a heartbeat and the class is not "infra".
	AttemptClass AttemptClass `json:"attemptClass,omitempty"`
	// Actor identifies the human principal that requested an intervention —
	// a stage.rerun.requested or a run.resumed action. Normative.
	Actor string `json:"actor,omitempty"`
	// InstructionAddendum is the one-off instruction text supplied for a
	// stage.rerun.requested event. Normative.
	InstructionAddendum string `json:"instructionAddendum,omitempty"`
	// Gate is the gate name for gate.* events. Normative on gate.evaluated;
	// gate.started and gate.paused are excluded as operational state.
	Gate string `json:"gate,omitempty"`
	// Verdict is the gate decision for gate.evaluated. Normative.
	Verdict string `json:"verdict,omitempty"`
	// Target is the branch/state a gate selected or a run.resumed action chose.
	// Normative.
	Target string `json:"target,omitempty"`
	// Escalated reports that gate evaluation selected its escalation control
	// branch. Normative.
	Escalated bool `json:"escalated,omitempty"`
	// Status is the terminal status for run.finished / stage.finished, or the
	// prior terminal phase for run.resumed. Normative.
	Status string `json:"status,omitempty"`
	// WorkflowVersion is the immutable workflow version re-asserted by a
	// run.resumed action. Normative.
	WorkflowVersion int `json:"workflowVersion,omitempty"`
	// WorkflowDigest is the immutable workflow digest re-asserted by a
	// run.resumed action. Normative.
	WorkflowDigest string `json:"workflowDigest,omitempty"`
	// Outputs mirrors a stage.finished ResultEnvelope's small, scalar-only
	// Outputs (docs/stage-contract.md) — journaled so a resumed run can
	// reconstruct a finished stage's result without it (walk's lastStage/
	// lastResult, or a gate's subject) being lost to an in-memory-only value
	// a crash wipes. Normative.
	Outputs map[string]any `json:"outputs,omitempty"`
	// Artifacts mirrors a stage.finished ResultEnvelope's Artifacts — the
	// pointers this attempt produced — for the same reconstruction reason as
	// Outputs. Each entry's Digest is normative; Path/Size/MediaType are not
	// (see Ref).
	Artifacts []Ref `json:"artifacts,omitempty"`

	// Ref points at in-journal content (artifact.recorded, input.snapshot). Its
	// Digest is normative except for runner-assembled context manifests, whose
	// bytes include non-normative pointer metadata; Path/Size are not (see Ref).
	Ref *Ref `json:"ref,omitempty"`
	// Name labels the Ref (artifact/input name). Normative.
	Name string `json:"name,omitempty"`
	// DataSchema identifies the record shape of schema-aware span content.
	// EXCLUDED from conformance because span.recorded is excluded as a whole.
	DataSchema string `json:"dataSchema,omitempty"`
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

	// --- instance-journal payload (scheduler/events.jsonl only; not used in a
	// run's own events.jsonl, since a run event's identity is implicit from its
	// directory) ---

	// Workflow is the workflow name for a scheduler decision (trigger.fired,
	// tick.skipped, or an instance-level run.started/run.finished echo).
	Workflow string `json:"workflow,omitempty"`
	// Gaggle scopes an instance-journal workflow name.
	Gaggle string `json:"gaggle,omitempty"`
	// RunID is the run a scheduler decision or claim transition pertains to.
	RunID string `json:"runId,omitempty"`
	// Reason is a short, stable explanation for an instance-level scheduler or
	// daemon lifecycle event.
	Reason string `json:"reason,omitempty"`
	// SkipCount is the consecutive shared-pool refusal count for a
	// workflow.starved event.
	SkipCount int `json:"skipCount,omitempty"`
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
// cross-runner conformance set (§3.3). Excluded: heartbeats, infra-retry
// attempts, and recovery/repair bookkeeping events that are local-runner
// mechanics.
func (e Event) IsConformanceNormative() bool {
	if e.AttemptClass == AttemptInfra {
		return false
	}
	switch e.Type {
	case EventStageHeartbeat, EventGateStarted, EventGatePaused, EventRepaired,
		EventDaemonStarted, EventDaemonCleanShutdown, EventDaemonDirtyRestart:
		// Gate markers and torn-write repair are durability/operational
		// mechanics; heartbeats are operational liveness, not orchestration
		// outcomes.
		return false
	case EventRunnerAnnotation:
		// Local-runner lifecycle bookkeeping lives under runner.* only.
		return false
	case EventSpanRecorded:
		// Spans carry live-harness transcripts (LLM output); structural only
		// per §3.3, never content-compared across runners.
		return false
	default:
		return true
	}
}
