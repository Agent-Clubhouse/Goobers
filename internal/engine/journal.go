package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

// JournalQuery is the Temporal query through which a run's journal projection
// is read (#629). The workflow accumulates every journal decision it makes as
// deterministic workflow state, so answering this query — on a live, completed,
// or failed run — replays history and re-derives the identical projection:
// the journal is a function of history, never an activity-side side channel.
const JournalQuery = "goobers.journal.v1"

// Journal op kinds. An op is one journal write the workflow committed to:
// either a plain event append or a content-addressed artifact record (which
// the projection writer turns into blob + artifact.recorded event, exactly as
// the local runner's journal does).
const (
	opAppend   = "append"
	opArtifact = "artifact"
)

// JournalOp is one journal write in a run's projection, in append order.
type JournalOp struct {
	// Kind is opAppend or opArtifact.
	Kind string `json:"kind"`
	// Event is the append payload (Kind == opAppend). Seq and Schema are
	// assigned by the journal writer; Time here is the deterministic
	// workflow-clock time the decision was made at (excluded from conformance,
	// populated for the product surface).
	Event *journal.Event `json:"event,omitempty"`
	// Artifact is the record payload (Kind == opArtifact).
	Artifact *JournalArtifactOp `json:"artifact,omitempty"`
	// Time is the workflow-deterministic timestamp for this write.
	Time time.Time `json:"time"`
}

// JournalArtifactOp records one content-addressed artifact the projection
// writer commits under artifacts/ — the runner-authored blobs whose bytes the
// workflow can reconstruct deterministically (context manifests, gate
// verdicts). Stage/Attempt/Class scope stage-attempt artifacts exactly like
// journal.Run.RecordStageArtifact; a bare artifact (gate verdicts) leaves them
// zero.
type JournalArtifactOp struct {
	Stage   string               `json:"stage,omitempty"`
	Attempt int                  `json:"attempt,omitempty"`
	Class   journal.AttemptClass `json:"class,omitempty"`
	Name    string               `json:"name"`
	Data    []byte               `json:"data"`
}

// JournalProjection is the complete, self-contained journal projection of one
// engine run: the pinned identity for run.yaml, the immutable input snapshots
// (pinned graph, item), and the seq-ordered journal ops. ProjectRun turns it
// into the standard runs/<id>/ layout.
type JournalProjection struct {
	// Identity carries the run.yaml identity fields the workflow owns
	// (RunID, Workflow, WorkflowVersion, WorkflowDigest, Gaggle, Trigger).
	// Schema, StartedAt, and Inputs are assigned by the projection writer.
	Identity journal.RunIdentity `json:"identity"`
	// Item is the driving backlog item snapshot, if any — journaled as the
	// immutable "item" input exactly like the local runner's Start.
	Item *apiv1.BacklogItem `json:"item,omitempty"`
	// Graph is the pinned canonical workflow graph JSON — the
	// journal.PinnedWorkflowGraphInputName input snapshot.
	Graph json.RawMessage `json:"graph,omitempty"`
	// Ops are the journal writes in order. The first is always the run.started
	// append; a projectable history ends with exactly one run.finished.
	Ops []JournalOp `json:"ops"`
}

// runJournal accumulates the journal projection as the workflow walks. All
// state is plain workflow state: mutated only from workflow code, exposed
// read-only through JournalQuery, so it is deterministic and replay-derived.
// Its emission sites mirror internal/runner's journal appends one-for-one for
// the shared (tier-agnostic) event stream; local-runner-only mechanics
// (heartbeats, resume repairs, mutation sidecars) have no engine analogue and
// are documented drift-ledger items where they matter.
type runJournal struct {
	proj JournalProjection

	usesRepo       bool
	branchRecorded bool
	branchRef      *journal.ExternalRef
}

