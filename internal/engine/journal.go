package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
	"github.com/goobers/goobers/internal/runcontrol"
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
// a plain event append, a content-addressed artifact record (which the
// projection writer turns into blob + artifact.recorded event, exactly as
// the local runner's journal does), or a span record (#2907) — an
// executor-produced blob (a harness transcript) the workflow can reference by
// digest but never itself hold the bytes of.
const (
	opAppend   = "append"
	opArtifact = "artifact"
	opSpan     = "span"
)

// JournalOp is one journal write in a run's projection, in append order.
type JournalOp struct {
	// Kind is opAppend, opArtifact, or opSpan.
	Kind string `json:"kind"`
	// Event is the append payload (Kind == opAppend). Seq and Schema are
	// assigned by the journal writer; Time here is the deterministic
	// workflow-clock time the decision was made at (excluded from conformance,
	// populated for the product surface).
	Event *journal.Event `json:"event,omitempty"`
	// Artifact is the record payload (Kind == opArtifact).
	Artifact *JournalArtifactOp `json:"artifact,omitempty"`
	// Span is the record payload (Kind == opSpan).
	Span *JournalSpanOp `json:"span,omitempty"`
	// Time is the workflow-deterministic timestamp for this write.
	Time time.Time `json:"time"`
	// EmitKey is the op's live-emission idempotency key (DS4), assigned once
	// by assignEmitKeys for a run with live journaling and empty otherwise.
	// The repair projection (ProjectRun) deliberately ignores it: a
	// re-projected journal carries no emit keys, which is what
	// livejournal.Authored keys the live/projected distinction on.
	EmitKey string `json:"emitKey,omitempty"`
}

// JournalArtifactOp records one content-addressed artifact the projection
// writer commits under artifacts/ — the runner-authored blobs whose bytes the
// workflow can reconstruct deterministically (context manifests, gate
// verdicts). Stage/Attempt/Class scope stage-attempt artifacts exactly like
// journal.Run.RecordStageArtifact; a bare artifact (gate verdicts) leaves them
// zero.
type JournalArtifactOp struct {
	Stage     string               `json:"stage,omitempty"`
	Attempt   int                  `json:"attempt,omitempty"`
	Class     journal.AttemptClass `json:"class,omitempty"`
	Name      string               `json:"name"`
	Data      []byte               `json:"data"`
	Integrity apiv1.Integrity      `json:"integrity"`
}

// JournalSpanOp records one within-stage span (a harness transcript) the
// executor that ran the stage already committed to content-addressed storage
// — journal.Run.RecordSpanWithSchema on the local runner, workerhost's
// StagingArtifacts on a tier-3 worker (#2900, #2935). Unlike JournalArtifactOp
// the workflow never holds the bytes: apiv1.ResultEnvelope.Transcript carries
// only the pointer the executor already wrote, so Ref is what the workflow
// deterministically knows from history, and the projection writer adopts the
// span by fetching Ref.Digest rather than recomputing it (#2907).
type JournalSpanOp struct {
	Stage      string               `json:"stage,omitempty"`
	Attempt    int                  `json:"attempt,omitempty"`
	Class      journal.AttemptClass `json:"class,omitempty"`
	Name       string               `json:"name"`
	DataSchema string               `json:"dataSchema,omitempty"`
	Ref        journal.Ref          `json:"ref"`
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
	// Definition is the pinned workflow definition used for crash-safe local
	// reconstruction of this projection.
	Definition json.RawMessage `json:"definition,omitempty"`
	// GateGooberCapabilities is the reviewer-goober capability map pinned into
	// the run input at start (#294) — journaled as the
	// journal.PinnedGateGooberCapabilitiesInputName input snapshot so
	// post-start consumers (the daemon credential plane, PR #3528) resolve an
	// agentic gate's reviewer grants from the run's pin, never the
	// currently-served config.
	GateGooberCapabilities json.RawMessage `json:"gateGooberCapabilities,omitempty"`
	// Ops are the journal writes in order. The first is always the run.started
	// append; a projectable history ends with exactly one run.finished.
	Ops []JournalOp `json:"ops"`
	// SchedulerOps are instance-journal events decided by the same history.
	// Scheduled runs carry one trigger.fired event; ordinary runs carry none.
	SchedulerOps []JournalOp `json:"schedulerOps,omitempty"`
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

	// Live emission state (DS4). live mirrors RunInput.LiveJournal — pinned
	// input, so replay agrees. emitted is the count of ops the live writer has
	// durably accepted; ordinals drives idempotency-key assignment
	// (assignEmitKeys). All plain workflow state.
	live     bool
	emitted  int
	ordinals map[string]int
}

