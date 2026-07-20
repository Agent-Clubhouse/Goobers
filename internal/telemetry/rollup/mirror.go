// Package rollup projects a run's journal (events.jsonl + run.yaml + the
// telemetry span exporter's spans/spans.jsonl) into a queryable local SQLite
// store — TEL-032, issue #22. The rollup is derived state: always rebuildable
// from the journals, never their source of truth.
package rollup

import "time"

// The types below mirror internal/journal's on-disk JSON shape field-for-field
// (same json tags) WITHOUT importing internal/journal. #8 (the journal package,
// PR #56) is still unmerged as of this package's authoring; importing it would
// give this package a hard build-order dependency on an unmerged PR, which the
// mission brief explicitly asked to avoid (same decoupling playbook as #12's
// provider seams: providers.ExternalRef/MutationRecorder mirror the journal's
// "external ref touched" concept without importing it either).
//
// The mirror is pinned field-for-field against internal/journal/event.go,
// identity.go, and ref.go as read from PR #56 at authoring time; a schema_test
// fixture (testdata/fixture-run/events.jsonl) encodes a real event of every
// type by hand so a future field-name drift fails a test, not a silent no-op.
// Once #8 merges, a follow-up can additionally round-trip through the real
// journal.Event/RunIdentity types for a belt-and-suspenders check.

// journalEvent mirrors internal/journal.Event. Workflow/RunID/Reason are only
// populated on instance-journal events (scheduler/events.jsonl) — a run's own
// events.jsonl never sets them, since a run event's identity is implicit from
// its directory (internal/journal/event.go's own doc comment on those three
// fields).
type journalEvent struct {
	Schema       string              `json:"schema"`
	Seq          uint64              `json:"seq"`
	Type         string              `json:"type"`
	Branch       int                 `json:"branch"`
	Time         time.Time           `json:"time"`
	Stage        string              `json:"stage,omitempty"`
	Attempt      int                 `json:"attempt,omitempty"`
	AttemptClass string              `json:"attemptClass,omitempty"`
	Gate         string              `json:"gate,omitempty"`
	Verdict      string              `json:"verdict,omitempty"`
	Target       string              `json:"target,omitempty"`
	Status       string              `json:"status,omitempty"`
	Ref          *journalRef         `json:"ref,omitempty"`
	Name         string              `json:"name,omitempty"`
	ExternalRef  *journalExternalRef `json:"externalRef,omitempty"`
	Error        *journalErrorDetail `json:"error,omitempty"`
	Redaction    *journalRedaction   `json:"redaction,omitempty"`
	Runner       map[string]any      `json:"runner,omitempty"`
	Workflow     string              `json:"workflow,omitempty"`
	RunID        string              `json:"runId,omitempty"`
	Reason       string              `json:"reason,omitempty"`
}

// Event type values, mirroring internal/journal's EventType constants.
const (
	eventStageStarted    = "stage.started"
	eventStageFinished   = "stage.finished"
	eventGateEvaluated   = "gate.evaluated"
	eventRefTouched      = "ref.touched"
	eventError           = "error"
	eventRunStarted      = "run.started"
	eventRunFinished     = "run.finished"
	eventSpanRecorded    = "span.recorded"
	eventTriggerFired    = "trigger.fired"
	eventTickSkipped     = "tick.skipped"
	eventWorkflowStarved = "workflow.starved"
	eventClaimAcquired   = "claim.acquired"
	eventClaimReleased   = "claim.released"
)

// journalRef mirrors internal/journal.Ref.
type journalRef struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType,omitempty"`
}

// journalExternalRef mirrors internal/journal.ExternalRef.
type journalExternalRef struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	URL      string `json:"url,omitempty"`
}

// journalErrorDetail mirrors internal/journal.ErrorDetail.
type journalErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// journalRedaction mirrors internal/journal.RedactionInfo.
type journalRedaction struct {
	Target    string `json:"target"`
	OldDigest string `json:"oldDigest"`
	NewDigest string `json:"newDigest"`
	Reason    string `json:"reason,omitempty"`
}

// runIdentity mirrors internal/journal.RunIdentity (run.yaml). journal decodes
// YAML via json-tagged structs (sigs.k8s.io/yaml, already a repo dependency),
// so this mirror decodes with the same library against the same tags.
type runIdentity struct {
	Schema          string            `json:"schema"`
	RunID           string            `json:"runId"`
	Workflow        string            `json:"workflow"`
	WorkflowVersion int               `json:"workflowVersion"`
	WorkflowDigest  string            `json:"workflowDigest,omitempty"`
	Gaggle          string            `json:"gaggle"`
	Trigger         journalTrigger    `json:"trigger"`
	Inputs          []journalInputRef `json:"inputs,omitempty"`
	StartedAt       time.Time         `json:"startedAt"`
}

// journalTrigger mirrors internal/journal.Trigger.
type journalTrigger struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
}

// journalInputRef mirrors internal/journal.InputRef.
type journalInputRef struct {
	Name string     `json:"name"`
	Ref  journalRef `json:"ref"`
}