// newRunJournal builds the recorder and registers the projection query. The
// caller records runStarted (and, for a non-deferred trigger, the run-branch
// provenance) once the definition has compiled.
func newRunJournal(ctx workflow.Context, in RunInput, m *wf.Machine) (*runJournal, error) {
	graph, err := json.Marshal(m.Graph())
	if err != nil {
		return nil, fmt.Errorf("engine: marshal pinned workflow graph: %w", err)
	}
	rec := &runJournal{
		proj: JournalProjection{
			Identity: journal.RunIdentity{
				RunID:           in.RunID,
				Workflow:        in.WorkflowName,
				WorkflowVersion: in.Version,
				WorkflowDigest:  m.Digest(),
				Gaggle:          in.Gaggle,
				Trigger:         journal.Trigger{Kind: journal.TriggerKind(in.TriggerKind), Ref: in.TriggerRef},
			},
			Item:  in.Item,
			Graph: graph,
		},
		usesRepo: runner.MachineUsesRepo(m),
		branchRef: &journal.ExternalRef{
			Provider: string(in.RepoRef.Provider),
			Kind:     "branch",
			ID: providers.BranchNameIn(
				providers.NormalizeBranchNamespace(in.BranchNamespace),
				in.WorkflowName, in.RunID,
			),
		},
	}
	if err := workflow.SetQueryHandler(ctx, JournalQuery, func() (JournalProjection, error) {
		return rec.proj, nil
	}); err != nil {
		return nil, fmt.Errorf("engine: register journal query: %w", err)
	}
	return rec, nil
}

func (r *runJournal) append(ctx workflow.Context, ev journal.Event) {
	r.appendAt(workflow.Now(ctx), ev)
}

func (r *runJournal) appendAt(at time.Time, ev journal.Event) {
	e := ev
	r.proj.Ops = append(r.proj.Ops, JournalOp{Kind: opAppend, Event: &e, Time: at})
}

func (r *runJournal) artifactAt(at time.Time, op JournalArtifactOp) {
	o := op
	r.proj.Ops = append(r.proj.Ops, JournalOp{Kind: opArtifact, Artifact: &o, Time: at})
}

// runStarted mirrors journal.Create's own opening append.
func (r *runJournal) runStarted(ctx workflow.Context) {
	r.append(ctx, journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)})
}

// recordRunBranchUpfront mirrors the local runner's Start: a repo-using run
// with a non-deferred trigger records its run-branch provenance before the
// first stage (internal/runner/run.go, deferRunBranchProvenance).
func (r *runJournal) recordRunBranchUpfront(ctx workflow.Context, in RunInput) {
	kind := journal.TriggerKind(in.TriggerKind)
	if kind == journal.TriggerSchedule || kind == journal.TriggerItem {
		return
	}
	r.recordRunBranch(ctx)
}

// recordRunBranch appends the run-branch ref.touched once per run, mirroring
// internal/runner.(*Runner).recordRunBranch. No-op for a workflow that never
// touches a repository workspace, or once recorded.
func (r *runJournal) recordRunBranch(ctx workflow.Context) {
	if !r.usesRepo || r.branchRecorded {
		return
	}
	r.branchRecorded = true
	ref := *r.branchRef
	r.append(ctx, journal.Event{Type: journal.EventRefTouched, ExternalRef: &ref})
}

// recordDeferredRunBranch applies the local runner's lazy branch-provenance
// rule after one stage dispatch (runTask: "a branchless no-work result with no
// provider mutations touched no external ref").
func (r *runJournal) recordDeferredRunBranch(ctx workflow.Context, dispatchErr error, result apiv1.ResultEnvelope) {
	if r.branchRecorded || !r.usesRepo {
		return
	}
	if dispatchErr != nil || result.Status != apiv1.ResultNoWork {
		r.recordRunBranch(ctx)
	}
}

func (r *runJournal) stageStarted(at time.Time, stage string, attempt int, class journal.AttemptClass) {
	r.appendAt(at, journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: attempt, AttemptClass: class})
}

// contextManifest mirrors internal/runner's recordContextManifest byte-for-byte
// so both runners commit identical manifest blobs (identical digests).
func (r *runJournal) contextManifest(at time.Time, stage string, attempt int, class journal.AttemptClass, pointers []apiv1.ContextPointer) error {
	copied := make([]apiv1.ContextPointer, len(pointers))
	copy(copied, pointers)
	data, err := json.Marshal(contextManifest{ContextPointers: copied})
	if err != nil {
		return fmt.Errorf("engine: marshal context manifest for %q: %w", stage, err)
	}
	r.artifactAt(at, JournalArtifactOp{
		Stage: stage, Attempt: attempt, Class: class,
		Name: journal.ContextManifestArtifactName(stage, attempt),
		Data: data,
	})
	return nil
}

