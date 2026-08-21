package journal

import (
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

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
	// EventGateOverridden records an operator replacing a nondeterministic
	// gate's verdict with a configured branch, including the required rationale.
	EventGateOverridden EventType = "gate.overridden"
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
	// EventRunnerIsolationPosture records the isolation posture the runner
	// applied to a stage (#1305) — which sandbox posture was in effect and
	// how it was satisfied. Like runner.annotation its payload lives entirely
	// under Runner and it is excluded from conformance: posture is a property
	// of the runner substrate, so the same workflow definition must produce
	// identical conformance views sandboxed or not.
	EventRunnerIsolationPosture EventType = "runner.isolation.posture"
	// EventNotificationRequested records exact pre-rendered content before any
	// sink is attempted.
	EventNotificationRequested EventType = "notification.requested"
	// EventNotificationReceipt records one sink attempt or suppression result.
	EventNotificationReceipt EventType = "notification.delivery.receipt"

	// Parallel/branch lifecycle (docs/design/static-fan-out-fan-in.md §6.2).
	// All four are conformance-normative: they and the completeness record are
	// what a parallel MEANS, as distinct from the interleaving, which is a
	// scheduling artefact.

	// EventParallelStarted opens a parallel state, naming its declared branch
	// set. Recorded on the root branch.
	EventParallelStarted EventType = "parallel.started"
	// EventBranchStarted opens one branch, carrying its Branch id and name.
	EventBranchStarted EventType = "branch.started"
	// EventBranchFinished closes one branch with its terminal BranchStatus.
	EventBranchFinished EventType = "branch.finished"
	// EventParallelFinished closes a parallel with the branch completeness
	// record and the routing decision the failure policy produced. Recorded on
	// the root branch.
	EventParallelFinished EventType = "parallel.finished"

	// Instance-journal event types (§4/§6): scheduler decisions and
	// claim-ledger transitions recorded to scheduler/events.jsonl, the same
	// envelope as a run journal's events.jsonl. EventRunStarted/EventRunFinished
	// above are reused there to announce a run's start/end at the instance
	// level (with Workflow/RunID set); these are scheduler-only concepts.

	// EventInitCompleted records a successful fresh `goobers init` completion.
	EventInitCompleted EventType = "init.completed"
	// EventTriggerFired records a cron/manual trigger firing for a workflow.
	EventTriggerFired EventType = "trigger.fired"
	// EventTickSkipped records a tick that did not start a run, with Reason set
	// (e.g. "conditions: max-parallel", "conditions: budget").
	EventTickSkipped EventType = "tick.skipped"
	// EventWorkflowStarved records a workflow crossing the scheduler's
	// consecutive shared-pool skip threshold.
	EventWorkflowStarved EventType = "workflow.starved"
	// EventProviderQuotaReset records a provider budget window expiring and
	// polling admission reopening.
	EventProviderQuotaReset EventType = "provider.quota.reset"
	// EventPollShed records one provider poll omitted to preserve a constrained
	// quota window for higher-priority polling.
	EventPollShed EventType = "poll.shed"
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
	// EventDaemonUpdateDrainStarted records the stable supervisor beginning a
	// graceful drain for a validated binary handoff.
	EventDaemonUpdateDrainStarted EventType = "daemon.update.drain_started"
	// EventDaemonUpdateRestarted records the supervisor launching the staged binary.
	EventDaemonUpdateRestarted EventType = "daemon.update.restarted"
	// EventDaemonUpdateHealthy records the candidate completing its heartbeat window.
	EventDaemonUpdateHealthy EventType = "daemon.update.healthy"
	// EventDaemonUpdateRolledBack records restoration of the retained previous binary.
	EventDaemonUpdateRolledBack EventType = "daemon.update.rolled_back"
	// EventDaemonUpdateEscalated records successful creation of the rollback issue.
	EventDaemonUpdateEscalated EventType = "daemon.update.escalated"
)

// TargetComplete is the explicit journal representation of the workflow
// engine's empty-string successful terminal target.
const TargetComplete = "@complete"

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

