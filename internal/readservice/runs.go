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
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
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

// OutcomeFilter selects the run or attempt population behind an Insight metric.
type OutcomeFilter string

// OutcomeFinished, OutcomeTerminal, OutcomeSuccess, OutcomeFailure, and
// OutcomeOther are the canonical metric populations.
const (
	OutcomeFinished OutcomeFilter = "finished"
	OutcomeTerminal OutcomeFilter = "terminal"
	OutcomeSuccess  OutcomeFilter = "success"
	OutcomeFailure  OutcomeFilter = "failure"
	OutcomeOther    OutcomeFilter = "other"
)

// StagePopulation selects which attempts of a stage contribute runs.
type StagePopulation string

// StagePopulationAttempts and StagePopulationMeasured are the canonical stage
// populations.
const (
	StagePopulationAttempts StagePopulation = "attempts"
	StagePopulationMeasured StagePopulation = "measured"
)

// OfflineRuns is the shared run-diagnostics boundary used by CLI adapters
// when no daemon is running.
type OfflineRuns interface {
	ListRuns(context.Context, RunListOptions) (RunList, error)
	RunIDs(context.Context) ([]string, error)
	GetRun(context.Context, string) (RunDetail, error)
	RunMetadata(context.Context, string) (journal.RunIdentity, *journal.State, error)
	RunEvents(context.Context, string) (EventList, error)
	StageAttempts(context.Context, string, string) (AttemptList, error)
	Artifact(context.Context, string, string) (ArtifactContent, error)
	RunTranscripts(context.Context, string, string) ([]TranscriptContent, error)
	RunSpans(context.Context, string) ([]rollup.SpanSummary, error)
	RunEscalation(context.Context, string) (*TraceEscalation, error)
	RunTraceRepassCount(context.Context, string) (int, error)
}

// NewOfflineRuns constructs the in-process run reader used for historic CLI
// inspection. Run reads depend only on the journal layout, not current config.
func NewOfflineRuns(layout instance.Layout) (OfflineRuns, error) {
	return &Local{
		sources: LocalSources{Layout: layout},
		ready:   func() bool { return false },
		now:     time.Now,
	}, nil
}

