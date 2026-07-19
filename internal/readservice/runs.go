package readservice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

const (
	defaultRunLimit = 50
	maxRunLimit     = 200
)

var (
	// ErrNotFound means the requested read-model object does not exist.
	ErrNotFound = errors.New("read service: not found")
	// ErrInvalidArgument means a path, filter, or cursor is malformed.
	ErrInvalidArgument = errors.New("read service: invalid argument")
	// ErrArtifactIntegrity means journal content failed containment or digest checks.
	ErrArtifactIntegrity = errors.New("read service: artifact integrity check failed")
)

// RunPhase keeps HTTP adapters on the shared read contract.
type RunPhase = journal.RunPhase

// TriggerKind keeps HTTP adapters on the shared read contract.
type TriggerKind = journal.TriggerKind

// RunListOptions controls deterministic run filtering and keyset pagination.
type RunListOptions struct {
	Gaggle   string
	Workflow string
	Phase    journal.RunPhase
	Trigger  journal.TriggerKind
	Limit    int
	Cursor   string
}

// RunList is one deterministic page of run summaries.
type RunList struct {
	Runs       []RunSummary `json:"runs"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// RunSummary is the journal-derived diagnostic summary shared by run lists and
// run detail.
type RunSummary struct {
	ID               string           `json:"id"`
	Workflow         string           `json:"workflow"`
	WorkflowVersion  int              `json:"workflowVersion"`
	WorkflowDigest   string           `json:"workflowDigest,omitempty"`
	Gaggle           string           `json:"gaggle"`
	Trigger          journal.Trigger  `json:"trigger"`
	Phase            journal.RunPhase `json:"phase"`
	Terminal         bool             `json:"terminal"`
	CurrentStage     string           `json:"currentStage,omitempty"`
	StartedAt        time.Time        `json:"startedAt"`
	FinishedAt       *time.Time       `json:"finishedAt,omitempty"`
	DurationMillis   int64            `json:"durationMillis"`
	LastSeq          uint64           `json:"lastSeq"`
	RepassCount      int              `json:"repassCount"`
	RetryCount       int              `json:"retryCount"`
	PolicyRetryCount int              `json:"policyRetryCount"`
	InfraRetryCount  int              `json:"infraRetryCount"`
}

// RunDetail includes the immutable graph pin and structured escalation cause.
// GraphStatus is "pinned" for current runs and "unavailable" for journals that
// predate graph snapshots.
type RunDetail struct {
	RunSummary
	Graph       *workflow.Graph  `json:"graph,omitempty"`
	GraphStatus string           `json:"graphStatus"`
	Escalation  *EscalationCause `json:"escalation,omitempty"`
}

// EscalationCause projects the durable event that selected escalation.
type EscalationCause struct {
	Selector       EscalationSelector `json:"selector"`
	SelectedBranch string             `json:"selectedBranch,omitempty"`
	RepassCount    int                `json:"repassCount"`
	RetryCount     int                `json:"retryCount"`
	TerminalReason string             `json:"terminalReason,omitempty"`
	CausalEventSeq uint64             `json:"causalEventSeq,omitempty"`
}

// EscalationSelector identifies the gate or condition responsible.
type EscalationSelector struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// EventList is the complete durable event ledger for one run.
type EventList struct {
	RunID  string     `json:"runId"`
	Events []RunEvent `json:"events"`
}

// RunEvent exposes the shared event envelope. Type-specific fields are only
// populated for the schema this build owns; Raw retains an unknown event's
// complete scrubbed JSON for forward-compatible inspection.
type RunEvent struct {
	Schema       string                 `json:"schema"`
	Seq          uint64                 `json:"seq"`
	Type         journal.EventType      `json:"type"`
	Branch       int                    `json:"branch"`
	Time         time.Time              `json:"time"`
	KnownSchema  bool                   `json:"knownSchema"`
	Stage        string                 `json:"stage,omitempty"`
	Attempt      int                    `json:"attempt,omitempty"`
	AttemptClass string                 `json:"attemptClass,omitempty"`
	Gate         string                 `json:"gate,omitempty"`
	Verdict      string                 `json:"verdict,omitempty"`
	Target       string                 `json:"target,omitempty"`
	Status       string                 `json:"status,omitempty"`
	Outputs      map[string]any         `json:"outputs,omitempty"`
	Artifacts    []ArtifactMetadata     `json:"artifacts,omitempty"`
	Artifact     *ArtifactMetadata      `json:"artifact,omitempty"`
	Name         string                 `json:"name,omitempty"`
	ExternalRef  *journal.ExternalRef   `json:"externalRef,omitempty"`
	Error        *journal.ErrorDetail   `json:"error,omitempty"`
	Redaction    *journal.RedactionInfo `json:"redaction,omitempty"`
	Runner       map[string]any         `json:"runner,omitempty"`
	Workflow     string                 `json:"workflow,omitempty"`
	RunID        string                 `json:"runId,omitempty"`
	Reason       string                 `json:"reason,omitempty"`
	Raw          json.RawMessage        `json:"raw,omitempty"`
}

// ArtifactMetadata deliberately omits journal-relative paths. Content is
// addressed exclusively by RunID and Digest through Artifact.
type ArtifactMetadata struct {
	Name         string `json:"name,omitempty"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
	MediaType    string `json:"mediaType"`
	Stage        string `json:"stage,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	AttemptClass string `json:"attemptClass,omitempty"`
	RecordedSeq  uint64 `json:"recordedSeq,omitempty"`
}

// AttemptList contains every traversal of a stage, including repasses that
// restart at attempt one.
type AttemptList struct {
	RunID    string         `json:"runId"`
	Stage    string         `json:"stage"`
	Attempts []StageAttempt `json:"attempts"`
}

// StageAttempt is one durable stage traversal.
type StageAttempt struct {
	Number         int                  `json:"number"`
	Class          string               `json:"class"`
	Status         string               `json:"status"`
	StartedSeq     uint64               `json:"startedSeq,omitempty"`
	FinishedSeq    uint64               `json:"finishedSeq,omitempty"`
	StartedAt      *time.Time           `json:"startedAt,omitempty"`
	FinishedAt     *time.Time           `json:"finishedAt,omitempty"`
	DurationMillis int64                `json:"durationMillis"`
	Outputs        map[string]any       `json:"outputs,omitempty"`
	Artifacts      []ArtifactMetadata   `json:"artifacts"`
	Error          *journal.ErrorDetail `json:"error,omitempty"`
}

// ArtifactContent is a verified, already-redacted journal artifact.
type ArtifactContent struct {
	Metadata ArtifactMetadata
	Bytes    []byte
}

type runCursor struct {
	StartedAt time.Time `json:"startedAt"`
	RunID     string    `json:"runId"`
}

type runRead struct {
	reader   *journal.Reader
	identity journal.RunIdentity
	records  []journal.EventRecord
}

// ListRuns returns newest-first summaries, with RunID ascending as the stable
// tie-breaker.
func (s *Local) ListRuns(ctx context.Context, options RunListOptions) (RunList, error) {
	limit := options.Limit
	if limit == 0 {
		limit = defaultRunLimit
	}
	if limit < 1 || limit > maxRunLimit {
		return RunList{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidArgument, maxRunLimit)
	}
	if options.Phase != "" && !canonicalPhase(options.Phase) {
		return RunList{}, fmt.Errorf("%w: unknown phase %q", ErrInvalidArgument, options.Phase)
	}
	if options.Trigger != "" && !canonicalTrigger(options.Trigger) {
		return RunList{}, fmt.Errorf("%w: unknown trigger %q", ErrInvalidArgument, options.Trigger)
	}

	var cursor *runCursor
	if options.Cursor != "" {
		decoded, err := decodeRunCursor(options.Cursor)
		if err != nil {
			return RunList{}, err
		}
		cursor = &decoded
	}

	allSummaries, err := s.runSummaries(ctx, false)
	if err != nil {
		return RunList{}, err
	}
	summaries := make([]RunSummary, 0, len(allSummaries))
	for _, summary := range allSummaries {
		if options.Gaggle != "" && summary.Gaggle != options.Gaggle {
			continue
		}
		if options.Workflow != "" && summary.Workflow != options.Workflow {
			continue
		}
		if options.Phase != "" && summary.Phase != options.Phase {
			continue
		}
		if options.Trigger != "" && summary.Trigger.Kind != options.Trigger {
			continue
		}
		summaries = append(summaries, summary)
	}

	if cursor != nil {
		start := sort.Search(len(summaries), func(i int) bool {
			return runAfterCursor(summaries[i], *cursor)
		})
		summaries = summaries[start:]
	}

	result := RunList{Runs: summaries}
	if len(result.Runs) > limit {
		result.Runs = result.Runs[:limit]
		next, err := encodeRunCursor(result.Runs[len(result.Runs)-1])
		if err != nil {
			return RunList{}, err
		}
		result.NextCursor = next
	}
	if result.Runs == nil {
		result.Runs = []RunSummary{}
	}
	return result, nil
}

func (s *Local) runSummaries(ctx context.Context, skipUnreadable bool) ([]RunSummary, error) {
	entries, err := os.ReadDir(s.sources.Layout.RunsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []RunSummary{}, nil
		}
		return nil, fmt.Errorf("read runs directory: %w", err)
	}

	observedAt := s.now().UTC()
	summaries := make([]RunSummary, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		run, err := s.openRun(entry.Name())
		if err != nil {
			if skipUnreadable || errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		summary, err := summarizeRun(run, observedAt)
		if err != nil {
			if skipUnreadable {
				continue
			}
			return nil, fmt.Errorf("summarize run %q: %w", entry.Name(), err)
		}
		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].StartedAt.Equal(summaries[j].StartedAt) {
			return summaries[i].ID < summaries[j].ID
		}
		return summaries[i].StartedAt.After(summaries[j].StartedAt)
	})
	return summaries, nil
}

// GetRun returns one journal-derived run detail.
func (s *Local) GetRun(ctx context.Context, runID string) (RunDetail, error) {
	if err := ctx.Err(); err != nil {
		return RunDetail{}, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return RunDetail{}, err
	}
	summary, err := summarizeRun(run, s.now().UTC())
	if err != nil {
		return RunDetail{}, err
	}
	graph, status, err := pinnedGraph(run)
	if err != nil {
		return RunDetail{}, err
	}
	return RunDetail{
		RunSummary:  summary,
		Graph:       graph,
		GraphStatus: status,
		Escalation:  escalationCause(summary, run.records),
	}, nil
}

// RunEvents returns ordered event projections for a run.
func (s *Local) RunEvents(ctx context.Context, runID string) (EventList, error) {
	if err := ctx.Err(); err != nil {
		return EventList{}, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return EventList{}, err
	}
	artifacts := indexArtifacts(run.records)
	events := make([]RunEvent, len(run.records))
	for i, record := range run.records {
		events[i] = projectEvent(record, artifacts)
	}
	return EventList{RunID: run.identity.RunID, Events: events}, nil
}

// StageAttempts returns all attempts for one stage in durable traversal order.
func (s *Local) StageAttempts(ctx context.Context, runID, stage string) (AttemptList, error) {
	if err := ctx.Err(); err != nil {
		return AttemptList{}, err
	}
	if stage == "" {
		return AttemptList{}, fmt.Errorf("%w: stage is required", ErrInvalidArgument)
	}
	run, err := s.openRun(runID)
	if err != nil {
		return AttemptList{}, err
	}

	artifacts := indexArtifacts(run.records)
	attempts := make([]StageAttempt, 0)
	for _, record := range run.records {
		event := record.Event
		if !event.KnownSchema() || event.Stage != stage {
			continue
		}
		switch event.Type {
		case journal.EventStageStarted:
			started := event.Time
			attempts = append(attempts, StageAttempt{
				Number:     event.Attempt,
				Class:      attemptClass(event.AttemptClass),
				Status:     "running",
				StartedSeq: event.Seq,
				StartedAt:  &started,
				Artifacts:  []ArtifactMetadata{},
			})
		case journal.EventArtifactRecorded:
			if event.Ref == nil {
				continue
			}
			if i := matchingOpenAttempt(attempts, event.Attempt, event.AttemptClass); i >= 0 {
				if metadata, ok := artifacts.bySeq[event.Seq]; ok {
					attempts[i].Artifacts = append(attempts[i].Artifacts, metadata)
				}
			}
		case journal.EventError:
			if event.Error == nil || event.Error.Code != "executor_error" {
				continue
			}
			i := matchingOpenAttempt(attempts, event.Attempt, event.AttemptClass)
			if i < 0 {
				attempts = append(attempts, StageAttempt{
					Number:    event.Attempt,
					Class:     attemptClass(event.AttemptClass),
					Artifacts: []ArtifactMetadata{},
				})
				i = len(attempts) - 1
			}
			finishAttempt(&attempts[i], event, string(apiv1.ResultFailure), nil, event.Error)
		case journal.EventStageFinished:
			i := matchingOpenAttempt(attempts, event.Attempt, event.AttemptClass)
			if i < 0 {
				attempts = append(attempts, StageAttempt{
					Number:    event.Attempt,
					Class:     attemptClass(event.AttemptClass),
					Artifacts: []ArtifactMetadata{},
				})
				i = len(attempts) - 1
			}
			finishAttempt(&attempts[i], event, event.Status, event.Outputs, event.Error)
			for _, ref := range event.Artifacts {
				if metadata, ok := artifacts.match(
					ref.Digest,
					stage,
					event.Attempt,
					event.AttemptClass,
					event.Branch,
					attempts[i].StartedSeq,
					event.Seq,
				); ok &&
					!containsArtifact(attempts[i].Artifacts, metadata.RecordedSeq, metadata.Digest) {
					attempts[i].Artifacts = append(attempts[i].Artifacts, metadata)
				}
			}
		}
	}
	return AttemptList{RunID: run.identity.RunID, Stage: stage, Attempts: attempts}, nil
}

// Artifact returns bytes only for a digest recorded as an artifact in this run.
func (s *Local) Artifact(ctx context.Context, runID, digest string) (ArtifactContent, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactContent{}, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return ArtifactContent{}, err
	}
	index := indexArtifacts(run.records)
	entry, ok := index.byDigest[digest]
	if !ok {
		return ArtifactContent{}, fmt.Errorf("%w: artifact %q", ErrNotFound, digest)
	}
	data, err := run.reader.ArtifactByDigest(digest)
	if err != nil {
		return ArtifactContent{}, fmt.Errorf("%w: %w", ErrArtifactIntegrity, err)
	}
	return ArtifactContent{Metadata: entry.metadata, Bytes: data}, nil
}

func (s *Local) openRun(runID string) (runRead, error) {
	if !apiv1.ValidRunID(runID) {
		return runRead{}, fmt.Errorf("%w: invalid run id", ErrInvalidArgument)
	}
	dir := filepath.Join(s.sources.Layout.RunsDir(), runID)
	info, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runRead{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
		}
		return runRead{}, fmt.Errorf("inspect run %q: %w", runID, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return runRead{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runRead{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
		}
		return runRead{}, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return runRead{}, err
	}
	if identity.RunID != runID {
		return runRead{}, fmt.Errorf("run identity mismatch: directory %q records %q", runID, identity.RunID)
	}
	records, err := reader.EventRecords()
	if err != nil {
		return runRead{}, err
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Event.Seq == records[j].Event.Seq {
			return records[i].Event.Branch < records[j].Event.Branch
		}
		return records[i].Event.Seq < records[j].Event.Seq
	})
	return runRead{reader: reader, identity: identity, records: records}, nil
}

func summarizeRun(run runRead, observedAt time.Time) (RunSummary, error) {
	phase := journal.PhaseRunning
	var finishedAt *time.Time
	var lastSeq uint64
	currentStage := ""
	seenInitial := make(map[string]bool)
	repasses, retries, policyRetries, infraRetries := 0, 0, 0, 0

	for _, record := range run.records {
		event := record.Event
		if event.Seq > lastSeq {
			lastSeq = event.Seq
		}
		if !event.KnownSchema() {
			continue
		}
		switch event.Type {
		case journal.EventStageStarted:
			currentStage = event.Stage
			switch event.AttemptClass {
			case journal.AttemptPolicy:
				retries++
				policyRetries++
			case journal.AttemptInfra:
				retries++
				infraRetries++
			default:
				if seenInitial[event.Stage] {
					repasses++
				}
				seenInitial[event.Stage] = true
			}
		case journal.EventStageFinished:
			if currentStage == event.Stage {
				currentStage = ""
			}
		case journal.EventGateStarted:
			currentStage = event.Gate
		case journal.EventGateEvaluated:
			if currentStage == event.Gate {
				currentStage = ""
			}
		case journal.EventRunFinished:
			if !canonicalPhase(journal.RunPhase(event.Status)) || event.Status == string(journal.PhaseRunning) {
				return RunSummary{}, fmt.Errorf("unsupported terminal phase %q", event.Status)
			}
			phase = journal.RunPhase(event.Status)
			finished := event.Time
			finishedAt = &finished
			currentStage = ""
		}
	}

	if phase == journal.PhaseRunning {
		if state, err := run.reader.State(); err == nil && state.LastSeq >= lastSeq && state.MachineState != "" {
			currentStage = state.MachineState
		}
	}
	durationEnd := observedAt
	if finishedAt != nil {
		durationEnd = *finishedAt
	}
	var duration int64
	if !durationEnd.Before(run.identity.StartedAt) {
		duration = durationEnd.Sub(run.identity.StartedAt).Milliseconds()
	}

	return RunSummary{
		ID:               run.identity.RunID,
		Workflow:         run.identity.Workflow,
		WorkflowVersion:  run.identity.WorkflowVersion,
		WorkflowDigest:   run.identity.WorkflowDigest,
		Gaggle:           run.identity.Gaggle,
		Trigger:          run.identity.Trigger,
		Phase:            phase,
		Terminal:         phase != journal.PhaseRunning,
		CurrentStage:     currentStage,
		StartedAt:        run.identity.StartedAt,
		FinishedAt:       finishedAt,
		DurationMillis:   duration,
		LastSeq:          lastSeq,
		RepassCount:      repasses,
		RetryCount:       retries,
		PolicyRetryCount: policyRetries,
		InfraRetryCount:  infraRetries,
	}, nil
}

func pinnedGraph(run runRead) (*workflow.Graph, string, error) {
	var ref *journal.Ref
	for _, input := range run.identity.Inputs {
		if input.Name == journal.PinnedWorkflowGraphInputName {
			candidate := input.Ref
			ref = &candidate
			break
		}
	}
	if ref == nil {
		return nil, "unavailable", nil
	}
	data, err := run.reader.ArtifactBytes(*ref)
	if err != nil {
		return nil, "", fmt.Errorf("%w: pinned graph: %w", ErrArtifactIntegrity, err)
	}
	var graph workflow.Graph
	if err := json.Unmarshal(data, &graph); err != nil {
		return nil, "", fmt.Errorf("%w: parse pinned graph: %w", ErrArtifactIntegrity, err)
	}
	if graph.Name != run.identity.Workflow ||
		graph.Version != run.identity.WorkflowVersion ||
		graph.Digest != run.identity.WorkflowDigest {
		return nil, "", fmt.Errorf("%w: pinned graph identity does not match run", ErrArtifactIntegrity)
	}
	return &graph, "pinned", nil
}

func canonicalPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseRunning, journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}

func canonicalTrigger(trigger journal.TriggerKind) bool {
	switch trigger {
	case journal.TriggerManual, journal.TriggerSchedule, journal.TriggerSignal, journal.TriggerItem:
		return true
	default:
		return false
	}
}

func escalationCause(summary RunSummary, records []journal.EventRecord) *EscalationCause {
	if summary.Phase != journal.PhaseEscalated {
		return nil
	}
	cause := &EscalationCause{
		RepassCount: summary.RepassCount,
		RetryCount:  summary.RetryCount,
	}
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if event.KnownSchema() &&
			event.Type == journal.EventGateEvaluated &&
			event.Target == workflow.TargetEscalate {
			cause.Selector = EscalationSelector{Kind: "gate", Name: event.Gate}
			cause.SelectedBranch = event.Verdict
			cause.TerminalReason = gateEscalationReason(event)
			cause.CausalEventSeq = event.Seq
			return cause
		}
	}
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() || event.Type != journal.EventStageFinished {
			continue
		}
		cause.Selector = EscalationSelector{Kind: "stage", Name: event.Stage}
		cause.TerminalReason = stageEscalationReason(event, records[i+1:])
		cause.CausalEventSeq = event.Seq
		return cause
	}
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() || event.Type != journal.EventError {
			continue
		}
		name := event.Stage
		if name == "" {
			name = event.Gate
		}
		cause.Selector = EscalationSelector{Kind: "condition", Name: name}
		cause.TerminalReason = eventErrorReason(event.Error)
		cause.CausalEventSeq = event.Seq
		break
	}
	return cause
}

func gateEscalationReason(event journal.Event) string {
	escalated, _ := event.Runner["escalated"].(bool)
	if escalated {
		duplicateDiff, _ := event.Runner["duplicateDiff"].(bool)
		if duplicateDiff {
			return "repass produced a diff identical to the immediately prior attempt"
		}
		return "repass budget exhausted"
	}
	return fmt.Sprintf("gate %s resolved %s -> %s", event.Gate, event.Verdict, event.Target)
}

func stageEscalationReason(event journal.Event, subsequent []journal.EventRecord) string {
	if reason := eventErrorReason(event.Error); reason != "" {
		return reason
	}
	if event.Status == string(apiv1.ResultBlocked) {
		for _, record := range subsequent {
			candidate := record.Event
			if candidate.KnownSchema() &&
				candidate.Type == journal.EventError &&
				candidate.Stage == event.Stage &&
				candidate.Error != nil &&
				candidate.Error.Code == "blocked_by_agent" {
				return eventErrorReason(candidate.Error)
			}
		}
	}
	if event.Status != "" {
		return fmt.Sprintf("stage %s finished with status %s", event.Stage, event.Status)
	}
	return fmt.Sprintf("stage %s selected escalation", event.Stage)
}

func eventErrorReason(detail *journal.ErrorDetail) string {
	if detail == nil {
		return ""
	}
	if detail.Message != "" {
		return detail.Message
	}
	return detail.Code
}

func projectEvent(record journal.EventRecord, artifacts artifactIndex) RunEvent {
	event := record.Event
	projected := RunEvent{
		Schema:      event.Schema,
		Seq:         event.Seq,
		Type:        event.Type,
		Branch:      event.Branch,
		Time:        event.Time,
		KnownSchema: event.KnownSchema(),
	}
	if !projected.KnownSchema {
		projected.Raw = append(json.RawMessage(nil), record.Raw...)
		return projected
	}

	projected.Stage = event.Stage
	projected.Attempt = event.Attempt
	if event.Attempt > 0 {
		projected.AttemptClass = attemptClass(event.AttemptClass)
	}
	projected.Gate = event.Gate
	projected.Verdict = event.Verdict
	projected.Target = event.Target
	projected.Status = event.Status
	projected.Outputs = scalarOutputs(event.Outputs)
	for _, ref := range event.Artifacts {
		if metadata, ok := artifacts.match(
			ref.Digest,
			event.Stage,
			event.Attempt,
			event.AttemptClass,
			event.Branch,
			0,
			event.Seq,
		); ok {
			projected.Artifacts = append(projected.Artifacts, metadata)
		}
	}
	if metadata, ok := artifacts.bySeq[event.Seq]; ok {
		projected.Artifact = &metadata
	}
	projected.Name = event.Name
	projected.ExternalRef = event.ExternalRef
	projected.Error = event.Error
	projected.Redaction = event.Redaction
	projected.Runner = event.Runner
	projected.Workflow = event.Workflow
	projected.RunID = event.RunID
	projected.Reason = event.Reason
	return projected
}

type artifactEntry struct {
	metadata ArtifactMetadata
	branch   int
	inferred bool
}

type artifactIndex struct {
	entries      []artifactEntry
	byDigest     map[string]artifactEntry
	bySeq        map[uint64]ArtifactMetadata
	replacements map[string]string
}

func (i artifactIndex) match(
	digest, stage string,
	attempt int,
	class journal.AttemptClass,
	branch int,
	afterSeq, beforeSeq uint64,
) (ArtifactMetadata, bool) {
	index := i.matchEntryIndex(digest, stage, attempt, class, branch, afterSeq, beforeSeq)
	if index < 0 {
		return ArtifactMetadata{}, false
	}
	return i.entries[index].metadata, true
}

func (i artifactIndex) matchEntryIndex(
	digest, stage string,
	attempt int,
	class journal.AttemptClass,
	branch int,
	afterSeq, beforeSeq uint64,
) int {
	digest = i.currentDigest(digest)
	for index := len(i.entries) - 1; index >= 0; index-- {
		metadata := i.entries[index].metadata
		if metadata.Digest != digest ||
			(stage != "" && metadata.Stage != stage) ||
			(attempt != 0 && metadata.Attempt != attempt) ||
			(attempt != 0 && metadata.AttemptClass != attemptClass(class)) ||
			(i.entries[index].inferred && i.entries[index].branch != branch) ||
			metadata.RecordedSeq <= afterSeq ||
			(beforeSeq != 0 && metadata.RecordedSeq >= beforeSeq) {
			continue
		}
		return index
	}
	return -1
}

func (i artifactIndex) currentDigest(digest string) string {
	for count := 0; count < len(i.replacements); count++ {
		replacement, ok := i.replacements[digest]
		if !ok {
			break
		}
		digest = replacement
	}
	return digest
}

func indexArtifacts(records []journal.EventRecord) artifactIndex {
	index := artifactIndex{
		byDigest:     make(map[string]artifactEntry),
		bySeq:        make(map[uint64]ArtifactMetadata),
		replacements: make(map[string]string),
	}
	type attemptKey struct {
		stage   string
		attempt int
		class   string
		branch  int
	}
	started := make(map[attemptKey]uint64)
	active := make(map[int]attemptKey)
	for _, record := range records {
		event := record.Event
		if !event.KnownSchema() {
			continue
		}
		key := attemptKey{
			stage:   event.Stage,
			attempt: event.Attempt,
			class:   attemptClass(event.AttemptClass),
			branch:  event.Branch,
		}
		switch event.Type {
		case journal.EventStageStarted:
			started[key] = event.Seq
			active[event.Branch] = key
		case journal.EventArtifactRecorded:
			if event.Ref == nil {
				continue
			}
			path := filepath.ToSlash(filepath.Clean(event.Ref.Path))
			if !strings.HasPrefix(path, "artifacts/") {
				continue
			}
			metadata := ArtifactMetadata{
				Name:        event.Name,
				Digest:      event.Ref.Digest,
				Size:        event.Ref.Size,
				MediaType:   normalizeMediaType(event.Ref.MediaType),
				Stage:       event.Stage,
				Attempt:     event.Attempt,
				RecordedSeq: event.Seq,
			}
			inferred := false
			if metadata.Stage == "" {
				if scope, ok := active[event.Branch]; ok {
					metadata.Stage = scope.stage
					metadata.Attempt = scope.attempt
					metadata.AttemptClass = scope.class
					inferred = true
				}
			}
			if event.Attempt > 0 {
				metadata.AttemptClass = attemptClass(event.AttemptClass)
			}
			entry := artifactEntry{
				metadata: metadata,
				branch:   event.Branch,
				inferred: inferred,
			}
			index.entries = append(index.entries, entry)
			index.bySeq[event.Seq] = metadata
			if _, exists := index.byDigest[metadata.Digest]; !exists {
				index.byDigest[metadata.Digest] = entry
			}
		case journal.EventStageFinished:
			for _, ref := range event.Artifacts {
				entryIndex := index.matchEntryIndex(
					ref.Digest,
					event.Stage,
					event.Attempt,
					event.AttemptClass,
					event.Branch,
					started[key],
					event.Seq,
				)
				if entryIndex < 0 {
					continue
				}
				entry := &index.entries[entryIndex]
				entry.metadata.Size = ref.Size
				entry.metadata.MediaType = normalizeMediaType(ref.MediaType)
				index.bySeq[entry.metadata.RecordedSeq] = entry.metadata
				if current, ok := index.byDigest[entry.metadata.Digest]; ok &&
					current.metadata.RecordedSeq == entry.metadata.RecordedSeq {
					index.byDigest[entry.metadata.Digest] = *entry
				}
			}
			delete(started, key)
			delete(active, event.Branch)
		case journal.EventRedaction:
			index.applyRedaction(event)
		}
	}
	return index
}

func (i *artifactIndex) applyRedaction(event journal.Event) {
	if event.Ref == nil ||
		event.Redaction == nil ||
		event.Ref.Digest != event.Redaction.NewDigest {
		return
	}
	path := filepath.ToSlash(filepath.Clean(event.Ref.Path))
	if !strings.HasPrefix(path, "artifacts/") {
		return
	}

	i.replacements[event.Redaction.OldDigest] = event.Redaction.NewDigest
	delete(i.byDigest, event.Redaction.OldDigest)
	var replacement *artifactEntry
	for index := range i.entries {
		entry := &i.entries[index]
		if entry.metadata.Digest != event.Redaction.OldDigest {
			continue
		}
		entry.metadata.Digest = event.Redaction.NewDigest
		entry.metadata.Size = event.Ref.Size
		i.bySeq[entry.metadata.RecordedSeq] = entry.metadata
		if replacement == nil {
			copy := *entry
			replacement = &copy
		}
	}
	if replacement == nil {
		return
	}
	if _, exists := i.byDigest[event.Redaction.NewDigest]; !exists {
		i.byDigest[event.Redaction.NewDigest] = *replacement
	}
	i.bySeq[event.Seq] = replacement.metadata
}

func finishAttempt(
	attempt *StageAttempt,
	event journal.Event,
	status string,
	outputs map[string]any,
	detail *journal.ErrorDetail,
) {
	finished := event.Time
	if event.AttemptClass != "" {
		attempt.Class = attemptClass(event.AttemptClass)
	}
	attempt.Status = status
	attempt.FinishedSeq = event.Seq
	attempt.FinishedAt = &finished
	attempt.Outputs = scalarOutputs(outputs)
	attempt.Error = detail
	if attempt.StartedAt != nil && !finished.Before(*attempt.StartedAt) {
		attempt.DurationMillis = finished.Sub(*attempt.StartedAt).Milliseconds()
	}
}

func normalizeMediaType(value string) string {
	if value == "" {
		return "application/octet-stream"
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "application/octet-stream"
	}
	return mime.FormatMediaType(mediaType, params)
}

func attemptClass(class journal.AttemptClass) string {
	if class == "" {
		return "initial"
	}
	return string(class)
}

func matchingOpenAttempt(attempts []StageAttempt, number int, class journal.AttemptClass) int {
	wantClass := attemptClass(class)
	fallback := -1
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].FinishedSeq != 0 || attempts[i].Number != number {
			continue
		}
		if attempts[i].Class == wantClass {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

func scalarOutputs(outputs map[string]any) map[string]any {
	var projected map[string]any
	for key, value := range outputs {
		switch value.(type) {
		case nil, bool, string, float64, json.Number, int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64:
			if projected == nil {
				projected = make(map[string]any)
			}
			projected[key] = value
		}
	}
	return projected
}

func containsArtifact(artifacts []ArtifactMetadata, seq uint64, digest string) bool {
	for _, artifact := range artifacts {
		if artifact.RecordedSeq == seq && artifact.Digest == digest {
			return true
		}
	}
	return false
}

func encodeRunCursor(summary RunSummary) (string, error) {
	data, err := json.Marshal(runCursor{StartedAt: summary.StartedAt, RunID: summary.ID})
	if err != nil {
		return "", fmt.Errorf("encode run cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeRunCursor(value string) (runCursor, error) {
	if len(value) > 1024 {
		return runCursor{}, fmt.Errorf("%w: cursor is too long", ErrInvalidArgument)
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return runCursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cursor runCursor
	if err := decoder.Decode(&cursor); err != nil {
		return runCursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	if !apiv1.ValidRunID(cursor.RunID) {
		return runCursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return runCursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	return cursor, nil
}

func runAfterCursor(summary RunSummary, cursor runCursor) bool {
	return summary.StartedAt.Before(cursor.StartedAt) ||
		(summary.StartedAt.Equal(cursor.StartedAt) && summary.ID > cursor.RunID)
}
