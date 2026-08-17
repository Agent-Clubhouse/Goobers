package readservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readprobe"
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

// These are the canonical attempt populations behind Insight metrics.
const (
	StagePopulationAttempts        StagePopulation = "attempts"
	StagePopulationMeasured        StagePopulation = "measured"
	StagePopulationTokenMeasured   StagePopulation = "token-measured"
	StagePopulationPremiumMeasured StagePopulation = "premium-measured"
	StagePopulationCostMeasured    StagePopulation = "cost-measured"
	StagePopulationRetryWaste      StagePopulation = "retry-waste"
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
	Transcript(context.Context, string, uint64) (TranscriptContent, error)
	RunTranscripts(context.Context, string, string) ([]TranscriptContent, error)
	RunSpans(context.Context, string) ([]rollup.SpanSummary, error)
	RunTelemetryStageAttempts(context.Context, string) ([]rollup.StageAttempt, error)
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
	Gaggle            string
	Workflow          string
	Stage             string
	Outcome           OutcomeFilter
	StagePopulation   StagePopulation
	Phase             journal.RunPhase
	Trigger           journal.TriggerKind
	Since             time.Time
	Until             time.Time
	Limit             int
	Cursor            string
	LatestPerWorkflow bool

	// ShowNoWork includes routine no-work completions (#2188) — a run that
	// touched exactly one stage and that stage's terminal status was
	// apiv1.ResultNoWork. False (the default) hides them, since a live instance
	// on a ~60s schedule cadence produces far more of these than runs an
	// operator actually cares about; explicitly setting this to true is the
	// escape hatch, not a second filter value, so there is no way to ask for
	// no-work runs ONLY.
	ShowNoWork bool

	// OrderByActivity sorts and filters on the run's last journal event rather
	// than its start (#1777).
	//
	// Additive: false is the existing behaviour, so no current caller changes
	// meaning by this field existing. Since/Until follow the axis — on this one
	// they bound last activity, which is what makes "runs active in the last 24h"
	// expressible at all.
	//
	// Only the read-model path serves it. The journal-derived paths order by
	// start and would have to sort every candidate to answer it, which is the
	// unbounded shape the read model exists to remove -- so a request carrying
	// this without a read model is refused rather than served slowly.
	OrderByActivity bool
}

// RunList is one deterministic page of run summaries.
type RunList struct {
	ReadStateEnvelope
	Runs             []RunSummary          `json:"runs"`
	WorkflowActivity []WorkflowRunActivity `json:"workflowActivity,omitempty"`
	NextCursor       string                `json:"nextCursor,omitempty"`
}

// WorkflowRunActivity is the current durable active-run count for one workflow.
type WorkflowRunActivity struct {
	Gaggle     string `json:"gaggle"`
	Workflow   string `json:"workflow"`
	ActiveRuns int    `json:"activeRuns"`
}

// RunSummary is the journal-derived diagnostic summary shared by run lists and
// run detail.
type RunSummary struct {
	ID              string           `json:"id"`
	Workflow        string           `json:"workflow"`
	WorkflowVersion int              `json:"workflowVersion"`
	WorkflowDigest  string           `json:"workflowDigest,omitempty"`
	Gaggle          string           `json:"gaggle"`
	Trigger         journal.Trigger  `json:"trigger"`
	Phase           journal.RunPhase `json:"phase"`
	Terminal        bool             `json:"terminal"`
	CurrentStage    string           `json:"currentStage,omitempty"`
	StartedAt       time.Time        `json:"startedAt"`
	FinishedAt      *time.Time       `json:"finishedAt,omitempty"`
	DurationMillis  int64            `json:"durationMillis"`
	LastActivityAt  time.Time        `json:"lastActivityAt"`
	// Stale is true only for a running run when both its last activity and the
	// daemon scheduler heartbeat are older than runner.livenessTimeout.
	Stale            bool   `json:"stale"`
	LastSeq          uint64 `json:"lastSeq"`
	RepassCount      int    `json:"repassCount"`
	RetryCount       int    `json:"retryCount"`
	PolicyRetryCount int    `json:"policyRetryCount"`
	InfraRetryCount  int    `json:"infraRetryCount"`
	// NoWork is true for a completed run that touched exactly one stage and
	// that stage's terminal status was apiv1.ResultNoWork (#2188) — a routine
	// schedule tick that found nothing to do, as opposed to a genuine
	// single-stage workflow that did real work.
	NoWork        bool               `json:"noWork"`
	Operator      OperatorRunSummary `json:"operator"`
	Stages        []string           `json:"-"`
	stageAttempts map[string][]StageAttempt
}

// OperatorRunSummary answers the operational questions that otherwise require
// correlating the journal, claim ledger, and artifact blobs by hand.
type OperatorRunSummary struct {
	Issue              *OperatorIssue       `json:"issue,omitempty"`
	CurrentStage       string               `json:"currentStage,omitempty"`
	LastHeartbeatAt    *time.Time           `json:"lastHeartbeatAt,omitempty"`
	HeartbeatAgeMillis *int64               `json:"heartbeatAgeMillis,omitempty"`
	Liveness           string               `json:"liveness"`
	Trajectory         string               `json:"trajectory"`
	PullRequest        *journal.ExternalRef `json:"pullRequest,omitempty"`
	PROpenerStage      string               `json:"prOpenerStage,omitempty"`
	Claim              OperatorClaim        `json:"claim"`
	LatestError        *journal.ErrorDetail `json:"latestError,omitempty"`
	Review             *OperatorReview      `json:"review,omitempty"`
	NextTransition     string               `json:"nextTransition,omitempty"`
	PotentialBlockers  []string             `json:"potentialBlockers"`
}

type OperatorIssue struct {
	Number string `json:"number"`
	Title  string `json:"title,omitempty"`
}