// RunListOptions controls deterministic run filtering and keyset pagination.
type RunListOptions struct {
	Gaggle          string
	Workflow        string
	Stage           string
	Outcome         OutcomeFilter
	StagePopulation StagePopulation
	Phase           journal.RunPhase
	Trigger         journal.TriggerKind
	Since           time.Time
	Until           time.Time
	Limit           int
	Cursor          string
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
	LastActivityAt   time.Time        `json:"lastActivityAt"`
	LastSeq          uint64           `json:"lastSeq"`
	RepassCount      int              `json:"repassCount"`
	RetryCount       int              `json:"retryCount"`
	PolicyRetryCount int              `json:"policyRetryCount"`
	InfraRetryCount  int              `json:"infraRetryCount"`
	Stages           []string         `json:"-"`
	stageAttempts    map[string][]StageAttempt
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
	Schema          string                 `json:"schema"`
	Seq             uint64                 `json:"seq"`
	Type            journal.EventType      `json:"type"`
	Branch          int                    `json:"branch"`
	Time            time.Time              `json:"time"`
	KnownSchema     bool                   `json:"knownSchema"`
	Stage           string                 `json:"stage,omitempty"`
	Attempt         int                    `json:"attempt,omitempty"`
	AttemptClass    string                 `json:"attemptClass,omitempty"`
	Gate            string                 `json:"gate,omitempty"`
	Verdict         string                 `json:"verdict,omitempty"`
	Target          string                 `json:"target,omitempty"`
	Escalated       bool                   `json:"escalated,omitempty"`
	Status          string                 `json:"status,omitempty"`
	Actor           string                 `json:"actor,omitempty"`
	WorkflowVersion int                    `json:"workflowVersion,omitempty"`
	WorkflowDigest  string                 `json:"workflowDigest,omitempty"`
	Outputs         map[string]any         `json:"outputs,omitempty"`
	Artifacts       []ArtifactMetadata     `json:"artifacts,omitempty"`
	Artifact        *ArtifactMetadata      `json:"artifact,omitempty"`
	Name            string                 `json:"name,omitempty"`
	ExternalRef     *journal.ExternalRef   `json:"externalRef,omitempty"`
	Error           *journal.ErrorDetail   `json:"error,omitempty"`
	Redaction       *journal.RedactionInfo `json:"redaction,omitempty"`
	Runner          map[string]any         `json:"runner,omitempty"`
	Workflow        string                 `json:"workflow,omitempty"`
	RunID           string                 `json:"runId,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
	Raw             json.RawMessage        `json:"raw,omitempty"`
	JournalEvent    *journal.Event         `json:"-"`
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

// TranscriptContent is one verified agent transcript recorded in a run.
type TranscriptContent struct {
	Seq   uint64
	Stage string
	Name  string
	Bytes []byte
}

// TraceEscalation retains the legacy CLI's gate-specific repass count and
// reviewer rationale without extending the HTTP run-detail contract.
type TraceEscalation struct {
	RepassCount            int
	LastNeedsChangesReason string
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
	if options.Outcome != "" && !canonicalOutcome(options.Outcome) {
		return RunList{}, fmt.Errorf("%w: unknown outcome %q", ErrInvalidArgument, options.Outcome)
	}
	if options.StagePopulation != "" && !canonicalStagePopulation(options.StagePopulation) {
		return RunList{}, fmt.Errorf("%w: unknown stage population %q", ErrInvalidArgument, options.StagePopulation)
	}
	if options.StagePopulation != "" && options.Stage == "" {
		return RunList{}, fmt.Errorf("%w: stage population requires a stage", ErrInvalidArgument)
	}
	if !options.Since.IsZero() && !options.Until.IsZero() && options.Since.After(options.Until) {
		return RunList{}, fmt.Errorf("%w: since must not be after until", ErrInvalidArgument)
	}

	var cursor *runCursor
	if options.Cursor != "" {
		decoded, err := decodeRunCursor(options.Cursor)
		if err != nil {
			return RunList{}, err
		}
		cursor = &decoded
	}

	if s.sources.Telemetry != nil {
		return s.listRunsIndexed(ctx, options, cursor, limit)
	}
	return s.listRunsScanning(ctx, options, cursor, limit)
}

// runMatches reports whether summary satisfies every option filter. The
// summary is always journal-hydrated and therefore authoritative, so this is
// the single predicate both the scanning and indexed paths apply — a lagging
// index status can never wrongly include or hide a run because Phase and
// stageAttempts here come from the journal, not the index.
func (s *Local) runMatches(summary RunSummary, options RunListOptions) bool {
	switch {
	case options.Gaggle != "" && summary.Gaggle != options.Gaggle:
		return false
	case options.Workflow != "" && summary.Workflow != options.Workflow:
		return false
	case options.Stage != "" && !containsString(summary.Stages, options.Stage):
		return false
	case (options.Outcome != "" || options.StagePopulation != "") && !summary.Terminal:
		return false
	case options.Stage != "" &&
		(options.Outcome != "" || options.StagePopulation != "") &&
		!matchesStageAttempt(summary.stageAttempts[options.Stage], options.Outcome, options.StagePopulation):
		return false
	case options.Stage == "" && options.Outcome != "" && !matchesRunOutcome(summary.Phase, options.Outcome):
		return false
	case options.Phase != "" && summary.Phase != options.Phase:
		return false
	case options.Trigger != "" && summary.Trigger.Kind != options.Trigger:
		return false
	case !options.Since.IsZero() && summary.StartedAt.Before(options.Since):
		return false
	case !options.Until.IsZero() && summary.StartedAt.After(options.Until):
		return false
	}
	return true
}

func attemptStageFor(options RunListOptions) string {
	if options.Stage != "" && (options.Outcome != "" || options.StagePopulation != "") {
		return options.Stage
	}
	return ""
}

func paginateRuns(summaries []RunSummary, limit int) (RunList, error) {
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

// listRunsScanning is the journal-authoritative fallback used when no telemetry
// index is available (offline/CLI reads). It opens and summarizes every run.
func (s *Local) listRunsScanning(ctx context.Context, options RunListOptions, cursor *runCursor, limit int) (RunList, error) {
	allSummaries, err := s.runSummariesForStage(ctx, false, attemptStageFor(options))
	if err != nil {
		return RunList{}, err
	}
	summaries := make([]RunSummary, 0, len(allSummaries))
	for _, summary := range allSummaries {
		if s.runMatches(summary, options) {
			summaries = append(summaries, summary)
		}
	}
	if cursor != nil {
		start := sort.Search(len(summaries), func(i int) bool {
			return runAfterCursor(summaries[i], *cursor)
		})
		summaries = summaries[start:]
	}
	return paginateRuns(summaries, limit)
}

// listRunsIndexed serves the list from the telemetry index without parsing
// every run's journal (DASH-18). The index chooses WHICH runs to open — bounded
// by page size, filters, and the keyset cursor — and each returned run's
// summary is hydrated from its journal so displayed data is always
// authoritative. Completeness is guaranteed by reconcileIndex, so a run present
// on disk but absent from the index (migrated/imported/still in flight) is
// never silently hidden.
func (s *Local) listRunsIndexed(ctx context.Context, options RunListOptions, cursor *runCursor, limit int) (RunList, error) {
	if err := s.reconcileIndex(ctx); err != nil {
		return RunList{}, err
	}

	observedAt := s.now().UTC()
	attemptStage := attemptStageFor(options)
	filter := rollup.RunListFilter{
		Gaggle:      options.Gaggle,
		Workflow:    options.Workflow,
		TriggerKind: string(options.Trigger),
		Since:       options.Since,
		Until:       options.Until,
	}

	// Keyset the first index fetch from the caller's cursor; thereafter advance
	// from the last row examined, so residual (phase/stage/outcome) filtering
	// that drops candidates still makes forward progress instead of re-reading
	// the same page.
	var keyStarted time.Time
	var keyRunID string
	if cursor != nil {
		keyStarted, keyRunID = cursor.StartedAt, cursor.RunID
	}

	const pageSize = 100
	kept := make([]RunSummary, 0, limit+1)
	for len(kept) <= limit {
		if err := ctx.Err(); err != nil {
			return RunList{}, err
		}
		refs, err := s.sources.Telemetry.RunRefPage(filter, keyStarted, keyRunID, pageSize)
		if err != nil {
			return RunList{}, err
		}
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			keyStarted, keyRunID = ref.StartedAt, ref.RunID
			run, err := s.openRun(ref.RunID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return RunList{}, err
			}
			summary, err := summarizeRunForStage(run, observedAt, attemptStage)
			if err != nil {
				return RunList{}, fmt.Errorf("summarize run %q: %w", ref.RunID, err)
			}
			if !s.runMatches(summary, options) {
				continue
			}
			kept = append(kept, summary)
			if len(kept) > limit {
				break
			}
		}
		if len(refs) < pageSize {
			break
		}
	}
	return paginateRuns(kept, limit)
}

// reconcileIndex backfills any on-disk run absent from the telemetry index so a
// list served from the index can never hide a run — the reviewer's
// "migrated telemetry" concern. Steady state is a no-op: the daemon ingests
// every run it executes, so the set difference is empty and this only lists
// directory names (never parses a journal). An individual run that fails to
// ingest simply stays out of this pass rather than failing the whole list.
func (s *Local) reconcileIndex(ctx context.Context) error {
	indexed, err := s.sources.Telemetry.IndexedRunIDs()
	if err != nil {
		return err
	}
	runDirs, err := s.sources.Layout.RunDirs()
	if err != nil {
		return err
	}
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read runs directory: %w", err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.IsDir() || !apiv1.ValidRunID(entry.Name()) {
				continue
			}
			if _, ok := indexed[entry.Name()]; ok {
				continue
			}
			_ = s.sources.Telemetry.IngestRun(filepath.Join(runsDir, entry.Name()))
		}
	}
	return nil
}

func (s *Local) runSummaries(ctx context.Context, skipUnreadable bool) ([]RunSummary, error) {
	return s.runSummariesForStage(ctx, skipUnreadable, "")
}

func (s *Local) runSummariesForStage(
	ctx context.Context,
	skipUnreadable bool,
	attemptStage string,
) ([]RunSummary, error) {
	runDirs, err := s.sources.Layout.RunDirs()
	if err != nil {
		return nil, err
	}

	observedAt := s.now().UTC()
	var summaries []RunSummary
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read runs directory: %w", err)
		}
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
			summary, err := summarizeRunForStage(run, observedAt, attemptStage)
			if err != nil {
				if skipUnreadable {
					continue
				}
				return nil, fmt.Errorf("summarize run %q: %w", entry.Name(), err)
			}
			summaries = append(summaries, summary)
		}
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].StartedAt.Equal(summaries[j].StartedAt) {
			return summaries[i].ID < summaries[j].ID
		}
		return summaries[i].StartedAt.After(summaries[j].StartedAt)
	})
	return summaries, nil
}

// RunIDs returns valid run directory names in lexical order without opening
// their journals. It supports prefix resolution without making unrelated
// corrupt journals block an otherwise valid trace lookup.
func (s *Local) RunIDs(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runDirs, err := s.sources.Layout.RunDirs()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read runs directory: %w", err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if entry.IsDir() && apiv1.ValidRunID(entry.Name()) {
				ids = append(ids, entry.Name())
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
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
	escalation, err := escalationCause(summary, run.records)
	if err != nil {
		return RunDetail{}, err
	}
	return RunDetail{
		RunSummary:  summary,
		Graph:       graph,
		GraphStatus: status,
		Escalation:  escalation,
	}, nil
}

// RunMetadata returns the exact recorded identity and optional checkpoint used
// by legacy CLI presentation. RunDetail remains the canonical product model.
func (s *Local) RunMetadata(ctx context.Context, runID string) (journal.RunIdentity, *journal.State, error) {
	if err := ctx.Err(); err != nil {
		return journal.RunIdentity{}, nil, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return journal.RunIdentity{}, nil, err
	}
	state, err := run.reader.State()
	if err != nil {
		return run.identity, nil, nil
	}
	return run.identity, &state, nil
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

	attempts := collectStageAttempts(run.records, indexArtifacts(run.records), stage)[stage]
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

// RunTranscripts returns verified transcript blobs in durable event order. An
// optional stage limits reads before any blob is opened.
func (s *Local) RunTranscripts(ctx context.Context, runID, stage string) ([]TranscriptContent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return nil, err
	}
	transcripts := make([]TranscriptContent, 0)
	for _, record := range run.records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		event := record.Event
		recordedStage := strings.TrimPrefix(event.Stage, runID+":")
		if !event.KnownSchema() ||
			event.Type != journal.EventSpanRecorded ||
			(event.Name != "transcript" && !strings.HasSuffix(event.Name, ".transcript")) ||
			(stage != "" && recordedStage != stage) {
			continue
		}
		if event.Ref == nil {
			return nil, fmt.Errorf(
				"transcript for stage %q at seq %d is unavailable: span event has no content reference",
				recordedStage,
				event.Seq,
			)
		}
		data, err := run.reader.SpanBytes(*event.Ref)
		if err != nil {
			return nil, fmt.Errorf(
				"transcript for stage %q at seq %d is unavailable: %w",
				recordedStage,
				event.Seq,
				err,
			)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf(
				"transcript for stage %q at seq %d is unavailable: recorded content is empty",
				recordedStage,
				event.Seq,
			)
		}
		transcripts = append(transcripts, TranscriptContent{
			Seq:   event.Seq,
			Stage: recordedStage,
			Name:  event.Name,
			Bytes: data,
		})
	}
	return transcripts, nil
}

// RunSpans returns rollup-ingested spans for one run. A missing telemetry
// database is a valid empty result for telemetry-disabled instances.
func (s *Local) RunSpans(ctx context.Context, runID string) ([]rollup.SpanSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.openRun(runID); err != nil {
		return nil, err
	}
	empty := []rollup.SpanSummary{}
	db := s.sources.Telemetry
	if db == nil {
		if _, err := os.Stat(s.sources.Layout.TelemetryDB()); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return empty, nil
			}
			return nil, err
		}
		var err error
		db, err = rollup.Open(s.sources.Layout.TelemetryDB())
		if err != nil {
			return nil, err
		}
		defer func() { _ = db.Close() }()
	}
	spans, err := db.Spans(runID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spans == nil {
		return empty, nil
	}
	return spans, nil
}

// RunEscalation returns the gate-specific values required by the legacy trace
// summary. The canonical RunDetail remains unchanged for HTTP consumers.
func (s *Local) RunEscalation(ctx context.Context, runID string) (*TraceEscalation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return nil, err
	}
	summary, err := summarizeRun(run, s.now().UTC())
	if err != nil {
		return nil, err
	}
	if summary.Phase != journal.PhaseEscalated {
		return nil, nil
	}
	records := currentLifecycleRecords(run.records)
	result := &TraceEscalation{}
	terminalStage := successfulTerminalStage(records)
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !isEscalatingGateEvent(event, terminalStage) {
			continue
		}
		result.RepassCount, err = gateRepassCount(records[:i+1], event.Gate)
		if err != nil {
			return nil, err
		}
		result.LastNeedsChangesReason, err = lastNeedsChangesReason(run.reader, records[:i+1], event.Gate)
		if err != nil {
			return nil, err
		}
		break
	}
	return result, nil
}

// RunTraceRepassCount preserves the V0 trace contract, where a repass is a
// gate transition back to "implement". Journals for workflows with a different
// repass target use the canonical repeated-stage count.
func (s *Local) RunTraceRepassCount(ctx context.Context, runID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return 0, err
	}
	summary, err := summarizeRun(run, s.now().UTC())
	if err != nil {
		return 0, err
	}
	legacyCount := 0
	for _, record := range run.records {
		event := record.Event
		if event.KnownSchema() &&
			event.Type == journal.EventGateEvaluated &&
			event.Target == "implement" {
			legacyCount++
		}
	}
	if legacyCount > 0 {
		return legacyCount, nil
	}
	return summary.RepassCount, nil
}

// openRunObserver, when non-nil, is invoked with each run id openRun reads. It
// is a test seam used to assert that the indexed list path opens a number of
// journals bounded by page size rather than scanning every run. Always nil in
// production.
var openRunObserver func(runID string)

func (s *Local) openRun(runID string) (runRead, error) {
	if openRunObserver != nil {
		openRunObserver(runID)
	}
	if !apiv1.ValidRunID(runID) {
		return runRead{}, fmt.Errorf("%w: invalid run id", ErrInvalidArgument)
	}
	dir, err := s.sources.Layout.FindRunDir(runID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runRead{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
		}
		return runRead{}, err
	}
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
	return summarizeRunForStage(run, observedAt, "")
}

func summarizeRunForStage(
	run runRead,
	observedAt time.Time,
	attemptStage string,
) (RunSummary, error) {
	phase := journal.PhaseRunning
	var finishedAt *time.Time
	var lastSeq uint64
	var lastActivityAt time.Time
	currentStage := ""
	seenStages := make(map[string]struct{})
	repasses, retries, policyRetries, infraRetries := countStageAttempts(run.records)

	for _, record := range run.records {
		event := record.Event
		if event.Seq > lastSeq {
			lastSeq = event.Seq
			lastActivityAt = event.Time
		}
		if !event.KnownSchema() {
			continue
		}
		if event.Stage != "" {
			seenStages[event.Stage] = struct{}{}
		}
		if event.Gate != "" {
			seenStages[event.Gate] = struct{}{}
		}
		switch event.Type {
		case journal.EventRunResumed:
			phase = journal.PhaseRunning
			finishedAt = nil
			currentStage = event.Target
		case journal.EventStageStarted:
			currentStage = event.Stage
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
		case journal.EventStageRerunRequested:
			phase = journal.PhaseRunning
			finishedAt = nil
			currentStage = event.Stage
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
	stages := make([]string, 0, len(seenStages))
	for stage := range seenStages {
		stages = append(stages, stage)
	}
	sort.Strings(stages)
	var stageAttempts map[string][]StageAttempt
	if attemptStage != "" {
		stageAttempts = collectStageAttempts(run.records, artifactIndex{}, attemptStage)
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
		LastActivityAt:   lastActivityAt,
		LastSeq:          lastSeq,
		RepassCount:      repasses,
		RetryCount:       retries,
		PolicyRetryCount: policyRetries,
		InfraRetryCount:  infraRetries,
		Stages:           stages,
		stageAttempts:    stageAttempts,
	}, nil
}

func matchesRunOutcome(phase journal.RunPhase, outcome OutcomeFilter) bool {
	switch outcome {
	case OutcomeFinished:
		return phase != journal.PhaseRunning
	case OutcomeTerminal:
		return phase == journal.PhaseCompleted || phase == journal.PhaseFailed
	case OutcomeSuccess:
		return phase == journal.PhaseCompleted
	case OutcomeFailure:
		return phase == journal.PhaseFailed
	case OutcomeOther:
		return phase == journal.PhaseAborted || phase == journal.PhaseEscalated
	default:
		return true
	}
}

func matchesStageAttempt(
	attempts []StageAttempt,
	outcome OutcomeFilter,
	population StagePopulation,
) bool {
	for _, attempt := range attempts {
		if population == StagePopulationMeasured &&
			(attempt.StartedAt == nil ||
				attempt.FinishedAt == nil ||
				attempt.FinishedAt.Before(*attempt.StartedAt)) {
			continue
		}
		switch outcome {
		case OutcomeFinished:
		case OutcomeTerminal:
			if attempt.Status != string(apiv1.ResultSuccess) &&
				attempt.Status != string(apiv1.ResultFailure) {
				continue
			}
		case OutcomeSuccess:
			if attempt.Status != string(apiv1.ResultSuccess) {
				continue
			}
		case OutcomeFailure:
			if attempt.Status != string(apiv1.ResultFailure) {
				continue
			}
		case OutcomeOther:
			if attempt.Status == string(apiv1.ResultSuccess) ||
				attempt.Status == string(apiv1.ResultFailure) {
				continue
			}
		}
		return true
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func canonicalOutcome(outcome OutcomeFilter) bool {
	switch outcome {
	case OutcomeFinished, OutcomeTerminal, OutcomeSuccess, OutcomeFailure, OutcomeOther:
		return true
	default:
		return false
	}
}

func canonicalStagePopulation(population StagePopulation) bool {
	switch population {
	case StagePopulationAttempts, StagePopulationMeasured:
		return true
	default:
		return false
	}
}

func countStageAttempts(records []journal.EventRecord) (repasses, retries, policyRetries, infraRetries int) {
	seenInitial := make(map[string]bool)
	for _, record := range records {
		event := record.Event
		if !event.KnownSchema() || event.Type != journal.EventStageStarted {
			continue
		}
		switch event.AttemptClass {
		case journal.AttemptPolicy:
			retries++
			policyRetries++
		case journal.AttemptInfra:
			retries++
			infraRetries++
		case journal.AttemptHuman:
			// An explicit operator rerun is neither a policy/infra retry
			// nor an automatic gate repass.
		default:
			if seenInitial[event.Stage] {
				repasses++
			}
			seenInitial[event.Stage] = true
		}
	}
	return repasses, retries, policyRetries, infraRetries
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

func escalationCause(summary RunSummary, records []journal.EventRecord) (*EscalationCause, error) {
	if summary.Phase != journal.PhaseEscalated {
		return nil, nil
	}
	records = currentLifecycleRecords(records)
	repasses, retries, _, _ := countStageAttempts(records)
	cause := &EscalationCause{
		RepassCount: repasses,
		RetryCount:  retries,
	}
	terminalStage := successfulTerminalStage(records)
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if isEscalatingGateEvent(event, terminalStage) {
			cause.Selector = EscalationSelector{Kind: "gate", Name: event.Gate}
			cause.SelectedBranch = event.Verdict
			cause.TerminalReason = gateEscalationReason(event)
			cause.CausalEventSeq = event.Seq
			repasses, err := gateRepassCount(records[:i+1], event.Gate)
			if err != nil {
				return nil, err
			}
			cause.RepassCount = repasses
			return cause, nil
		}
	}
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() ||
			event.Type != journal.EventStageFinished ||
			(event.Status != string(apiv1.ResultFailure) &&
				event.Status != string(apiv1.ResultBlocked)) {
			continue
		}
		cause.Selector = EscalationSelector{Kind: "stage", Name: event.Stage}
		cause.TerminalReason = stageEscalationReason(event, records[i+1:])
		cause.CausalEventSeq = event.Seq
		return cause, nil
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
	return cause, nil
}

func currentLifecycleRecords(records []journal.EventRecord) []journal.EventRecord {
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if event.KnownSchema() && event.Type == journal.EventRunResumed {
			return records[i+1:]
		}
	}
	return records
}

func successfulTerminalStage(records []journal.EventRecord) string {
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() || event.Type != journal.EventStageFinished {
			continue
		}
		if event.Status == string(apiv1.ResultSuccess) {
			return event.Stage
		}
		return ""
	}
	return ""
}

func gateRepassCount(records []journal.EventRecord, gate string) (int, error) {
	if len(records) == 0 {
		return 0, fmt.Errorf("repass count for gate %q: no events", gate)
	}
	if raw, ok := records[len(records)-1].Event.Runner["repassAttempt"]; ok {
		data, err := json.Marshal(raw)
		if err != nil {
			return 0, fmt.Errorf("marshal repass count for gate %q: %w", gate, err)
		}
		var count int
		if err := json.Unmarshal(data, &count); err != nil {
			return 0, fmt.Errorf("parse repass count for gate %q: %w", gate, err)
		}
		if count < 0 {
			return 0, fmt.Errorf("invalid repass count %d for gate %q", count, gate)
		}
		return count, nil
	}

	count := 0
	for _, record := range records {
		event := record.Event
		if !event.KnownSchema() || event.Type != journal.EventGateEvaluated || event.Gate != gate {
			continue
		}
		if event.Verdict == string(apiv1.VerdictPass) {
			count = 0
		} else {
			count++
		}
	}
	return count, nil
}

func lastNeedsChangesReason(reader *journal.Reader, records []journal.EventRecord, gate string) (string, error) {
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() ||
			event.Type != journal.EventGateEvaluated ||
			event.Gate != gate ||
			event.Verdict != string(apiv1.VerdictNeedsChanges) {
			continue
		}
		if event.Ref == nil {
			break
		}
		data, err := reader.ArtifactBytes(*event.Ref)
		if err != nil {
			return "", fmt.Errorf("read verdict for gate %q: %w", gate, err)
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal(data, &verdict); err != nil {
			return "", fmt.Errorf("parse verdict for gate %q: %w", gate, err)
		}
		if verdict.Decision != apiv1.VerdictNeedsChanges {
			return "", fmt.Errorf(
				"verdict artifact for gate %q has decision %q, want %q",
				gate,
				verdict.Decision,
				apiv1.VerdictNeedsChanges,
			)
		}
		if reason := strings.TrimSpace(verdict.Rationale); reason != "" {
			return reason, nil
		}
		return strings.TrimSpace(verdict.Summary), nil
	}
	return "", nil
}

func gateEscalationReason(event journal.Event) string {
	if gateMarkedEscalated(event) {
		duplicateDiff, _ := event.Runner["duplicateDiff"].(bool)
		if duplicateDiff {
			return "repass produced a diff identical to the immediately prior attempt"
		}
		return "repass budget exhausted"
	}
	return fmt.Sprintf("gate %s resolved %s -> %s", event.Gate, event.Verdict, event.Target)
}

func gateMarkedEscalated(event journal.Event) bool {
	if event.Escalated {
		return true
	}
	escalated, _ := event.Runner["escalated"].(bool)
	return escalated
}

func isEscalatingGateEvent(event journal.Event, terminalStage string) bool {
	return event.KnownSchema() &&
		event.Type == journal.EventGateEvaluated &&
		(event.Target == workflow.TargetEscalate ||
			gateMarkedEscalated(event) ||
			(terminalStage != "" && event.Target == terminalStage))
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
	journalEvent := event
	projected.JournalEvent = &journalEvent

	projected.Stage = event.Stage
	projected.Attempt = event.Attempt
	if event.Attempt > 0 {
		projected.AttemptClass = attemptClass(event.AttemptClass)
	}
	projected.Gate = event.Gate
	projected.Verdict = event.Verdict
	projected.Target = event.Target
	projected.Escalated = event.Escalated
	projected.Status = event.Status
	projected.Actor = event.Actor
	projected.WorkflowVersion = event.WorkflowVersion
	projected.WorkflowDigest = event.WorkflowDigest
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

func collectStageAttempts(
	records []journal.EventRecord,
	artifacts artifactIndex,
	stage string,
) map[string][]StageAttempt {
	byStage := make(map[string][]StageAttempt)
	if stage != "" {
		byStage[stage] = []StageAttempt{}
	}
	for _, record := range records {
		event := record.Event
		if !event.KnownSchema() {
			continue
		}
		// A terminal run closes any attempt still open. A gate whose evaluation
		// errors terminally emits no stage.finished (and its error is not an
		// executor_error), so without this its attempt would project as
		// permanently "running" — the DASH-20 regression. run.finished carries
		// no stage, so it must be handled before the per-stage filter.
		if event.Type == journal.EventRunFinished {
			finished := event.Time
			for st := range byStage {
				for i := range byStage[st] {
					if byStage[st][i].FinishedSeq != 0 {
						continue
					}
					attempt := &byStage[st][i]
					attempt.Status = string(apiv1.ResultFailure)
					attempt.FinishedSeq = event.Seq
					attempt.FinishedAt = &finished
					if attempt.StartedAt != nil && !finished.Before(*attempt.StartedAt) {
						attempt.DurationMillis = finished.Sub(*attempt.StartedAt).Milliseconds()
					}
				}
			}
			continue
		}
		if event.Stage == "" || (stage != "" && event.Stage != stage) {
			continue
		}
		attempts := byStage[event.Stage]
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
					event.Stage,
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
		byStage[event.Stage] = attempts
	}
	return byStage
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