const (
	// RunnerAnnotationRunRecovery identifies a recovered run.
	RunnerAnnotationRunRecovery = "run.recovery"
	// RunnerAnnotationTriggerRecovery identifies a recovered pending trigger.
	RunnerAnnotationTriggerRecovery = "trigger.recovery"
	// RunnerAnnotationWorkflowDigestDrift identifies in-flight runs whose
	// pinned workflow digest no longer matches the served definition (#3376):
	// they either resume from their pinned snapshot or, when that snapshot is
	// unavailable, are refused at the next daemon restart.
	RunnerAnnotationWorkflowDigestDrift = "workflow.digest.drift"
	// RecoveryActionResumed records continuation of an interrupted stage.
	RecoveryActionResumed = "resumed"
	// RecoveryActionRetried records a new attempt after interruption.
	RecoveryActionRetried = "retried"
	// RecoveryActionNewClaim records an item claimed after daemon restart.
	RecoveryActionNewClaim = "new_claim"
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
	// Seq is the monotonic per-run sequence number (from 1). The ordering key;
	// events order by (Branch, Seq) at every tier. Normative WITHIN a branch —
	// including the root branch and every run that never forks. Its absolute
	// value is EXCLUDED from conformance ACROSS distinct non-zero branches,
	// because branch interleaving is a scheduling artefact: two conformant
	// runners may interleave differently and still be equivalent (ARCHITECTURE
	// §3.3, design/static-fan-out-fan-in.md §6.2).
	Seq uint64 `json:"seq"`
	// Type is the event kind. Normative.
	Type EventType `json:"type"`
	// Branch is the parallel-branch id, normative at every tier as the primary
	// ordering key. 0 is the run's ROOT branch — every run that never forks
	// carries 0 on every event. Declared parallel branches number from 1 in
	// declaration order, so a branch id is deterministic and reproducible across
	// runs and runners.
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
	// Actor identifies the human principal that requested an intervention.
	// Normative.
	Actor string `json:"actor,omitempty"`
	// Action identifies the human intervention recorded by run.resumed.
	// Normative.
	Action string `json:"action,omitempty"`
	// Decision is the configured gate branch selected by a run.resumed
	// intervention. Normative.
	Decision string `json:"decision,omitempty"`
	// InstructionAddendum is the one-off instruction text supplied for a
	// stage.rerun.requested event. Normative.
	InstructionAddendum string `json:"instructionAddendum,omitempty"`
	// Rationale explains why an operator overrode a nondeterministic gate.
	// Normative.
	Rationale string `json:"rationale,omitempty"`
	// Gate is the gate name for gate.* events. Normative on gate.evaluated and
	// gate.overridden; gate.started and gate.paused are excluded.
	Gate string `json:"gate,omitempty"`
	// Verdict is the gate decision for gate.evaluated or gate.overridden.
	Verdict string `json:"verdict,omitempty"`
	// Target is the branch/state a gate selected or a run.resumed action chose.
	// Normative.
	Target string `json:"target,omitempty"`
	// Complete marks a run.resumed intervention that selected the workflow's
	// terminal completion branch. Normative.
	Complete bool `json:"complete,omitempty"`
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
	// Outputs. Each entry's Digest and Integrity are normative;
	// Path/Size/MediaType are not (see Ref).
	Artifacts []Ref `json:"artifacts,omitempty"`
	// Integrity is the provenance grade on an input snapshot, artifact, or
	// integrity-admission refusal. Normative.
	Integrity apiv1.Integrity `json:"integrity,omitempty"`
	// MinimumIntegrity is the refusing stage's declared threshold on an
	// integrity-admission error. Normative.
	MinimumIntegrity apiv1.Integrity `json:"minimumIntegrity,omitempty"`

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
	// NotificationRequest is the typed payload on notification.requested.
	// Notification delivery is operational output and excluded from workflow
	// conformance.
	NotificationRequest *apiv1.NotificationRequest `json:"notificationRequest,omitempty"`
	// NotificationReceipt is the typed payload on notification.delivery.receipt.
	NotificationReceipt *apiv1.NotificationReceipt `json:"notificationReceipt,omitempty"`

	// --- parallel/branch payload (§6.2) ---

	// Parallel is the name of the parallel state a branch belongs to, set on
	// parallel.started/finished and branch.started/finished. Normative.
	Parallel string `json:"parallel,omitempty"`
	// BranchName is the declared name of the branch this event concerns. The
	// numeric id lives in Branch; the name is what an author wrote and what
	// branch-qualified references use. Normative.
	BranchName string `json:"branchName,omitempty"`
	// BranchStatus is the terminal status of a branch on branch.finished.
	// Normative.
	BranchStatus BranchStatus `json:"branchStatus,omitempty"`
	// Completeness is the branch completeness record on parallel.finished: one
	// entry per DECLARED branch, in declaration order, so "did every branch
	// report?" is answerable from the journal alone. Normative — it, and not
	// the interleaving, is what a parallel means.
	Completeness []BranchOutcome `json:"completeness,omitempty"`

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

// BranchStatus is the terminal status of one parallel branch.
type BranchStatus string

const (
	// BranchSucceeded means the branch reached @join with work done.
	BranchSucceeded BranchStatus = "succeeded"
	// BranchFailed means a stage in the branch failed terminally after its
	// retry policy was exhausted.
	BranchFailed BranchStatus = "failed"
	// BranchTimedOut means the branch exceeded branchTimeoutSeconds and was
	// terminated at its next stage boundary.
	BranchTimedOut BranchStatus = "timed-out"
	// BranchCancelled means a sibling's failure (or a whole-run exit) stopped
	// this branch before it settled, under fail_fast.
	BranchCancelled BranchStatus = "cancelled"
	// BranchNoOutput means the branch settled without producing anything — a
	// branch-scoped no-work, or one whose only substantive stage carried
	// continueOnError. It is deliberately distinct from succeeded, which the
	// other four statuses could not express: a join must be able to tell "ran
	// and found nothing" from "ran and produced findings".
	BranchNoOutput BranchStatus = "no-output"
)

// BranchOutcome is one entry in a parallel's completeness record.
type BranchOutcome struct {
	// Branch is the numeric branch id (from 1; 0 is the run's root).
	Branch int `json:"branch"`
	// Name is the declared branch name.
	Name string `json:"name"`
	// Status is the branch's terminal status.
	Status BranchStatus `json:"status"`
	// Artifacts is how many artifacts the branch recorded. It lets a join
	// distinguish a branch that settled empty from one that produced work
	// without resolving every pointer.
	Artifacts int `json:"artifacts"`
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
		EventInitCompleted, EventDaemonStarted, EventDaemonCleanShutdown, EventDaemonDirtyRestart:
		// Gate markers and torn-write repair are durability/operational
		// mechanics; heartbeats are operational liveness, not orchestration
		// outcomes.
		return false
	case EventRunnerAnnotation, EventRunnerIsolationPosture:
		// Local-runner lifecycle/substrate bookkeeping lives under runner.*
		// only; isolation posture must never split the conformance surface.
		return false
	case EventSpanRecorded:
		// Spans carry live-harness transcripts (LLM output); structural only
		// per §3.3, never content-compared across runners.
		return false
	case EventNotificationRequested, EventNotificationReceipt:
		// Output transports are deployment-side effects, not deterministic
		// workflow-machine transitions.
		return false
	default:
		return true
	}
}