type OperatorClaim struct {
	LeaseStatus    string     `json:"leaseStatus"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	ProviderMarker string     `json:"providerMarker"`
}

type OperatorReview struct {
	Verdict   string `json:"verdict"`
	Rationale string `json:"rationale,omitempty"`
}

// RunDetail includes the immutable graph pin and structured escalation cause.
// GraphStatus is "pinned" for current runs and "unavailable" for journals that
// predate graph snapshots.
type RunDetail struct {
	ReadStateEnvelope
	RunSummary
	Graph       *workflow.Graph  `json:"graph,omitempty"`
	GraphStatus string           `json:"graphStatus"`
	Escalation  *EscalationCause `json:"escalation,omitempty"`
	Outcome     *RunOutcome      `json:"outcome,omitempty"`
	// Transitions is the run's exact executed workflow-graph transition
	// history (#1427) — never inferred from "both endpoint nodes were
	// visited", which is what let the portal highlight an untaken repass
	// edge (#1430). TransitionsStatus mirrors GraphStatus's own vocabulary:
	// "projected" when Transitions is authoritative (even if empty — a
	// freshly-started run has none yet), "unavailable" for a run predating a
	// pinned graph snapshot.
	Transitions       []RunTransition `json:"transitions,omitempty"`
	TransitionsStatus string          `json:"transitionsStatus"`
}

// RunTransition is one executed transition in a run's workflow graph, the
// API-facing projection of readmodel.TransitionRow (kept distinct from it
// deliberately: RunDetail's JSON contract must not change shape just because
// the internal projection's field set does).
type RunTransition struct {
	Branch     int    `json:"branch"`
	Occurrence int    `json:"occurrence"`
	Seq        uint64 `json:"seq"`
	Source     string `json:"source"`
	Target     string `json:"target,omitempty"`
	Verdict    string `json:"verdict,omitempty"`
	Terminal   bool   `json:"terminal,omitempty"`
	Status     string `json:"status,omitempty"`
	Repass     bool   `json:"repass,omitempty"`
}

func runTransitionsFrom(rows []readmodel.TransitionRow) []RunTransition {
	if rows == nil {
		return nil
	}
	out := make([]RunTransition, len(rows))
	for i, row := range rows {
		out[i] = RunTransition{
			Branch: row.Branch, Occurrence: row.Occurrence, Seq: row.Seq,
			Source: row.Source, Target: row.Target, Verdict: row.Verdict,
			Terminal: row.Terminal, Status: row.TerminalStatus, Repass: row.Repass,
		}
	}
	return out
}

// RunOutcome projects the business decision a completed run reached — the
// second axis distinct from Phase (issue #851). Phase/Terminal answer "did
// the machinery work" (the execution axis #849 fixed); Outcome answers "of a
// completed run, what did it decide" (e.g. merge-review's merged / declined
// to merge). Only present when Phase == journal.PhaseCompleted; non-nil but
// all-empty for a completed run whose path evaluated no gate at all (a
// purely deterministic workflow) — the axis is still meaningful, it just has
// no gate decision to report, which a nil RunOutcome could not distinguish
// from "not computed."
//
// Deliberately not a fixed enum: Verdict/Target are the terminal gate's own
// values (per-workflow vocabulary), matching #851's explicit implementation
// latitude rather than inventing a cross-workflow taxonomy. Rollup/dashboard
// propagation and the curated no-work-run disposition are intentionally left
// as follow-up work — this is the read-model projection only.
type RunOutcome struct {
	// Gate is the last gate decision before completion. Empty when the run
	// completed with no gate decision.
	Gate string `json:"gate,omitempty"`
	// Verdict is that gate's decision.
	Verdict string `json:"verdict,omitempty"`
	// Target is the branch/state the gate selected.
	Target string `json:"target,omitempty"`
	// CausalEventSeq is the deciding gate event's sequence number.
	CausalEventSeq uint64 `json:"causalEventSeq,omitempty"`
}

// EscalationCause projects the durable event that selected escalation.
type EscalationCause struct {
	Selector       EscalationSelector     `json:"selector"`
	SelectedBranch string                 `json:"selectedBranch,omitempty"`
	RepassCount    int                    `json:"repassCount"`
	RetryCount     int                    `json:"retryCount"`
	TerminalReason string                 `json:"terminalReason,omitempty"`
	CausalEventSeq uint64                 `json:"causalEventSeq,omitempty"`
	Remediation    *RemediationEscalation `json:"remediation,omitempty"`
}

// RemediationEscalation describes what the PR remediation workflow actually
// attempted before selecting escalation.
type RemediationEscalation struct {
	Outcome         string   `json:"outcome"`
	Attempted       bool     `json:"attempted"`
	AttemptedCauses []string `json:"attemptedCauses,omitempty"`
}

// EscalationSelector identifies the gate or condition responsible.
type EscalationSelector struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// EventList is the complete durable event ledger for one run.
type EventList struct {
	ReadStateEnvelope
	RunID  string     `json:"runId"`
	Events []RunEvent `json:"events"`
}

// RunEvent exposes the shared event envelope. Type-specific fields are only
// populated for the schema this build owns; Raw retains an unknown event's
// complete scrubbed JSON for forward-compatible inspection.
type RunEvent struct {
	Schema              string                  `json:"schema"`
	Seq                 uint64                  `json:"seq"`
	Type                journal.EventType       `json:"type"`
	Branch              int                     `json:"branch"`
	Time                time.Time               `json:"time"`
	KnownSchema         bool                    `json:"knownSchema"`
	Category            RunEventCategory        `json:"category"`
	ReplayChapter       bool                    `json:"replayChapter"`
	Stage               string                  `json:"stage,omitempty"`
	Attempt             int                     `json:"attempt,omitempty"`
	AttemptClass        string                  `json:"attemptClass,omitempty"`
	Gate                string                  `json:"gate,omitempty"`
	Verdict             string                  `json:"verdict,omitempty"`
	Target              string                  `json:"target,omitempty"`
	Escalated           bool                    `json:"escalated,omitempty"`
	Status              string                  `json:"status,omitempty"`
	Actor               string                  `json:"actor,omitempty"`
	Action              string                  `json:"action,omitempty"`
	Decision            string                  `json:"decision,omitempty"`
	Rationale           string                  `json:"rationale,omitempty"`
	Complete            bool                    `json:"complete,omitempty"`
	InstructionAddendum string                  `json:"instructionAddendum,omitempty"`
	WorkflowVersion     int                     `json:"workflowVersion,omitempty"`
	WorkflowDigest      string                  `json:"workflowDigest,omitempty"`
	Outputs             map[string]any          `json:"outputs,omitempty"`
	Artifacts           []ArtifactMetadata      `json:"artifacts,omitempty"`
	Artifact            *ArtifactMetadata       `json:"artifact,omitempty"`
	Name                string                  `json:"name,omitempty"`
	ExternalRef         *journal.ExternalRef    `json:"externalRef,omitempty"`
	Error               *journal.ErrorDetail    `json:"error,omitempty"`
	Redaction           *journal.RedactionInfo  `json:"redaction,omitempty"`
	Runner              map[string]any          `json:"runner,omitempty"`
	Workflow            string                  `json:"workflow,omitempty"`
	RunID               string                  `json:"runId,omitempty"`
	Reason              string                  `json:"reason,omitempty"`
	Parallel            string                  `json:"parallel,omitempty"`
	BranchName          string                  `json:"branchName,omitempty"`
	BranchStatus        journal.BranchStatus    `json:"branchStatus,omitempty"`
	Completeness        []journal.BranchOutcome `json:"completeness,omitempty"`
	Raw                 json.RawMessage         `json:"raw,omitempty"`
	JournalEvent        *journal.Event          `json:"-"`
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
	ReadStateEnvelope
	RunID    string         `json:"runId"`
	Stage    string         `json:"stage"`
	Attempts []StageAttempt `json:"attempts"`
}

// StageAttempt is one durable stage traversal.
type StageAttempt struct {
	// ID is opaque and remains stable when a live attempt completes.
	ID string `json:"id"`
	// Visit groups retries under the same per-stage graph traversal.
	Visit  int    `json:"visit"`
	Number int    `json:"number"`
	Class  string `json:"class"`
	Status string `json:"status"`
	// Model is the requested/selected model (e.g. "auto") indexed from the
	// attempt's agent-invocation span, when the telemetry rollup has ingested
	// it. Empty when telemetry is unavailable or the attempt has no matching
	// span yet.
	Model          string               `json:"model,omitempty"`
	StartedSeq     uint64               `json:"startedSeq,omitempty"`
	FinishedSeq    uint64               `json:"finishedSeq,omitempty"`
	StartedAt      *time.Time           `json:"startedAt,omitempty"`
	FinishedAt     *time.Time           `json:"finishedAt,omitempty"`
	DurationMillis int64                `json:"durationMillis"`
	Outputs        map[string]any       `json:"outputs,omitempty"`
	Artifacts      []ArtifactMetadata   `json:"artifacts"`
	Error          *journal.ErrorDetail `json:"error,omitempty"`
	branch         int
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
func (s *Local) listRunsUnannotated(ctx context.Context, options RunListOptions) (RunList, error) {
	if options.LatestPerWorkflow {
		if options.Stage != "" ||
			options.Outcome != "" ||
			options.StagePopulation != "" ||
			options.Phase != "" ||
			options.Trigger != "" ||
			!options.Since.IsZero() ||
			!options.Until.IsZero() ||
			options.Limit != 0 ||
			options.Cursor != "" {
			return RunList{}, fmt.Errorf("%w: latest-per-workflow only accepts gaggle and workflow filters", ErrInvalidArgument)
		}
		// The read-model aggregate answers the whole page — outcomes AND activity
		// — in one indexed query with zero journal opens (#1891). It replaces the
		// window function, the backwards terminal walk, and the separate activity
		// call below, so it returns directly rather than falling through to them.
		if s.readModelReads {
			return s.listLatestWorkflowOutcomesFromReadModel(ctx, options)
		}
		var (
			result RunList
			err    error
		)
		if s.ReadMode() != ReadModeAuthoritative && s.sources.Telemetry != nil {
			result, err = s.listLatestWorkflowOutcomesIndexed(ctx, options)
		} else {
			result, err = s.listLatestWorkflowOutcomesScanning(ctx, options)
		}
		if err != nil {
			return RunList{}, err
		}
		result.WorkflowActivity, err = s.workflowRunActivity(ctx, options)
		if err != nil {
			return RunList{}, err
		}
		return result, nil
	}

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
	if options.StagePopulation != "" &&
		!telemetryStagePopulation(options.StagePopulation) &&
		options.Stage == "" {
		return RunList{}, fmt.Errorf("%w: stage population requires a stage", ErrInvalidArgument)
	}
	if telemetryStagePopulation(options.StagePopulation) && s.sources.Telemetry == nil {
		return RunList{}, ErrTelemetryUnavailable
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

	// An enabled read model owns every daemon list request. Unsupported filter
	// combinations are refused by its closed set rather than falling through to
	// a journal scan.
	if s.readModelReads {
		return s.listRunsFromReadModel(ctx, options, cursor, limit)
	}
	// The activity axis is read-model only, and unserved is a REFUSAL rather
	// than a fallback (#1777).
	//
	// The journal-derived paths order by start. Handed OrderByActivity they
	// would quietly return start-ordered rows — a page that looks correct, is
	// sorted wrongly, and gives an operator watching an attention list exactly
	// the answer the feature exists to fix. Sorting the candidates instead would
	// mean sorting all of them, which is the unbounded shape the read model
	// removes.
	//
	// So say so. A caller that gets this can retry without the axis, or wait for
	// the projection; both beat being lied to about the order.
	if options.OrderByActivity {
		return RunList{}, fmt.Errorf(
			"%w: ordering by last activity requires the read model (mode %s)",
			ErrBoundedReadUnavailable, s.ReadMode())
	}
	if s.ReadMode() != ReadModeAuthoritative && s.sources.Telemetry != nil {
		return s.listRunsIndexed(ctx, options, cursor, limit)
	}
	// The journal-scanning path is now REACHED DELIBERATELY, not fallen into
	// (#1933, §11.2).
	//
	// It used to be entered silently whenever the indexed sources were absent,
	// which made "the read path is bounded" a property of one topology rather
	// than of the service — and gave an operator on the standalone dashboard
	// O(total history) per request with nothing to indicate it.
	//
	// Building and degraded modes refuse instead. A caller that gets this error
	// can retry, show a progress banner, or opt into the scan explicitly; all of
	// those beat a request that appears to work and takes minutes.
	if !s.boundedReadAvailable() {
		return RunList{}, fmt.Errorf("%w (mode %s)", ErrBoundedReadUnavailable, s.ReadMode())
	}
	return s.listRunsScanning(ctx, options, cursor, limit)
}

func (s *Local) listLatestWorkflowOutcomesScanning(ctx context.Context, options RunListOptions) (RunList, error) {
	summaries, err := s.runSummaries(ctx, false)
	if err != nil {
		return RunList{}, err
	}
	return latestWorkflowOutcomes(summaries, options), nil
}

func (s *Local) listLatestWorkflowOutcomesIndexed(ctx context.Context, options RunListOptions) (RunList, error) {
	// No reconcile here. It used to run on this path and reach IngestRun ->
	// WithPruneProtection -> acquireJournalLock, which is how a READ came to
	// create .lock files in all 40,665 run directories on the live instance —
	// including the 10,906 with no run.yaml that can never be ingested (#1924,
	// §6.3). Completeness is now the repair sweep's job, off the request path.
	refs, err := s.sources.Telemetry.LatestWorkflowRunRefs(ctx, options.Gaggle, options.Workflow)
	if err != nil {
		return RunList{}, err
	}

	observedAt := s.now().UTC()
	summaries := make([]RunSummary, 0, len(refs))
	for _, ref := range refs {
		summary, err := s.latestTerminalWorkflowOutcome(ctx, ref, observedAt)
		if err != nil {
			return RunList{}, err
		}
		if summary != nil {
			summaries = append(summaries, *summary)
		}
	}
	return latestWorkflowOutcomes(summaries, options), nil
}

func (s *Local) latestTerminalWorkflowOutcome(
	ctx context.Context,
	workflowRef rollup.WorkflowRunRef,
	observedAt time.Time,
) (*RunSummary, error) {
	refs := []rollup.RunRef{workflowRef.RunRef}
	for {
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			run, err := s.openRun(ref.RunID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return nil, err
			}
			summary, err := summarizeRunForStage(run, observedAt, "")
			if err != nil {
				return nil, fmt.Errorf("summarize run %q: %w", ref.RunID, err)
			}
			if summary.Terminal {
				return &summary, nil
			}
		}

		cursor := refs[len(refs)-1]
		var err error
		refs, err = s.sources.Telemetry.RunRefPage(
			ctx,
			rollup.RunListFilter{
				Gaggle:   workflowRef.Gaggle,
				Workflow: workflowRef.Workflow,
			},
			cursor.StartedAt,
			cursor.RunID,
			defaultRunLimit,
		)
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			return nil, nil
		}
	}
}

func latestWorkflowOutcomes(summaries []RunSummary, options RunListOptions) RunList {
	runs := make([]RunSummary, 0, len(summaries))
	seen := make(map[string]struct{}, len(summaries))
	for _, summary := range summaries {
		if !summary.Terminal ||
			(options.Gaggle != "" && summary.Gaggle != options.Gaggle) ||
			(options.Workflow != "" && summary.Workflow != options.Workflow) {
			continue
		}
		key := summary.Gaggle + "\x00" + summary.Workflow
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		runs = append(runs, summary)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	return RunList{Runs: runs}
}

func (s *Local) workflowRunActivity(
	ctx context.Context,
	options RunListOptions,
) ([]WorkflowRunActivity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	counts, err := s.activeRunCounts()
	if err != nil {
		return nil, err
	}
	activity := make([]WorkflowRunActivity, 0, len(counts))
	for identity, activeRuns := range counts {
		if (options.Gaggle != "" && identity.Gaggle != options.Gaggle) ||
			(options.Workflow != "" && identity.Workflow != options.Workflow) {
			continue
		}
		activity = append(activity, WorkflowRunActivity{
			Gaggle:     identity.Gaggle,
			Workflow:   identity.Workflow,
			ActiveRuns: activeRuns,
		})
	}
	sort.Slice(activity, func(i, j int) bool {
		if activity[i].Gaggle == activity[j].Gaggle {
			return activity[i].Workflow < activity[j].Workflow
		}
		return activity[i].Gaggle < activity[j].Gaggle
	})
	return activity, ctx.Err()
}

// runMatches reports whether summary satisfies every option filter. The
// summary is always journal-hydrated and therefore authoritative, so this is
// the single predicate both the scanning and indexed paths apply — a lagging
// index status can never wrongly include or hide a run because Phase and
// stageAttempts here come from the journal, not the index.
func (s *Local) runMatches(summary RunSummary, options RunListOptions) bool {
	journalPopulation := options.StagePopulation
	if telemetryStagePopulation(journalPopulation) {
		journalPopulation = ""
	}
	switch {
	case !options.ShowNoWork && summary.NoWork:
		return false
	case options.Gaggle != "" && summary.Gaggle != options.Gaggle:
		return false
	case options.Workflow != "" && summary.Workflow != options.Workflow:
		return false
	case options.Stage != "" && !containsString(summary.Stages, options.Stage):
		return false
	case (options.Outcome != "" ||
		(options.StagePopulation != "" && !telemetryStagePopulation(options.StagePopulation))) &&
		!summary.Terminal:
		return false
	case options.Stage != "" &&
		(options.Outcome != "" || journalPopulation != "") &&
		!matchesStageAttempt(summary.stageAttempts[options.Stage], options.Outcome, journalPopulation):
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

// candidateRunStatuses translates options.Phase / options.Outcome into the
// runs.status values a matching journal could possibly carry, so
// listRunsIndexed can ask the index to skip candidates that runMatches would
// reject anyway (DASH-18 continued: an unfiltered or stage-filtered list
// still opens one journal per page row, but a Phase/Outcome-filtered list —
// e.g. the Overview's "active runs" query — no longer has to open every
// terminal run in history just to find the rare non-terminal ones, which is
// what made ListRuns effectively hang once an instance accumulated tens of
// thousands of runs).
//
// narrowed reports whether any narrowing applies at all; when false, the
// caller must not touch RunListFilter.Statuses (nil means "no filter", not
// "match nothing"). possible reports whether the combination can match any
// row; when narrowed is true and possible is false, the caller can return an
// empty page without any index or journal I/O.
//
// This is a pure optimization: runMatches still re-checks every candidate
// against its hydrated journal, so a stale or not-yet-reconciled index row
// can only cost an extra journal open, never wrongly hide a run.
func candidateRunStatuses(options RunListOptions) (statuses []string, narrowed, possible bool) {
	var set map[string]struct{}
	intersect := func(next []string) {
		if set == nil {
			set = make(map[string]struct{}, len(next))
			for _, status := range next {
				set[status] = struct{}{}
			}
			return
		}
		for status := range set {
			if !containsString(next, status) {
				delete(set, status)
			}
		}
	}
	if options.Phase != "" {
		intersect(phaseStatuses(options.Phase))
		narrowed = true
	}
	// Outcome pushdown is only valid when it is being applied directly to the
	// run's own phase — runMatches applies a Stage-scoped Outcome to that
	// stage's attempts instead, which the runs.status column cannot answer.
	if options.Stage == "" && options.Outcome != "" {
		intersect(outcomeStatuses(options.Outcome))
		narrowed = true
	}
	if !narrowed {
		return nil, false, true
	}
	statuses = make([]string, 0, len(set))
	for status := range set {
		statuses = append(statuses, status)
	}
	return statuses, true, len(statuses) > 0
}

// phaseStatuses returns the runs.status values consistent with phase. A
// non-terminal run has no recorded status (see insertRun), represented here
// by the empty string.
func phaseStatuses(phase journal.RunPhase) []string {
	if phase == journal.PhaseRunning {
		return []string{""}
	}
	return []string{string(phase)}
}

// outcomeStatuses returns the runs.status values consistent with a run-scoped
// (non-Stage) outcome filter.
func outcomeStatuses(outcome OutcomeFilter) []string {
	switch outcome {
	case OutcomeFinished:
		return []string{
			string(journal.PhaseCompleted),
			string(journal.PhaseFailed),
			string(journal.PhaseAborted),
			string(journal.PhaseEscalated),
		}
	case OutcomeTerminal:
		return []string{string(journal.PhaseCompleted), string(journal.PhaseFailed)}
	case OutcomeSuccess:
		return []string{string(journal.PhaseCompleted)}
	case OutcomeFailure:
		return []string{string(journal.PhaseFailed)}
	case OutcomeOther:
		return []string{string(journal.PhaseAborted), string(journal.PhaseEscalated)}
	default:
		return nil
	}
}

func attemptStageFor(options RunListOptions) string {
	if options.Stage != "" &&
		(options.Outcome != "" ||
			(options.StagePopulation != "" && !telemetryStagePopulation(options.StagePopulation))) {
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
// authoritative. Completeness is guaranteed by the repair sweep
// (internal/readmodel/repair), not by this function — see the deleted-
// reconcileIndex note below for why an inline reconcile on the request path
// was the wrong place to put that guarantee — so a run present on disk but
// absent from the index (migrated/imported/still in flight) is never silently
// hidden.
func (s *Local) listRunsIndexed(ctx context.Context, options RunListOptions, cursor *runCursor, limit int) (RunList, error) {
	// No reconcile here — see listLatestWorkflowOutcomesIndexed. A read does not
	// write.

	statuses, narrowed, possible := candidateRunStatuses(options)
	if narrowed && !possible {
		// Phase and Outcome together can't match anything (e.g. Phase=running
		// with Outcome=success): every candidate would fail runMatches, so
		// skip the index round-trip and journal opens entirely.
		return paginateRuns(nil, limit)
	}

	observedAt := s.now().UTC()
	attemptStage := attemptStageFor(options)
	filter := rollup.RunListFilter{
		Gaggle:      options.Gaggle,
		Workflow:    options.Workflow,
		TriggerKind: string(options.Trigger),
		Since:       options.Since,
		Until:       options.Until,
		Statuses:    statuses,
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
		refs, err := s.sources.Telemetry.RunRefPage(ctx, filter, keyStarted, keyRunID, pageSize)
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
			matchesUsage, err := s.matchesTelemetryPopulation(ctx, ref.RunID, options)
			if err != nil {
				return RunList{}, err
			}
			if !matchesUsage {
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
	if err := s.decorateOperatorClaims(ctx, kept, observedAt); err != nil {
		return RunList{}, err
	}
	return paginateRuns(kept, limit)
}

// reconcileIndex and its two test-seam observers are DELETED (#1924, §6.3).
//
// It backfilled any on-disk run absent from the telemetry index so a list could
// not hide a run — a real requirement, solved in the wrong place. It ran on the
// HTTP list path and reached IngestRun → WithPruneProtection →
// acquireJournalLock, which is why all 40,665 run directories on the live
// instance contain a `.lock` file, including the 10,906 with no run.yaml that
// can never be ingested. Every one was created by a read.
//
// Its incremental bound did not hold either. Skipping a run root whose mtime had
// not advanced is correct reasoning and useless in practice: every new run bumps
// its parent's mtime, so on a live instance the root is always dirty and the
// scan read all 40,665 entries every pass. A bound that only holds when nothing
// is happening is not a bound.
//
// Completeness is now the repair sweep's (internal/readmodel/repair): a fixed
// I/O budget walked continuously with a durable cursor, off the request path,
// never taking a journal lock, and bidirectional — it also removes projected
// rows whose journal has vanished, which reconcile could not do at all.

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
	if err := s.decorateOperatorClaims(ctx, summaries, observedAt); err != nil {
		return nil, err
	}
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
func (s *Local) getRunUnannotated(ctx context.Context, runID string) (RunDetail, error) {
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
	transitions, transitionsStatus := readmodel.ProjectTransitions(recordEvents(run.records), graph)
	return RunDetail{
		RunSummary:        summary,
		Graph:             graph,
		GraphStatus:       status,
		Escalation:        escalation,
		Outcome:           runOutcome(summary, run.records),
		Transitions:       runTransitionsFrom(transitions),
		TransitionsStatus: transitionsStatus,
	}, nil
}

// recordEvents unwraps a run's raw-preserving event records into the plain
// events readmodel.ProjectTransitions folds over.
func recordEvents(records []journal.EventRecord) []journal.Event {
	events := make([]journal.Event, len(records))
	for i, record := range records {
		events[i] = record.Event
	}
	return events
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
func (s *Local) runEventsUnannotated(ctx context.Context, runID string) (EventList, error) {
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
func (s *Local) stageAttemptsUnannotated(ctx context.Context, runID, stage string) (AttemptList, error) {
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

	attempts := collectStageAttempts(run.identity.RunID, run.records, indexArtifacts(run.records), stage)[stage]
	if telemetryAttempts, err := s.telemetryStageAttempts(ctx, run.identity.RunID); err == nil {
		attachStageAttemptModels(attempts, stage, telemetryAttempts)
	}
	return AttemptList{RunID: run.identity.RunID, Stage: stage, Attempts: attempts}, nil
}

// attachStageAttemptModels fills in Model on each attempt of stage from the
// matching rollup-indexed stage attempt, correlated by durable traversal
// (Visit) and attempt number. Best-effort: an attempt with no telemetry match
// simply keeps an empty Model.
func attachStageAttemptModels(attempts []StageAttempt, stage string, telemetryAttempts []rollup.StageAttempt) {
	for i := range attempts {
		for _, ta := range telemetryAttempts {
			if ta.Stage == stage && ta.Traversal == attempts[i].Visit && ta.Attempt == attempts[i].Number && ta.Model != "" {
				attempts[i].Model = ta.Model
				break
			}
		}
	}
}

// telemetryStageAttempts returns rollup-ingested stage attempts (each
// carrying its indexed requested model, when present) for runID. A missing
// telemetry database is a valid empty result, matching RunSpans' contract:
// model provenance is informational and must never make StageAttempts fail.
func (s *Local) telemetryStageAttempts(ctx context.Context, runID string) ([]rollup.StageAttempt, error) {
	empty := []rollup.StageAttempt{}
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
	return db.StageAttempts(ctx, runID)
}

// RunTelemetryStageAttempts returns rollup-ingested stage attempts (with each
// attempt's indexed requested model) for the whole run, across every stage.
// Best-effort, same contract as RunSpans: a missing telemetry database is a
// valid empty result.
func (s *Local) RunTelemetryStageAttempts(ctx context.Context, runID string) ([]rollup.StageAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.openRun(runID); err != nil {
		return nil, err
	}
	attempts, err := s.telemetryStageAttempts(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
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

// Transcript returns the verified transcript recorded at one durable sequence.
func (s *Local) Transcript(ctx context.Context, runID string, seq uint64) (TranscriptContent, error) {
	if err := ctx.Err(); err != nil {
		return TranscriptContent{}, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return TranscriptContent{}, err
	}
	for _, record := range run.records {
		if record.Event.Seq != seq {
			continue
		}
		recordedStage, ok := transcriptStage(record.Event, runID, "")
		if !ok {
			break
		}
		return readTranscript(run, record.Event, recordedStage)
	}
	return TranscriptContent{}, fmt.Errorf("%w: transcript at seq %d", ErrNotFound, seq)
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
		recordedStage, ok := transcriptStage(event, runID, stage)
		if !ok {
			continue
		}
		transcript, err := readTranscript(run, event, recordedStage)
		if err != nil {
			return nil, err
		}
		transcripts = append(transcripts, transcript)
	}
	return transcripts, nil
}

func transcriptStage(event journal.Event, runID, stage string) (string, bool) {
	recordedStage := strings.TrimPrefix(event.Stage, runID+":")
	if !event.KnownSchema() ||
		event.Type != journal.EventSpanRecorded ||
		(event.Name != "transcript" && !strings.HasSuffix(event.Name, ".transcript")) ||
		(stage != "" && recordedStage != stage) {
		return "", false
	}
	return recordedStage, true
}

func readTranscript(run runRead, event journal.Event, recordedStage string) (TranscriptContent, error) {
	if event.Ref == nil {
		return TranscriptContent{}, fmt.Errorf(
			"transcript for stage %q at seq %d is unavailable: span event has no content reference",
			recordedStage,
			event.Seq,
		)
	}
	data, err := run.reader.SpanBytes(*event.Ref)
	if err != nil {
		return TranscriptContent{}, fmt.Errorf(
			"transcript for stage %q at seq %d is unavailable: %w",
			recordedStage,
			event.Seq,
			err,
		)
	}
	if len(data) == 0 {
		return TranscriptContent{}, fmt.Errorf(
			"transcript for stage %q at seq %d is unavailable: recorded content is empty",
			recordedStage,
			event.Seq,
		)
	}
	return TranscriptContent{
		Seq:   event.Seq,
		Stage: recordedStage,
		Name:  event.Name,
		Bytes: data,
	}, nil
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
	spans, err := db.Spans(ctx, runID)
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
	readprobe.RecordJournalOpen()
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
	operator := OperatorRunSummary{
		Trajectory:        "parked",
		Liveness:          "no-heartbeat",
		Claim:             OperatorClaim{LeaseStatus: "none", ProviderMarker: "not-recorded"},
		PotentialBlockers: []string{},
	}
	if run.identity.Trigger.Kind == journal.TriggerItem && run.identity.Trigger.Ref != "" {
		operator.Issue = &OperatorIssue{Number: run.identity.Trigger.Ref}
	}
	var lastHeartbeat time.Time
	providerClaimRecorded := false
	seenStages := make(map[string]struct{})
	lastStageStatus := make(map[string]string)
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
		if event.Error != nil {
			detail := *event.Error
			operator.LatestError = &detail
		}
		switch event.Type {
		case journal.EventStageHeartbeat:
			if event.Time.After(lastHeartbeat) {
				lastHeartbeat = event.Time
			}
		case journal.EventRunnerAnnotation:
			if queue, ok := readmodel.RunnerQueueStatus(event); ok {
				currentStage = queue
			}
			if suggestion, ok := readmodel.RunnerResetSuggestion(event); ok {
				currentStage = suggestion
			}
		case journal.EventRunResumed, journal.EventGateOverridden:
			phase = journal.PhaseRunning
			finishedAt = nil
			currentStage = event.Target
		case journal.EventStageStarted:
			currentStage = event.Stage
		case journal.EventStageFinished:
			if currentStage == event.Stage {
				currentStage = ""
			}
			lastStageStatus[event.Stage] = event.Status
			if title, ok := event.Outputs["title"].(string); ok && title != "" {
				if operator.Issue == nil {
					operator.Issue = &OperatorIssue{}
				}
				operator.Issue.Title = title
			}
		case journal.EventGateStarted:
			currentStage = event.Gate
		case journal.EventGateEvaluated:
			if currentStage == event.Gate {
				currentStage = ""
			}
			review := &OperatorReview{Verdict: event.Verdict}
			if event.Ref != nil {
				data, err := run.reader.ArtifactBytes(*event.Ref)
				if err != nil {
					operator.PotentialBlockers = append(operator.PotentialBlockers,
						fmt.Sprintf("review rationale unavailable: %v", err))
				} else {
					var verdict apiv1.Verdict
					if err := json.Unmarshal(data, &verdict); err != nil {
						operator.PotentialBlockers = append(operator.PotentialBlockers,
							fmt.Sprintf("review rationale is invalid: %v", err))
					} else {
						review.Rationale = strings.TrimSpace(verdict.Rationale)
						if review.Rationale == "" {
							review.Rationale = strings.TrimSpace(verdict.Summary)
						}
					}
				}
			}
			operator.Review = review
		case journal.EventRefTouched:
			if event.ExternalRef == nil {
				continue
			}
			switch event.ExternalRef.Kind {
			case "issue":
				if operator.Issue == nil {
					operator.Issue = &OperatorIssue{}
				}
				operator.Issue.Number = event.ExternalRef.ID
				operation, _ := event.Runner["operation"].(string)
				providerClaimRecorded = providerClaimRecorded || operation == "claim"
			case "pr":
				ref := *event.ExternalRef
				operator.PullRequest = &ref
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
			if !strings.HasPrefix(currentStage, "Workspace reset suggested:") {
				currentStage = ""
			}
		}
	}

	if phase == journal.PhaseRunning {
		if state, err := run.reader.State(); err == nil && state.LastSeq >= lastSeq && state.MachineState != "" {
			currentStage = state.MachineState
		}
		operator.CurrentStage = currentStage
		operator.Trajectory = operatorTrajectory(currentStage, phase)
		if !lastHeartbeat.IsZero() {
			heartbeat := lastHeartbeat
			operator.LastHeartbeatAt = &heartbeat
			age := max(observedAt.Sub(heartbeat).Milliseconds(), 0)
			operator.HeartbeatAgeMillis = &age
		}
		if phase != journal.PhaseRunning {
			operator.Liveness = "terminal"
		}
		if providerClaimRecorded {
			operator.Claim.ProviderMarker = "recorded"
		}
		if graph, status, err := pinnedGraph(run); err != nil {
			return RunSummary{}, err
		} else if status == "pinned" {
			for _, node := range graph.Nodes {
				if operatorTrajectory(node.ID, journal.PhaseRunning) == "open PR" {
					operator.PROpenerStage = node.ID
					break
				}
			}
		}
		if operator.PullRequest != nil {
			operator.PROpenerStage = ""
		}
		if phase == journal.PhaseRunning {
			if currentStage == "" {
				operator.NextTransition = "start the next workflow stage"
			} else {
				operator.NextTransition = "finish " + currentStage
			}
		}
		if operator.LatestError != nil {
			operator.PotentialBlockers = append(operator.PotentialBlockers,
				operator.LatestError.Code+": "+operator.LatestError.Message)
		}
		if operator.Review != nil && operator.Review.Verdict != "" &&
			operator.Review.Verdict != "pass" && operator.Review.Verdict != "approve" {
			operator.PotentialBlockers = append(operator.PotentialBlockers,
				"review "+operator.Review.Verdict+": "+operator.Review.Rationale)
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
	// A routine no-work tick is a completed run that touched exactly one
	// stage, and that stage's own terminal status was no-work — as opposed to
	// a multi-stage run that hit no-work partway, or a genuinely single-stage
	// workflow that succeeded.
	noWork := phase == journal.PhaseCompleted && len(lastStageStatus) == 1
	if noWork {
		for _, status := range lastStageStatus {
			noWork = status == string(apiv1.ResultNoWork)
		}
	}
	var stageAttempts map[string][]StageAttempt
	if attemptStage != "" {
		stageAttempts = collectStageAttempts(run.identity.RunID, run.records, artifactIndex{}, attemptStage)
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
		NoWork:           noWork,
		Operator:         operator,
		Stages:           stages,
		stageAttempts:    stageAttempts,
	}, nil
}

func operatorTrajectory(stage string, phase journal.RunPhase) string {
	if phase != journal.PhaseRunning {
		return "parked"
	}
	stage = strings.ToLower(stage)
	switch {
	case strings.Contains(stage, "review"):
		return "review"
	case strings.Contains(stage, "local-ci"), strings.Contains(stage, "local_ci"):
		return "local CI"
	case strings.Contains(stage, "push"):
		return "push"
	case strings.Contains(stage, "open-pr"), strings.Contains(stage, "open_pr"):
		return "open PR"
	case strings.Contains(stage, "poll"), strings.Contains(stage, "ci-status"):
		return "CI poll"
	case strings.Contains(stage, "close-out"), strings.Contains(stage, "close_out"):
		return "close-out"
	case strings.Contains(stage, "implement"):
		return "implementing"
	default:
		return "parked"
	}
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
	case StagePopulationAttempts,
		StagePopulationMeasured,
		StagePopulationTokenMeasured,
		StagePopulationPremiumMeasured,
		StagePopulationCostMeasured,
		StagePopulationRetryWaste:
		return true
	default:
		return false
	}
}

func telemetryStagePopulation(population StagePopulation) bool {
	switch population {
	case StagePopulationTokenMeasured,
		StagePopulationPremiumMeasured,
		StagePopulationCostMeasured,
		StagePopulationRetryWaste:
		return true
	default:
		return false
	}
}

func (s *Local) matchesTelemetryPopulation(ctx context.Context, runID string, options RunListOptions) (bool, error) {
	if !telemetryStagePopulation(options.StagePopulation) {
		return true, nil
	}
	attempts, err := s.sources.Telemetry.StageAttempts(ctx, runID)
	if err != nil {
		return false, err
	}
	return matchesTelemetryAttempts(attempts, options.Stage, options.StagePopulation), nil
}

func matchesTelemetryAttempts(attempts []rollup.StageAttempt, stage string, population StagePopulation) bool {
	type traversalKey struct {
		stage       string
		branch      int
		branchKnown bool
	}
	latest := make(map[traversalKey]int)
	for _, attempt := range attempts {
		key := traversalKey{
			stage:       attempt.Stage,
			branch:      attempt.Branch,
			branchKnown: attempt.BranchKnown,
		}
		if attempt.Traversal > latest[key] {
			latest[key] = attempt.Traversal
		}
	}
	for _, attempt := range attempts {
		if stage != "" && attempt.Stage != stage {
			continue
		}
		switch population {
		case StagePopulationTokenMeasured:
			if attempt.InputTokens != nil && attempt.OutputTokens != nil {
				return true
			}
		case StagePopulationPremiumMeasured:
			if attempt.CopilotPremiumRequests != nil {
				return true
			}
		case StagePopulationCostMeasured:
			if attempt.CostUSD != nil {
				return true
			}
		case StagePopulationRetryWaste:
			key := traversalKey{
				stage:       attempt.Stage,
				branch:      attempt.Branch,
				branchKnown: attempt.BranchKnown,
			}
			if attempt.Traversal < latest[key] {
				return true
			}
		}
	}
	return false
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
	if remediation := remediationEscalation(records); remediation != nil {
		cause.Remediation = remediation
	}
	remediationReason := remediationEscalationReason(records)
	terminalStage := successfulTerminalStage(records)
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if isEscalatingGateEvent(event, terminalStage) {
			cause.Selector = EscalationSelector{Kind: "gate", Name: event.Gate}
			cause.SelectedBranch = event.Verdict
			cause.TerminalReason = gateEscalationReason(event)
			if remediationReason != "" {
				cause.TerminalReason = remediationReason
			}
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

func remediationEscalation(records []journal.EventRecord) *RemediationEscalation {
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() || event.Type != journal.EventStageFinished ||
			!isRemediationCheckpointStage(event.Stage) || event.Status != string(apiv1.ResultSuccess) {
			continue
		}
		outcome, _ := event.Outputs["escalationOutcome"].(string)
		if outcome == "" {
			return nil
		}
		attempted, err := strconv.ParseBool(fmt.Sprint(event.Outputs["remediationAttempted"]))
		if err != nil {
			return nil
		}
		var causes []string
		for _, cause := range strings.Split(fmt.Sprint(event.Outputs["attemptedCauses"]), ",") {
			if cause = strings.TrimSpace(cause); cause != "" {
				causes = append(causes, cause)
			}
		}
		return &RemediationEscalation{Outcome: outcome, Attempted: attempted, AttemptedCauses: causes}
	}
	return nil
}

func remediationEscalationReason(records []journal.EventRecord) string {
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() || event.Type != journal.EventStageFinished ||
			!isRemediationCheckpointStage(event.Stage) || event.Status != string(apiv1.ResultSuccess) {
			continue
		}
		reason, _ := event.Outputs["escalationReason"].(string)
		return strings.TrimSpace(reason)
	}
	return ""
}

func isRemediationCheckpointStage(stage string) bool {
	switch stage {
	case "remediation-checkpoint", "park-escalated", "park-invalid-finding-responses", "park-infrastructure-failure":
		return true
	default:
		return false
	}
}

// runOutcome derives the #851 business-decision axis for a completed run
// from the last gate decision before completion — the same "walk backward
// for the decisive gate" approach escalationCause uses for the escalated
// case. Returns nil for a non-completed run.
func runOutcome(summary RunSummary, records []journal.EventRecord) *RunOutcome {
	if summary.Phase != journal.PhaseCompleted {
		return nil
	}
	records = currentLifecycleRecords(records)
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() ||
			(event.Type != journal.EventGateEvaluated && event.Type != journal.EventGateOverridden) {
			continue
		}
		return &RunOutcome{
			Gate:           event.Gate,
			Verdict:        event.Verdict,
			Target:         event.Target,
			CausalEventSeq: event.Seq,
		}
	}
	return &RunOutcome{}
}

func currentLifecycleRecords(records []journal.EventRecord) []journal.EventRecord {
	for i := len(records) - 1; i >= 0; i-- {
		event := records[i].Event
		if !event.KnownSchema() {
			continue
		}
		switch event.Type {
		case journal.EventRunResumed:
			return records[i+1:]
		case journal.EventGateOverridden:
			return records[i:]
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
	category, replayChapter := classifyRunEvent(event)
	projected := RunEvent{
		Schema:        event.Schema,
		Seq:           event.Seq,
		Type:          event.Type,
		Branch:        event.Branch,
		Time:          event.Time,
		KnownSchema:   event.KnownSchema(),
		Category:      category,
		ReplayChapter: replayChapter,
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
	projected.Action = event.Action
	projected.Decision = event.Decision
	projected.Rationale = event.Rationale
	projected.Complete = event.Complete
	projected.InstructionAddendum = event.InstructionAddendum
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
	projected.Parallel = event.Parallel
	projected.BranchName = event.BranchName
	projected.BranchStatus = event.BranchStatus
	projected.Completeness = event.Completeness
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
	runID string,
	records []journal.EventRecord,
	artifacts artifactIndex,
	stage string,
) map[string][]StageAttempt {
	byStage := make(map[string][]StageAttempt)
	visits := make(map[string]stageVisitState)
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
		case journal.EventStageRerunRequested:
			visit := visits[event.Stage]
			visit.humanRequested = true
			visits[event.Stage] = visit
		case journal.EventStageStarted:
			attempts = append(attempts, newStageAttempt(runID, event, visits, true))
		case journal.EventArtifactRecorded:
			if event.Ref == nil {
				continue
			}
			if i := matchingOpenAttempt(attempts, event.Attempt, event.AttemptClass, event.Branch); i >= 0 {
				if metadata, ok := artifacts.bySeq[event.Seq]; ok {
					attempts[i].Artifacts = append(attempts[i].Artifacts, metadata)
				}
			}
		case journal.EventError:
			if event.Error == nil || event.Error.Code != "executor_error" {
				continue
			}
			i := matchingOpenAttempt(attempts, event.Attempt, event.AttemptClass, event.Branch)
			if i < 0 {
				attempts = append(attempts, newStageAttempt(runID, event, visits, false))
				i = len(attempts) - 1
			}
			finishAttempt(&attempts[i], event, string(apiv1.ResultFailure), nil, event.Error)
		case journal.EventStageFinished:
			i := matchingOpenAttempt(attempts, event.Attempt, event.AttemptClass, event.Branch)
			if i < 0 {
				attempts = append(attempts, newStageAttempt(runID, event, visits, false))
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

type stageVisitState struct {
	ordinal        int
	humanVisit     bool
	humanRequested bool
}

func newStageAttempt(
	runID string,
	event journal.Event,
	visits map[string]stageVisitState,
	started bool,
) StageAttempt {
	visit := visits[event.Stage]
	switch event.AttemptClass {
	case "":
		visit.ordinal++
		visit.humanVisit = false
		visit.humanRequested = false
	case journal.AttemptHuman:
		// Legacy journals can lack stage.rerun.requested. Treat their first
		// consecutive human attempt as the visit boundary.
		if visit.humanRequested || !visit.humanVisit || visit.ordinal == 0 {
			visit.ordinal++
		}
		visit.humanVisit = true
		visit.humanRequested = false
	default:
		if visit.ordinal == 0 {
			visit.ordinal = 1
		}
	}
	visits[event.Stage] = visit
	attempt := StageAttempt{
		ID:        stageAttemptID(runID, event.Branch, event.Stage, event.Seq),
		Visit:     visit.ordinal,
		Number:    event.Attempt,
		Class:     attemptClass(event.AttemptClass),
		Artifacts: []ArtifactMetadata{},
		branch:    event.Branch,
	}
	if started {
		eventTime := event.Time
		attempt.Status = "running"
		attempt.StartedSeq = event.Seq
		attempt.StartedAt = &eventTime
	}
	return attempt
}

func stageAttemptID(runID string, branch int, stage string, anchorSeq uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%d", runID, branch, stage, anchorSeq)))
	return "sta_" + base64.RawURLEncoding.EncodeToString(sum[:])
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

func matchingOpenAttempt(
	attempts []StageAttempt,
	number int,
	class journal.AttemptClass,
	branch int,
) int {
	wantClass := attemptClass(class)
	bestIndex := -1
	bestScore := -1
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].FinishedSeq != 0 || attempts[i].Number != number {
			continue
		}
		score := 0
		if attempts[i].branch == branch {
			score += 2
		}
		if attempts[i].Class == wantClass {
			score++
		}
		if score > bestScore {
			bestIndex = i
			bestScore = score
		}
		if score == 3 {
			return i
		}
	}
	return bestIndex
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

// ListRuns returns the read response with its freshness envelope attached.
//
// A thin wrapper around listRunsUnannotated so the envelope lands on EVERY success
// return rather than on whichever ones someone remembered to edit. Several of
// these methods return successfully from more than one place.
func (s *Local) ListRuns(ctx context.Context, options RunListOptions) (RunList, error) {
	out, err := s.listRunsUnannotated(ctx, options)
	if err != nil {
		return RunList{}, err
	}
	if err := s.annotateRunStaleness(out.Runs); err != nil {
		return RunList{}, err
	}
	return annotated[RunList](ctx, s, out), nil
}

// GetRun returns the read response with its freshness envelope attached.
//
// A thin wrapper around getRunUnannotated so the envelope lands on EVERY success
// return rather than on whichever ones someone remembered to edit. Several of
// these methods return successfully from more than one place.
func (s *Local) GetRun(ctx context.Context, runID string) (RunDetail, error) {
	out, err := s.getRunUnannotated(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if out.Phase == journal.PhaseRunning && s.sources.SchedulerHeartbeat != nil {
		lastTickAt, err := s.sources.SchedulerHeartbeat()
		if err != nil {
			return RunDetail{}, fmt.Errorf("read scheduler heartbeat: %w", err)
		}
		out.Stale = runIsStale(out.RunSummary, s.now().UTC(), lastTickAt, s.sources.LivenessTimeout)
	}
	return annotated[RunDetail](ctx, s, out), nil
}

func (s *Local) annotateRunStaleness(runs []RunSummary) error {
	if s.sources.SchedulerHeartbeat == nil {
		return nil
	}
	hasRunning := false
	for i := range runs {
		if runs[i].Phase == journal.PhaseRunning {
			hasRunning = true
			break
		}
	}
	if !hasRunning {
		return nil
	}
	lastTickAt, err := s.sources.SchedulerHeartbeat()
	if err != nil {
		return fmt.Errorf("read scheduler heartbeat: %w", err)
	}
	observedAt := s.now().UTC()
	for i := range runs {
		runs[i].Stale = runIsStale(runs[i], observedAt, lastTickAt, s.sources.LivenessTimeout)
	}
	return nil
}

func runIsStale(run RunSummary, observedAt, lastTickAt time.Time, timeout time.Duration) bool {
	if run.Phase != journal.PhaseRunning || timeout <= 0 ||
		daemonstate.Evaluate(observedAt, lastTickAt, timeout).Healthy {
		return false
	}
	if run.LastActivityAt.IsZero() {
		return true
	}
	return observedAt.Sub(run.LastActivityAt) > timeout
}

// RunEvents returns the read response with its freshness envelope attached.
//
// A thin wrapper around runEventsUnannotated so the envelope lands on EVERY success
// return rather than on whichever ones someone remembered to edit. Several of
// these methods return successfully from more than one place.
func (s *Local) RunEvents(ctx context.Context, runID string) (EventList, error) {
	out, err := s.runEventsUnannotated(ctx, runID)
	if err != nil {
		return EventList{}, err
	}
	return annotated[EventList](ctx, s, out), nil
}

// StageAttempts returns the read response with its freshness envelope attached.
//
// A thin wrapper around stageAttemptsUnannotated so the envelope lands on EVERY success
// return rather than on whichever ones someone remembered to edit. Several of
// these methods return successfully from more than one place.
func (s *Local) StageAttempts(ctx context.Context, runID, stage string) (AttemptList, error) {
	out, err := s.stageAttemptsUnannotated(ctx, runID, stage)
	if err != nil {
		return AttemptList{}, err
	}
	return annotated[AttemptList](ctx, s, out), nil
}