// contextManifest matches the local runner's marshaled shape
// (internal/runner/run.go) so manifest digests agree across runners.
type contextManifest struct {
	ContextPointers []apiv1.ContextPointer `json:"contextPointers"`
}

// executorError mirrors runTask's per-attempt dispatch-failure event.
func (r *runJournal) executorError(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, failureClass journal.AttemptClass, dispatchErr error) {
	r.append(ctx, journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
		Error:  &journal.ErrorDetail{Code: "executor_error", Message: dispatchErr.Error()},
		Runner: map[string]any{"retryFailureClass": string(failureClass)},
	})
}

// stageFinished mirrors runTask's stage.finished append, including the
// tolerated-failure output discard.
func (r *runJournal) stageFinished(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, result apiv1.ResultEnvelope, continueOnError bool) {
	outputs := result.Outputs
	if result.Status == apiv1.ResultFailure && continueOnError {
		outputs = nil
	}
	r.append(ctx, journal.Event{
		Type: journal.EventStageFinished, Stage: stage, Attempt: attempt, AttemptClass: class,
		Status: string(result.Status), Error: resultErrorDetail(result),
		Outputs: outputs, Artifacts: journalRefsFrom(result.Artifacts),
	})
}

// toleratedFailure mirrors journalToleratedFailure: the error event that keeps
// a continueOnError'd failure visible, attributed to the failing attempt.
func (r *runJournal) toleratedFailure(ctx workflow.Context, stage string) {
	attempt, class := r.lastFinishedAttempt(stage)
	r.append(ctx, journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
		Error: &journal.ErrorDetail{
			Code:    "stage_failure_tolerated",
			Message: fmt.Sprintf("stage %q failure tolerated by continueOnError", stage),
		},
	})
}

// lastFinishedAttempt scans the recorded ops backwards for stage's most recent
// stage.finished — the same journal-derived attribution
// journalToleratedFailure reads back from events.jsonl.
func (r *runJournal) lastFinishedAttempt(stage string) (int, journal.AttemptClass) {
	for i := len(r.proj.Ops) - 1; i >= 0; i-- {
		op := r.proj.Ops[i]
		if op.Kind != opAppend || op.Event == nil || op.Event.Stage != stage {
			continue
		}
		if op.Event.Type == journal.EventStageFinished {
			return op.Event.Attempt, op.Event.AttemptClass
		}
	}
	return 0, ""
}

// blocked mirrors taskOutcome's #544 arm: the blocked cause journaled before
// the escalated terminal.
func (r *runJournal) blocked(ctx workflow.Context, stage string, result apiv1.ResultEnvelope) {
	r.append(ctx, journal.Event{
		Type: journal.EventError, Stage: stage,
		Error: &journal.ErrorDetail{Code: "blocked_by_agent", Message: blockedReason(result)},
	})
}

func (r *runJournal) gatePaused(ctx workflow.Context, gate string) {
	r.append(ctx, journal.Event{Type: journal.EventGatePaused, Gate: gate})
}

// gateStarted mirrors internal/gate's recordStart durable pre-dispatch marker.
func (r *runJournal) gateStarted(ctx workflow.Context, gate string, repassAttempt int) {
	r.append(ctx, journal.Event{
		Type:   journal.EventGateStarted,
		Gate:   gate,
		Runner: map[string]any{"repassAttempt": repassAttempt},
	})
}

// evaluatorRetry mirrors internal/gate's recordEvaluatorRetry (#765).
func (r *runJournal) evaluatorRetry(ctx workflow.Context, gate string, attempt int, err error) {
	r.append(ctx, journal.Event{
		Type:  journal.EventError,
		Gate:  gate,
		Error: &journal.ErrorDetail{Code: "evaluator_transient", Message: err.Error()},
		Runner: map[string]any{
			"evaluatorAttempt":  attempt,
			"retryFailureClass": "infra",
		},
	})
}