// newRunJournal builds the recorder and registers the projection query. The
// caller records runStarted (and, for a non-deferred trigger, the run-branch
// provenance) once the definition has compiled.
func newRunJournal(ctx workflow.Context, in RunInput, m *wf.Machine) (*runJournal, error) {
	rec, err := newRunJournalRecorder(in, m)
	if err != nil {
		return nil, err
	}
	if err := workflow.SetQueryHandler(ctx, JournalQuery, func() (JournalProjection, error) {
		return rec.proj, nil
	}); err != nil {
		return nil, fmt.Errorf("engine: register journal query: %w", err)
	}
	return rec, nil
}

// newRunJournalRecorder builds the recorder without touching workflow.Context.
//
// Split out of newRunJournal so the identity and input snapshots have exactly
// one construction site. The daemon's engine starter reserves a run's journal
// BEFORE Temporal is asked to start the workflow (decision 005 D1's
// start-to-first-emit window), and the header it writes must be the same
// bytes the workflow's own first emit would have written — a live journal
// absorbs the later duplicate run.started and keeps the FIRST header as
// run.yaml, so any drift between the two construction sites would become a
// permanently wrong run.yaml on exactly the runs the reservation exists to
// protect. One function, two callers, no drift.
func newRunJournalRecorder(in RunInput, m *wf.Machine) (*runJournal, error) {
	graph, err := json.Marshal(m.Graph())
	if err != nil {
		return nil, fmt.Errorf("engine: marshal pinned workflow graph: %w", err)
	}
	definition, err := json.Marshal(m.Def)
	if err != nil {
		return nil, fmt.Errorf("engine: marshal pinned workflow definition: %w", err)
	}
	// json.Marshal sorts map keys, so this is workflow-deterministic.
	var gateGooberCapabilities json.RawMessage
	if len(in.GateGooberCapabilities) > 0 {
		gateGooberCapabilities, err = json.Marshal(in.GateGooberCapabilities)
		if err != nil {
			return nil, fmt.Errorf("engine: marshal pinned gate-goober capabilities: %w", err)
		}
	}
	runControls := in.RunControls
	if runControls.MaxRepasses == 0 && in.MaxRepasses > 0 {
		runControls.MaxRepasses = int32(in.MaxRepasses)
	}
	effectiveControls, err := runcontrol.Resolve(runControls, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("engine: resolve run controls: %w", err)
	}
	runControls = effectiveControls.Overrides()
	rec := &runJournal{
		proj: JournalProjection{
			Identity: journal.RunIdentity{
				RunID:           in.RunID,
				Workflow:        in.WorkflowName,
				WorkflowVersion: in.Version,
				WorkflowDigest:  m.Digest(),
				Gaggle:          in.Gaggle,
				// Every run this workflow journals is, by construction, driven
				// by the engine. The daemon reads it back from run.yaml to
				// keep its resume scan, stall sweep and operator paths off a
				// run it does not own (decision 003, Phase-0 hygiene). It is
				// pinned here — in the workflow, as deterministic state —
				// rather than stamped by the projection writer, so the live
				// journal plane's OpenHeader carries it from the very first
				// emit and a run is never briefly indistinguishable from a
				// runner-driven one.
				Driver:      journal.DriverEngine,
				RunControls: &runControls,
				Trigger:     journal.Trigger{Kind: journal.TriggerKind(in.TriggerKind), Ref: in.TriggerRef},
				// #3876: pinned kit provenance, matching what
				// gooberDigestStarter stamps on a runner-driven run.
				GooberDigest: in.GooberDigest,
			},
			Item:                   in.Item,
			Graph:                  graph,
			Definition:             definition,
			GateGooberCapabilities: gateGooberCapabilities,
		},
		usesRepo: runner.MachineUsesRepo(m),
		live:     in.LiveJournal,
		branchRef: &journal.ExternalRef{
			Provider: string(in.RepoRef.Provider),
			Kind:     "branch",
			ID: providers.BranchNameIn(
				providers.NormalizeBranchNamespace(in.BranchNamespace),
				in.WorkflowName, in.RunID,
			),
		},
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

func (r *runJournal) triggerFiredAt(at time.Time, in RunInput) {
	ev := journal.Event{
		Type:     journal.EventTriggerFired,
		Workflow: in.WorkflowName,
		Gaggle:   in.Gaggle,
		RunID:    in.RunID,
		Reason:   "scheduled",
	}
	r.proj.SchedulerOps = append(r.proj.SchedulerOps, JournalOp{Kind: opAppend, Event: &ev, Time: at})
}

func (r *runJournal) artifactAt(at time.Time, op JournalArtifactOp) {
	o := op
	r.proj.Ops = append(r.proj.Ops, JournalOp{Kind: opArtifact, Artifact: &o, Time: at})
}

func (r *runJournal) spanAt(at time.Time, op JournalSpanOp) {
	o := op
	r.proj.Ops = append(r.proj.Ops, JournalOp{Kind: opSpan, Span: &o, Time: at})
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
func (r *runJournal) recordDeferredRunBranch(ctx workflow.Context, dispatchErr error, result apiv1.ResultEnvelope, mutated bool) {
	if r.branchRecorded || !r.usesRepo {
		return
	}
	if dispatchErr != nil || result.Status != apiv1.ResultNoWork || mutated {
		r.recordRunBranch(ctx)
	}
}

func (r *runJournal) mutationIssues(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, issues []string) {
	if len(issues) == 0 {
		return
	}
	r.append(ctx, journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
		Error: &journal.ErrorDetail{Code: "mutation_sidecar_read_failed", Message: strings.Join(issues, "; ")},
	})
}

func (r *runJournal) mutations(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, mutations []mutationFact) {
	for _, mutation := range mutations {
		r.append(ctx, journal.Event{
			Type: journal.EventRefTouched, Stage: stage, Attempt: attempt, AttemptClass: class,
			ExternalRef: &journal.ExternalRef{
				Provider: mutation.Provider, Kind: mutation.Kind, ID: mutation.ID, URL: mutation.URL,
			},
			Runner: map[string]any{"operation": mutation.Operation},
		})
	}
}

func (r *runJournal) stageStarted(at time.Time, stage string, attempt int, class journal.AttemptClass) {
	r.appendAt(at, journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: attempt, AttemptClass: class})
}

// placement journals one attempt's runner.placement provenance from what the
// dispatch reported back (#3875, plan item E3) — the engine's counterpart to
// internal/runner.runTask's own PlacementEvent append beside stage.started.
//
// Journal-only and conformance-EXCLUDED (journal/event.go), so it has no state
// effect on the walk and cannot move a run's control flow; §11 acceptance 6
// ("which runner served the stage, which pod carried it, which image that pod
// actually ran, and how long the attempt waited for capacity") is the whole
// reason it exists, and the stall sweep is its first reader.
//
// AFTER the dispatch, not beside stage.started, and that ordering is forced
// rather than chosen: a pod attempt's placement is not KNOWN until the pod has
// been created and the attempt has settled (StagePlacement's "settled attempts
// only" contract), and inventing one at stage.started would journal the
// placement the walk ASKED for instead of the one it got — precisely the fact
// finding 002's inventory row says is missing. It still lands between this
// attempt's stage.started and the next event a reader correlates it with, so
// "every stage.started is followed by a runner.placement" holds on the wire.
//
// An attempt whose dispatch FAILED carries no placement (every dispatcher error
// discards the report) and journals nothing: absence is honest here, and a
// fabricated block would be the first untested branch in a contract that has
// none.
func (r *runJournal) placement(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, result stageActivityResult) {
	placement, ok := attemptPlacement(result)
	if !ok {
		return
	}
	r.append(ctx, journal.PlacementEvent(stage, attempt, class, placement))
}

// attemptPlacement projects one dispatch result onto the journal's placement
// payload, preferring the pod arm's dispatcher report over the self arm's
// self-observation. Both are never set at once — a stage executes on exactly
// one substrate — and the pod arm is checked first so a result that somehow
// carried both would still describe the pod that actually ran the work.
func attemptPlacement(result stageActivityResult) (journal.Placement, bool) {
	if pod := result.Placement; pod != nil {
		placement := journal.Placement{
			Runner: pod.Runner,
			Pod:    pod.Pod,
			Image:  pod.Image,
		}
		// Absent rather than zero: journal.Placement's timestamps are pointers
		// precisely so "this attempt never queued" and "it queued at the zero
		// instant" stay distinguishable.
		if !pod.QueuedAt.IsZero() {
			queuedAt := pod.QueuedAt
			placement.QueuedAt = &queuedAt
		}
		if !pod.PodStartedAt.IsZero() {
			podStartedAt := pod.PodStartedAt
			placement.PodStartedAt = &podStartedAt
		}
		return placement, placement.Runner != ""
	}
	if self := result.SelfPlacement; self != nil {
		return *self, self.Runner != ""
	}
	return journal.Placement{}, false
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
		Data: data, Integrity: apiv1.IntegrityDerived,
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

func (r *runJournal) integrityRefused(ctx workflow.Context, stage string, admission *apiv1.IntegrityAdmissionError) {
	r.append(ctx, journal.Event{
		Type:             journal.EventError,
		Stage:            stage,
		Integrity:        admission.Actual,
		MinimumIntegrity: admission.Minimum,
		Error: &journal.ErrorDetail{
			Code: apiv1.IntegrityAdmissionErrorCode, Message: admission.Error(),
		},
	})
}

// stageFinished mirrors runTask's stage.finished append, including the
// tolerated-failure output discard. A transcript the executor recorded is
// journaled as a span op immediately before stage.finished, mirroring the
// local runner's own ordering (the harness executor records its span mid-run,
// before runTask appends stage.finished) — see JournalSpanOp (#2907).
func (r *runJournal) stageFinished(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, result apiv1.ResultEnvelope, continueOnError bool) {
	if result.Transcript != nil {
		r.spanAt(workflow.Now(ctx), JournalSpanOp{
			Stage: stage, Attempt: attempt, Class: class,
			Name: stage + ".transcript", Ref: journalRefFrom(*result.Transcript),
		})
	}
	r.append(ctx, stageFinishedEvent(stage, attempt, class, result, continueOnError))
}

// stageFinishedEvent builds the stage.finished event, including the
// tolerated-failure output discard. Split out from stageFinished so the
// discard's exact shape is testable without a workflow environment.
func stageFinishedEvent(stage string, attempt int, class journal.AttemptClass, result apiv1.ResultEnvelope, continueOnError bool) journal.Event {
	outputs := result.Outputs
	var runnerFacts map[string]any
	if result.Status == apiv1.ResultFailure && continueOnError {
		outputs = nil
		// The output discard mirrors the local runner exactly and must stay.
		// But a rate-limit reset is not a stage OUTPUT in any meaningful
		// sense — it is a scheduler fact about the provider, and the local
		// runner acts on it (notifyRateLimited) BEFORE it reaches the
		// continueOnError arm, precisely so a tolerated failure still parks
		// the provider window. An engine run's only channel to this daemon's
		// ProviderQuotaState is the journal, so the fact is carried on the
		// event's Runner map: not conformance-normative (journal.ConformanceView
		// does not project Runner), so the two drivers' journals still match,
		// while the daemon's live observer can still see it.
		if reset, ok := rateLimitResetFact(result); ok {
			runnerFacts = map[string]any{executor.OutputRateLimitReset: reset}
		}
	}
	return journal.Event{
		Type: journal.EventStageFinished, Stage: stage, Attempt: attempt, AttemptClass: class,
		Status: string(result.Status), Error: resultErrorDetail(result),
		Outputs: outputs, Artifacts: journalRefsFrom(result.Artifacts),
		Runner: runnerFacts,
		// Mirrors the local runner's stage.finished: the produced provenance is
		// normative, so it must appear identically in both journals (TBH-4).
		Integrity: result.Integrity,
	}
}

// rateLimitResetFact recovers a rate-limited failure's reset instant from the
// result's declared outputs, as the raw value the stage wrote so the daemon's
// observer applies the one shared parse to it.
func rateLimitResetFact(result apiv1.ResultEnvelope) (any, bool) {
	if result.Error == nil || result.Error.Code != providers.ErrorCodeRateLimited {
		return nil, false
	}
	reset, ok := result.Outputs[executor.OutputRateLimitReset]
	return reset, ok
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

// gateStarted mirrors internal/gate's recordStart durable pre-dispatch
// marker. podAttempt is the pod dispatch this marker precedes — the gate's
// gateDispatches ordinal (gatePodAttempt; see gates.go for the two counters'
// full contract) that the dispatch arm's evaluateWithInfraRetry call is about
// to claim — and 0 for the self arm, which never dispatches a pod. Passed 0
// rather than the eventual attempt count on an infra retry: this marker is
// journaled once, before the retry loop starts, so it can only attribute the
// FIRST pod attempt an evaluation will try; a retry's own later attempt
// number is visible on the surrendered result and the pod it names, not
// re-journaled here. The key is omitted from Runner rather than journaled as
// 0, so a self-arm gate.started reads exactly as it always has.
func (r *runJournal) gateStarted(ctx workflow.Context, gate string, repassAttempt, podAttempt int) {
	ev := journal.Event{
		Type:   journal.EventGateStarted,
		Gate:   gate,
		Runner: map[string]any{"repassAttempt": repassAttempt},
	}
	if podAttempt > 0 {
		ev.Runner["podAttempt"] = podAttempt
	}
	r.append(ctx, ev)
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
//
// Like recordVerdict it hands the verdict's ArtifactPointer back (nil for a
// non-agentic gate) so walk can surface it as the "<gate>.verdict" repass
// ContextPointer (#412). The pointer is computed workflow-side from the same
// bytes the projection will commit (journal.ArtifactRef), keeping it a
// deterministic function of history.
func (r *runJournal) gateEvaluated(ctx workflow.Context, gr gateResult, verdict *apiv1.Verdict) (*apiv1.ArtifactPointer, error) {
	at := workflow.Now(ctx)
	ev := journal.Event{
		Type: journal.EventGateEvaluated,
		Gate: gr.Gate, Verdict: gr.Outcome, Target: gr.Target, Escalated: gr.Escalated,
		Runner: map[string]any{
			"repassAttempt":   gr.Attempt,
			"gateAttempt":     gr.GateAttempt,
			"escalated":       gr.Escalated,
			"duplicateDiff":   gr.DuplicateDiff,
			"verdictCacheHit": gr.CacheHit,
		},
	}
	if gr.RepassTarget != "" {
		ev.Runner["repassTarget"] = gr.RepassTarget
	}
	// The implementation-lane annotations (#3882), keyed exactly as
	// internal/gate's recordVerdict keys them: this event is the only place a
	// consumer can learn that a gate resolved WITHOUT invoking a reviewer, and
	// which of the previous verdict's findings this one settled.
	if gr.DiffDigest != "" {
		ev.Runner["diffDigest"] = gr.DiffDigest
	}
	if gr.RepassCause != nil {
		ev.Runner["repassCause"] = gr.RepassCause
	}
	if gr.Reason != "" {
		ev.Runner["reason"] = gr.Reason
	}
	if verdict != nil && len(verdict.Findings) > 0 {
		identities := make([]string, 0, len(verdict.Findings))
		for i := range verdict.Findings {
			learning.NormalizeFinding(&verdict.Findings[i], gr.Gate, gr.DiffDigest)
			identities = append(identities, verdict.Findings[i].ID)
		}
		ev.Runner["findingIdentities"] = identities
		ev.Runner["learningFindings"] = gate.LearningFindingRecords(verdict.Findings)
		ev.Runner["correctionFeedback"] = verdict.Rationale
	}
	if len(gr.ResolvedFindingIDs) > 0 {
		ev.Runner["resolvedFindingIdentities"] = gr.ResolvedFindingIDs
	}
	if len(gr.SuppressedFindingIDs) > 0 {
		ev.Runner["suppressedFindingIdentities"] = gr.SuppressedFindingIDs
	}
	if len(gr.ReopenedFindingIDs) > 0 {
		ev.Runner["reopenedFindingIdentities"] = gr.ReopenedFindingIDs
	}
	if len(gr.DisprovenFindingIDs) > 0 {
		ev.Runner["disprovenFindingIdentities"] = gr.DisprovenFindingIDs
	}
	if len(gr.DisprovenFindings) > 0 {
		ev.Runner["disprovenLearningFindings"] = gate.LearningFindingRecords(gr.DisprovenFindings)
	}
	if len(gr.ArbitratedFindingIDs) > 0 {
		ev.Runner["arbitratedFindingIdentities"] = gr.ArbitratedFindingIDs
	}
	if len(gr.RepeatFindingDispositions) > 0 {
		ev.Runner["repeatFindingDispositions"] = gr.RepeatFindingDispositions
	}
	var artifact *apiv1.ArtifactPointer
	if verdict != nil {
		data, err := json.Marshal(verdict)
		if err != nil {
			return nil, fmt.Errorf("engine: marshal verdict for gate %q: %w", gr.Gate, err)
		}
		ref, err := journal.ArtifactRef(data)
		if err != nil {
			return nil, fmt.Errorf("engine: address verdict for gate %q: %w", gr.Gate, err)
		}
		name := fmt.Sprintf("verdict/%s-%d.json", gr.Gate, gr.Attempt)
		r.artifactAt(at, JournalArtifactOp{Name: name, Data: data, Integrity: apiv1.IntegrityDerived})
		ev.Name = name
		ev.Integrity = apiv1.IntegrityDerived
		artifact = &apiv1.ArtifactPointer{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
			MediaType: "application/json", Integrity: apiv1.IntegrityDerived,
		}
	}
	r.appendAt(at, ev)
	return artifact, nil
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

// runCanceledCause is the run_failed cause text for a cancelled run. The
// cancellation error itself is Temporal vocabulary ("canceled"), so the cause
// names the event in the run's own terms and keeps the underlying error for
// the operator who has to tell an external `temporal workflow cancel` apart
// from the daemon's stall sweep.
func runCanceledCause(err error) string {
	if err == nil {
		return "run canceled on the engine"
	}
	return "run canceled on the engine: " + err.Error()
}

// runFinished closes the projection with the terminal phase, mapped to the
// local runner's run.finished vocabulary.
func (r *runJournal) runFinished(ctx workflow.Context, phase journal.RunPhase) {
	r.append(ctx, journal.Event{Type: journal.EventRunFinished, Status: string(phase)})
}

// PhaseForStatus maps the engine's RunResult status onto the local runner's
// terminal phase vocabulary — the same mapping the cross-runner outcome tests
// pin (crossrunner_test.go statusForPhase, inverted).
//
// Exported (plan item E2) because the daemon's RunResult -> StartResult mapping
// and the terminal-hook frame built on it MUST key on the journal PHASE, not on
// the status word. StatusBlocked — an @abort target — maps to PhaseAborted, not
// to anything "blocked"-shaped, so a hook table keyed on status wording
// silently skips that whole class of terminal. Keeping one exported mapping
// means there is nothing for a second table to disagree with.
func PhaseForStatus(status string) (journal.RunPhase, error) {
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
		out[i] = journalRefFrom(a)
	}
	return out
}

// journalRefFrom converts one wire ArtifactPointer to journal.Ref form.
func journalRefFrom(a apiv1.ArtifactPointer) journal.Ref {
	return journal.Ref{
		Path: a.Path, Digest: a.Digest, Size: a.Size, MediaType: a.MediaType, Integrity: a.Integrity,
	}
}
