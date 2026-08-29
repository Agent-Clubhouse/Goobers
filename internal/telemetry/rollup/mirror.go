// Package rollup projects a run's journal (events.jsonl + run.yaml + the
// telemetry span exporter's spans/spans.jsonl) into a queryable local SQLite
// store — TEL-032, issue #22. The rollup is derived state: always rebuildable
// from the journals, never their source of truth.
package rollup

import "time"

// The types below mirror the portions of internal/journal's on-disk JSON shape
// that rollup consumes (with the same json tags) WITHOUT importing
// internal/journal, keeping the production reader decoupled from the writer.
//
// journal_roundtrip_test.go reflectively pins journalEvent's JSON field set to
// journal.Event plus an explicit set of intentionally unmirrored fields.
// fixture_test.go also hand-writes representative journal records inline,
// while the round-trip tests exercise records written by the real package.

// journalEvent mirrors the fields rollup consumes from internal/journal.Event.
// Workflow/RunID/Reason are only populated on instance-journal events
// (scheduler/events.jsonl) — a run's own events.jsonl never sets them, since a
// run event's identity is implicit from its directory.
type journalEvent struct {
	Schema            string              `json:"schema"`
	Seq               uint64              `json:"seq"`
	Type              string              `json:"type"`
	Branch            int                 `json:"branch"`
	Time              time.Time           `json:"time"`
	Stage             string              `json:"stage,omitempty"`
	Attempt           int                 `json:"attempt,omitempty"`
	AttemptClass      string              `json:"attemptClass,omitempty"`
	Gate              string              `json:"gate,omitempty"`
	Verdict           string              `json:"verdict,omitempty"`
	Target            string              `json:"target,omitempty"`
	Escalated         bool                `json:"escalated,omitempty"`
	Status            string              `json:"status,omitempty"`
	Outputs           map[string]any      `json:"outputs,omitempty"`
	Artifacts         []journalRef        `json:"artifacts,omitempty"`
	Actor             string              `json:"actor,omitempty"`
	WorkflowVersion   int                 `json:"workflowVersion,omitempty"`
	WorkflowDigest    string              `json:"workflowDigest,omitempty"`
	Ref               *journalRef         `json:"ref,omitempty"`
	Name              string              `json:"name,omitempty"`
	DataSchema        string              `json:"dataSchema,omitempty"`
	ExternalRef       *journalExternalRef `json:"externalRef,omitempty"`
	Error             *journalErrorDetail `json:"error,omitempty"`
	Redaction         *journalRedaction   `json:"redaction,omitempty"`
	Runner            map[string]any      `json:"runner,omitempty"`
	Workflow          string              `json:"workflow,omitempty"`
	RunID             string              `json:"runId,omitempty"`
	Reason            string              `json:"reason,omitempty"`
	SourceRunID       string              `json:"sourceRunId,omitempty"`
	SourceTerminalSeq uint64              `json:"sourceTerminalSeq,omitempty"`
}

// Event type values accepted from run and instance journals.
const (
	eventStageStarted       = "stage.started"
	eventStageFinished      = "stage.finished"
	eventGateEvaluated      = "gate.evaluated"
	eventRefTouched         = "ref.touched"
	eventError              = "error"
	eventRunStarted         = "run.started"
	eventRunResumed         = "run.resumed"
	eventRunFinished        = "run.finished"
	eventSpanRecorded       = "span.recorded"
	eventInitCompleted      = "init.completed"
	eventTriggerFired       = "trigger.fired"
	eventTickSkipped        = "tick.skipped"
	eventProviderQuotaReset = "provider.quota.reset"
	eventPollShed           = "poll.shed"
	eventClaimAcquired      = "claim.acquired"
	eventClaimReleased      = "claim.released"
	eventClaimForceReleased = "claim.force_released"
)

const eventWorkflowStarved = "workflow.starved"

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

const (
	// eventSchema and runSchema mirror the payload schemas this package's
	// independent projection types understand.
	eventSchema = "goobers.dev/journal/event/v1"
	runSchema   = "goobers.dev/journal/run/v1"
)

// runIdentity mirrors internal/journal.RunIdentity (run.yaml). journal decodes
// YAML via json-tagged structs (sigs.k8s.io/yaml, already a repo dependency),
// so this mirror decodes with the same library against the same tags.
type runIdentity struct {
	Schema          string            `json:"schema"`
	RunID           string            `json:"runId"`
	Workflow        string            `json:"workflow"`
	WorkflowVersion int               `json:"workflowVersion"`
	WorkflowDigest  string            `json:"workflowDigest,omitempty"`
	GooberDigest    string            `json:"gooberDigest,omitempty"`
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