// gateEvaluated mirrors internal/gate's recordVerdict: the flat normative
// verdict event, with the full agentic Verdict committed as a
// "verdict/<gate>-<attempt>.json" artifact the event's Name references (the
// projection writer resolves the event Ref from the recorded artifact).
func (r *runJournal) gateEvaluated(ctx workflow.Context, gr gateResult, verdict *apiv1.Verdict) error {
	at := workflow.Now(ctx)
	ev := journal.Event{
		Type: journal.EventGateEvaluated,
		Gate: gr.Gate, Verdict: gr.Outcome, Target: gr.Target, Escalated: gr.Escalated,
		Runner: map[string]any{
			"repassAttempt": gr.Attempt,
			"escalated":     gr.Escalated,
		},
	}
	if verdict != nil {
		data, err := json.Marshal(verdict)
		if err != nil {
			return fmt.Errorf("engine: marshal verdict for gate %q: %w", gr.Gate, err)
		}
		name := fmt.Sprintf("verdict/%s-%d.json", gr.Gate, gr.Attempt)
		r.artifactAt(at, JournalArtifactOp{Name: name, Data: data})
		ev.Name = name
	}
	r.appendAt(at, ev)
	return nil
}

// runFailedCause mirrors failTerminal/finishStageFailure's run_failed cause
// event (#305/#710): stage-attributed when the failure has one, bare for a
// walk-level error.
func (r *runJournal) runFailedCause(ctx workflow.Context, stage, code, message string) {
	journaled := message
	if stage != "" && code != "" {
		journaled = code + ": " + message
	}
	r.append(ctx, journal.Event{
		Type: journal.EventError, Stage: stage,
		Error: &journal.ErrorDetail{Code: "run_failed", Message: journaled},
	})
}

// runFinished closes the projection with the terminal phase, mapped to the
// local runner's run.finished vocabulary.
func (r *runJournal) runFinished(ctx workflow.Context, phase journal.RunPhase) {
	r.append(ctx, journal.Event{Type: journal.EventRunFinished, Status: string(phase)})
}

// phaseForStatus maps the engine's RunResult status onto the local runner's
// terminal phase vocabulary — the same mapping the cross-runner outcome tests
// pin (crossrunner_test.go statusForPhase, inverted).
func phaseForStatus(status string) (journal.RunPhase, error) {
	switch status {
	case StatusCompleted:
		return journal.PhaseCompleted, nil
	case StatusBlocked:
		return journal.PhaseAborted, nil
	case StatusEscalated:
		return journal.PhaseEscalated, nil
	case StatusFailed:
		return journal.PhaseFailed, nil
	}
	return "", fmt.Errorf("engine: no journal phase for run status %q", status)
}

// blockedReason mirrors internal/runner's blockedReason.
func blockedReason(result apiv1.ResultEnvelope) string {
	if result.Error != nil && result.Error.Message != "" {
		if result.Error.Code != "" {
			return result.Error.Code + ": " + result.Error.Message
		}
		return result.Error.Message
	}
	if s := strings.TrimSpace(result.Summary); s != "" {
		return s
	}
	return "stage reported blocked with no error detail"
}

// resultErrorDetail mirrors internal/runner's errorDetailFrom for the plain
// case. The #415 escalate-code summary override is deliberately not ported:
// its codes are runner-owned policy the engine does not yet route (see the
// drift note in gates.go), and the override only rewrites Error.Message,
// which is excluded from conformance.
func resultErrorDetail(result apiv1.ResultEnvelope) *journal.ErrorDetail {
	if result.Error == nil {
		return nil
	}
	return &journal.ErrorDetail{Code: result.Error.Code, Message: result.Error.Message}
}

// journalRefsFrom mirrors internal/runner's refsFrom: the wire artifacts a
// stage reported, in journal.Ref form for the stage.finished event.
func journalRefsFrom(artifacts []apiv1.ArtifactPointer) []journal.Ref {
	if len(artifacts) == 0 {
		return nil
	}
	out := make([]journal.Ref, len(artifacts))
	for i, a := range artifacts {
		out[i] = journal.Ref{Path: a.Path, Digest: a.Digest, Size: a.Size, MediaType: a.MediaType}
	}
	return out
}
