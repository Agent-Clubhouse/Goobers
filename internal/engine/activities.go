package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// Activity names. The workflow refers to activities by these names so it is
// decoupled from the concrete receiver instance; they must equal the method
// names on Activities exactly (Temporal registers struct methods by name).
const (
	ActInvokeGoober       = "InvokeGoober"
	ActReviewGoober       = "ReviewGoober"
	ActRunDeterministic   = "RunDeterministic"
	ActEvaluateAutomated  = "EvaluateAutomated"
	ActReconcileSchedules = "ReconcileSchedules"
	// ActDispatchStage is the mode-3 dispatch activity (#3588,
	// dispatchstage.go): the stage executes in a dispatcher-created pod and
	// its surrendered outputs marshal back into the same stageActivityResult
	// the in-process activities return.
	ActDispatchStage     = "DispatchStage"
	mutationsSidecarFile = "mutations.jsonl"
)

// Activities bundles the engine's side-effecting operations as Temporal
// activities. Each seam (defined in package invoke) is optional; a nil seam
// yields a clear "not configured" error if the workflow reaches a node that needs
// it, rather than a panic. The runtime (M8) constructs this with a real
// invoke.Goober.
type Activities struct {
	Goober invoke.Goober
	Det    invoke.Deterministic
	Auto   invoke.Automated
	// ScheduleService is required only by the quarantined tier-3 schedule
	// reconciliation workflow.
	ScheduleService workflowservice.WorkflowServiceClient
	// Workspaces provisions the fresh working copy each stage attempt runs
	// in. Required for any stage that executes in a workspace (agentic tasks,
	// deterministic tasks, agentic reviewer gates); an automated gate's checks
	// are pure functions over env.Inputs and get no workspace, matching the
	// local runner (#112).
	Workspaces WorkspaceProvisioner
	// Scrubber removes secret-shaped material before activity results enter
	// Temporal history. Nil uses the journal's pattern scrubber.
	Scrubber journal.Scrubber
	// Journal is the live-journal emission seam (DS4): in-process it is the
	// daemon's *livejournal.Writer, remotely it is livejournal.HTTPEmitter at
	// the write API's journal plane. Required only for runs pinned with
	// RunInput.LiveJournal; a run that demands live journaling on a worker
	// with no emitter fails closed as an infra-classed attempt (EmitJournal).
	Journal JournalEmitter
	// Canary is the #2931 fail-closed dispatch canary
	// (distributed-state-and-coordination.md §11, decision record
	// docs/design/goobernetes-decisions.md): it asserts that no known
	// credential value appears in a serialized dispatch envelope, and refuses
	// to execute the stage when one does. Wire the EXACT-VALUE registry
	// (journal.RegistryScrubber — the same registry every resolver-issued and
	// credential-plane-minted value is registered with), never the pattern
	// net: a pattern can false-positive on legitimate stage inputs, and a
	// canary that can misfire on issue text would train operators to bypass
	// it. Nil disables the canary (the local runner path, which never
	// serializes an envelope off-process).
	Canary journal.Scrubber
	// Spans reads back a harness transcript an executor already committed to
	// content-addressed storage, by digest. Required only for the #3374
	// context-inspection check (InvokeGoober): a stage claiming
	// DEPENDENCY_NOT_MET is held to having actually READ the context it was
	// handed, and its transcript is the only record of whether it did.
	//
	// A nil seam FAILS OPEN — the claim is accepted unvalidated — which is
	// the deliberate choice for a check whose whole purpose is to catch an
	// agent skipping its inputs. Failing closed would turn a worker with no
	// blob store into one that rejects every legitimate blocked result, and
	// a run that cannot verify a claim is in no position to refuse it.
	Spans SpanSource
	// Dispatcher is the mode-3 substrate seam (#3588, dispatchstage.go):
	// *dispatcher.Dispatcher in production, a fake in tests. Required only by
	// DispatchStage; a run with no pinned non-self placement never reaches
	// it, and a nil seam fails that activity closed with a clear error.
	Dispatcher StageDispatcher
	// Surrenders is the plane DispatchStage reads surrendered stage results
	// from (dispatcher.ReadSurrenderedResult) — the same plane the
	// dispatcher's SurrenderGate confirms against. Required alongside
	// Dispatcher.
	Surrenders dispatcher.SurrenderPlane
}

// DispatchStageResult is what a stage activity returns: the stage's own
// result envelope plus the substrate facts the engine journals alongside it.
//
// EXPORTED for decision 003 ruling 2 (with stageActivityResult kept as an
// alias below, so every in-package reference and every recorded history stay
// identical): DispatchOne returns this to a caller outside the package — the
// daemon's runner — which has to be able to name the type it decodes into.
//
// The JSON tags are a WIRE CONTRACT recorded in ActivityTaskCompleted events;
// Placement is additive and omitempty, so a history written before it existed
// decodes with a nil Placement rather than failing.
type DispatchStageResult struct {
	// Embed the legacy activity result so its JSON stays flat and histories
	// recorded before mutation metadata was added remain replay-decodable.
	apiv1.ResultEnvelope
	Mutations      []MutationFact `json:"mutations,omitempty"`
	MutationIssues []string       `json:"mutationIssues,omitempty"`
	// WorkspaceDelta is the blob digest of a bundle carrying what this stage
	// committed (#3763), for the engine's continuity record to hand to a
	// later stage. A pod surrenders one; a self-placed stage on a writable
	// repo workspace publishes one through its workspace's DeltaPublisher
	// (#3803) — its commits are on this worker's mirror, which the next pod
	// (or another worker) cannot see.
	WorkspaceDelta string `json:"workspaceDelta,omitempty"`
	// WorkspaceDeltaBase / WorkspaceDeltaTip are the bundle's two commits,
	// journaled beside the digest (runner.workspace.delta). Additive and
	// omitempty so histories recorded before them decode unchanged.
	WorkspaceDeltaBase string `json:"workspaceDeltaBase,omitempty"`
	WorkspaceDeltaTip  string `json:"workspaceDeltaTip,omitempty"`
	// WorkspaceDeltaUnchanged reports that a writable repo stage succeeded
	// WITHOUT moving its branch, so there was nothing to publish — distinct
	// from "could not publish" (an error) and from "not a repo stage" (no
	// flag), and journaled explicitly so the record's silence is a fact.
	WorkspaceDeltaUnchanged bool `json:"workspaceDeltaUnchanged,omitempty"`
	// Placement is where this attempt actually ran, when it ran in a pod and
	// the attempt SETTLED. Nil for every in-process arm (InvokeGoober,
	// RunDeterministic, ReviewGoober): those execute on the host that is
	// already recorded, so a provenance block there would say nothing. Nil
	// also, and unavoidably, for a refused placement — see StagePlacement.
	Placement *StagePlacement `json:"placement,omitempty"`
	// SelfPlacement is where this attempt ran when it ran IN-PROCESS on the
	// worker (InvokeGoober, RunDeterministic) — the self arm's answer to the
	// question Placement answers for the pod arm (#3875, plan item E3).
	//
	// It is a SECOND field rather than a second producer of Placement because
	// the two blocks are different facts with different completeness
	// contracts: *StagePlacement is the dispatcher's report of a settled pod
	// attempt and every one of its five fields is populated (see its doc),
	// while this is what one process can observe about itself — GOOS, host,
	// and whatever the deployment declared through GOOBERS_RUNNER_* — with no
	// pod, no image, and no queue wait, ever. Folding the self arm into
	// Placement would make "non-nil means all five" false and put a
	// partially-populated block behind a doc comment that says it is
	// unreachable.
	//
	// The payload type is journal.Placement itself, not a local mirror: the
	// local runner journals the identical struct from runner.SelfPlacement,
	// and the parity harness diffs the two events. Nil for every pod arm, and
	// nil for a deployment that has declared no placement at all
	// (runner.PlacementDeclaredInEnvironment — §11 item 1 zero-declaration
	// invariance).
	//
	// WIRE CONTRACT, and additive/omitempty for the reason the whole struct
	// documents: a history recorded before this field existed decodes with a
	// nil SelfPlacement, so replaying it journals no placement event and
	// re-projects byte-identically to what it projected before.
	SelfPlacement *journal.Placement `json:"selfPlacement,omitempty"`
	// Verdict is the reviewer's decision when the attempt was an agentic
	// reviewer GATE dispatched to a pod (DispatchStageInput.Review; decision
	// 001 rulings 7–8) — what ReviewGoober returns for the self arm, carried
	// on the dispatch result because DispatchStage is the one activity every
	// pod attempt returns through. Nil for every task attempt and for every
	// in-process arm. Additive and omitempty: a history recorded before it
	// existed decodes with a nil Verdict.
	//
	// DispatchStage populates it ONLY after re-validating the surrendered
	// verdict (a non-empty Decision, the verdict schema): a non-nil Verdict
	// here is one the engine may route on.
	Verdict *apiv1.Verdict `json:"verdict,omitempty"`
	// SalvageMarker is the #724 salvage evidence for an agentic attempt whose
	// session timed out with committed work in the tree: the marker bytes the
	// walk commits as "<stage>/salvage-on-timeout.json". Carried on the result
	// rather than written here because the projection commits only bytes that
	// are in history (#629) — an artifact the activity wrote to disk could not
	// be re-derived by a replay or rebuilt by the repair projection.
	//
	// Additive and omitempty: a history recorded before this field existed
	// decodes with nil and journals nothing, exactly as it did.
	SalvageMarker []byte `json:"salvageMarker,omitempty"`
	// BaseSyncConflict is the #813 conflict detail — the code, message,
	// branch, base ref and CONFLICTING FILES — for a deterministic stage whose
	// base synchronization conflicted. Carried for the same reason
	// SalvageMarker is. Nil for every other outcome.
	BaseSyncConflict []byte `json:"baseSyncConflict,omitempty"`
	// UnpushedDiff is the #3366 capture: the patch an agentic attempt left
	// uncommitted-to-the-remote in its workspace, taken before that workspace
	// is torn down. Nil when the branch did not move, when the workspace
	// cannot report a diff, or for a non-repo stage.
	UnpushedDiff *UnpushedDiffCapture `json:"unpushedDiff,omitempty"`
}

// UnpushedDiffCapture is what an attempt's workspace was holding when it
// finished: the patch itself plus the branch and base ref it was taken
// against. The walk turns it into the "<stage>/unpushed-diff.patch" artifact
// and its metadata sidecar (#3366).
//
// A WIRE CONTRACT recorded in ActivityTaskCompleted events, like the struct
// that carries it.
type UnpushedDiffCapture struct {
	Diff    []byte `json:"diff,omitempty"`
	Branch  string `json:"branch,omitempty"`
	BaseRef string `json:"baseRef,omitempty"`
}

// StagePlacement is the substrate provenance of one pod-executed stage
// attempt, lifted verbatim from dispatcher.Report.
//
// It exists because the driver that journals `runner.placement` is not the
// process that created the pod: under decision 003 the daemon's runner drives
// the run and the WORKER's dispatcher creates the pod, so without these fields
// crossing back over the activity boundary the runner can only journal the
// placement it ASKED for, never the one it got. §11 acceptance 6 wants the
// second — which runner served the stage, which pod carried it, which image
// that pod actually ran, and how long the attempt waited for capacity.
//
// SETTLED ATTEMPTS ONLY, and every field is then populated. This is a property
// of the seam, not a coincidence, so read it as the contract:
// DispatchStage builds provenance at exactly one return — the one that carries
// a surrendered envelope — and every dispatcher error that left surrender
// unconfirmed is returned as a classified error with the report DISCARDED
// (dispatchstage.go, the SurrenderConfirmed guard). A settled outcome in turn
// requires CreatePod to have already succeeded, and Dispatch stamps Runner and
// QueuedAt before it renders, Image off the rendered spec, and Pod and
// PodStartedAt immediately after the create. So a non-nil *StagePlacement
// always names all five. Do NOT write a branch for a partially populated
// block: it is unreachable, and code that handles it is untested code that
// will rot.
//
// The corollary is the honest cost of this shape, and a caller journalling
// §11 acceptance 6 has to know it: the placement failures an operator most
// wants to see — a capacity wait that timed out, a decision-009 skew refusal,
// an agentic kit that would not publish — deliver NO provenance block at all.
// What crosses instead is the classified error, whose message names the runner
// ("capacity wait for runner %q", "probe capacity for runner %q") and, for a
// skew refusal, the exact image ("version-skew refusal for image %q"). That is
// text, not fields, and it is deliberately all this step ships: carrying a
// report onto the failure means putting it in the ApplicationError details,
// where slot 0 already belongs to the infrastructure retry-at instant
// (classifySeamError / infrastructureRetryDelay), so it is a wire-contract
// change that belongs with the runner branch that would consume it (step 6),
// not with the export.
//
// Every field is nevertheless omitzero, for decoding tolerance rather than to
// describe a live state: a block recorded by some other or older producer
// still round-trips.
type StagePlacement struct {
	// Runner is the inventory entry SelectRunner resolved for the attempt.
	Runner string `json:"runner,omitzero"`
	// Pod is the created pod's name.
	Pod string `json:"pod,omitzero"`
	// Image is the image the stage container actually ran — the decision-009
	// skew subject, read back from the rendered pod rather than from the
	// runner's declared host, so a deployment-templated runner reports the
	// template's image and not the Deployment name.
	Image string `json:"image,omitzero"`
	// QueuedAt and PodStartedAt bound the attempt's wait for capacity:
	// QueuedAt is stamped when the dispatcher accepted the attempt,
	// PodStartedAt when the pod was created.
	QueuedAt     time.Time `json:"queuedAt,omitzero"`
	PodStartedAt time.Time `json:"podStartedAt,omitzero"`
}

// stageActivityResult is the in-package spelling of DispatchStageResult. An
// ALIAS, not a definition: the export must not introduce a second type that
// could drift from the one recorded in history.
type stageActivityResult = DispatchStageResult

// MutationFact is one external effect a stage reported committing — the D5
// mutation record the driver journals for open-pr / merge-pr / push-branch.
//
// EXPORTED alongside DispatchStageResult, and for the same reason: the point
// of this contract is that a caller outside internal/engine can NAME what it
// decodes into, and a result whose element type is unnameable defeats that for
// the one field the runner must journal. mutationFact stays as an alias, so no
// second type exists and the recorded JSON is untouched.
type MutationFact struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// mutationFact is the in-package spelling of MutationFact. An ALIAS, for the
// same reason stageActivityResult is one.
type mutationFact = MutationFact

type scheduleReconcileActivityInput struct {
	Namespace     string
	TaskQueue     string
	CatchupWindow time.Duration
	Snapshot      ScheduleSnapshot
}

// ReconcileSchedules applies one snapshot inside the durable per-instance
// reconciliation workflow.
func (a *Activities) ReconcileSchedules(ctx context.Context, input scheduleReconcileActivityInput) error {
	if a.ScheduleService == nil {
		return fmt.Errorf("reconcile schedules: %w", ErrNotConfigured)
	}
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				activity.RecordHeartbeat(ctx, input.Snapshot.ConfigGeneration)
			case <-ctx.Done():
				return
			case <-stopHeartbeat:
				return
			}
		}
	}()
	activity.RecordHeartbeat(ctx, input.Snapshot.ConfigGeneration)

	reconciler, err := newScheduleReconciler(
		newTemporalScheduleStore(a.ScheduleService, input.Namespace),
		input.TaskQueue,
		input.CatchupWindow,
	)
	if err != nil {
		return err
	}
	return reconciler.reconcileDirect(ctx, input.Snapshot)
}

// ErrNotConfigured is returned by an activity whose backing seam was not wired.
var ErrNotConfigured = errors.New("engine: activity dependency not configured")

// classifySeamError converts a seam error into a typed Temporal application
// error so the attempt class survives into workflow history (#622). The
// invoke-level infrastructure marker cannot cross the activity boundary — the
// SDK serializes errors — so the class is committed here, at the last point
// the marker is visible; the workflow's retry loop and the history→journal
// projection (#629) both read it back from the recorded type alone.
func classifySeamError(err error) error {
	if err == nil {
		return nil
	}
	if invoke.IsInfrastructureFailure(err) {
		options := temporal.ApplicationErrorOptions{}
		if retryAt, ok := invoke.InfrastructureRetryAt(err); ok {
			options.Details = []interface{}{retryAt}
		}
		return temporal.NewApplicationErrorWithOptions(err.Error(), FailureTypeInfrastructure, options)
	}
	// A TRANSIENT worktree-provision failure (#3882, the engine drift-ledger
	// entry this closes): a lock contended by a concurrent worktree operation,
	// a fetch that lost its connection, a transiently-unavailable remote. The
	// local runner already reclassifies these as infrastructure so the attempt
	// retries instead of burning a repass on the AGENT for something the agent
	// never touched; the engine classified every provision failure as a stage
	// failure, which charged the run's own budget for the worker's disk.
	//
	// Checked after the invoke marker, not before: an executor that has
	// already declared its own failure class owns that answer.
	if worktree.IsTransientProvisionError(err) {
		return temporal.NewApplicationError(err.Error(), FailureTypeInfrastructure)
	}
	return temporal.NewApplicationError(err.Error(), FailureTypeStage)
}

// provisionWorkspace provisions the working copy for one stage attempt and
// stamps its path into env's required workspace field. It fails closed
// (#621/#156): a missing provisioner, a provision failure, or an empty path
// is an error — the stage never dispatches with a partial envelope, which is
// what previously made every capability-scoped credential fail closed the
// moment a real executor was wired.
//
// workspaceDelta is handed to the provisioner only for a writable repo mode:
// a scratch or read-only workspace has no run branch to land it on, and a
// read-only stage reads the pinned base by definition (the same gate the pod
// arm applies in dispatchstage.go).
func (a *Activities) provisionWorkspace(ctx context.Context, env *apiv1.InvocationEnvelope, mode apiv1.WorkspaceMode, syncBase bool, workspaceBranch, workspaceDelta string) (Workspace, error) {
	if a.Workspaces == nil {
		return nil, fmt.Errorf("stage %q requires a workspace but no provisioner is wired: %w", env.TaskID, ErrNotConfigured)
	}
	if !writableWorkspace(mode) {
		workspaceDelta = ""
	}
	ws, err := a.Workspaces.Provision(ctx, WorkspaceRequest{
		RunID:           env.RunID,
		Stage:           strings.TrimPrefix(env.TaskID, env.RunID+":"),
		Gaggle:          env.Gaggle,
		Workflow:        env.WorkflowID,
		BranchNamespace: env.BranchNamespace,
		WorkspaceBranch: workspaceBranch,
		RepoRef:         env.RepoRef,
		Mode:            mode,
		SyncBase:        syncBase,
		WorkspaceDelta:  workspaceDelta,
	})
	if err != nil {
		if worktree.IsTransientProvisionError(err) {
			return nil, invoke.InfrastructureFailure(fmt.Errorf("provision workspace for stage %q: %w", env.TaskID, err))
		}
		return nil, fmt.Errorf("provision workspace for stage %q: %w", env.TaskID, err)
	}
	if ws == nil || ws.Path() == "" {
		removeWorkspace(ctx, env.TaskID, ws)
		return nil, fmt.Errorf("workspace provisioner returned no path for stage %q (the closed invocation schema requires workspace)", env.TaskID)
	}
	env.Workspace = ws.Path()
	return ws, nil
}

// workspaceTeardownTimeout bounds one attempt's detached workspace cleanup
// (#3645). Teardown runs detached from the attempt context, so nothing else
// would ever cancel it: a git worktree removal wedged on a filesystem lock
// would otherwise retain the worker's resources for the process's lifetime.
// A variable so tests can shorten the bound; production never reassigns it.
var workspaceTeardownTimeout = 2 * time.Minute

// MetricWorkspaceTeardownFailure is the counter incremented once per stage
// attempt whose workspace teardown failed or exceeded
// workspaceTeardownTimeout. Locked or wedged worktrees accumulate on the
// worker's disk, so this is the operator's signal to sweep them (#3645).
const MetricWorkspaceTeardownFailure = "goobers_workspace_teardown_failure"

// removeWorkspace tears one attempt's working copy down. Best-effort by
// design: a teardown failure never overrides the stage's own result/error
// (the local runner's additive removeErr contract, issue #136). Detached from
// ctx so an already-expired attempt still cleans up, but bounded by
// workspaceTeardownTimeout and never silent: a failed or timed-out teardown
// leaves a leaked working copy behind, so it is reported as an error log with
// the stage and path that leaked plus a counter increment (#3645), rather
// than being discarded.
func removeWorkspace(ctx context.Context, taskID string, ws Workspace) {
	if ws == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceTeardownTimeout)
	defer cancel()
	// Remove runs on its own goroutine so an implementation that ignores
	// cancellation still cannot pin the attempt past the bound; the abandoned
	// goroutine finishes (or not) on its own, having been reported as leaked.
	done := make(chan error, 1)
	go func() { done <- ws.Remove(cleanupCtx) }()
	var err error
	select {
	case err = <-done:
	case <-cleanupCtx.Done():
		err = fmt.Errorf("workspace teardown exceeded %s: %w", workspaceTeardownTimeout, cleanupCtx.Err())
	}
	if err == nil {
		return
	}
	reportWorkspaceTeardownFailure(ctx, taskID, ws.Path(), err)
}

// reportWorkspaceTeardownFailure emits the durable diagnostics for a leaked
// workspace. Inside an activity it uses the worker's own logger and metrics
// handler (so the failure lands in the worker's log/metrics pipeline
// alongside the attempt it belongs to); outside one — engine unit tests, the
// in-process paths — it falls back to the default slog logger rather than
// panicking on the missing activity environment.
func reportWorkspaceTeardownFailure(ctx context.Context, taskID, path string, err error) {
	if activity.IsActivity(ctx) {
		activity.GetMetricsHandler(ctx).Counter(MetricWorkspaceTeardownFailure).Inc(1)
		activity.GetLogger(ctx).Error(
			"workspace teardown failed; the working copy leaked and must be swept",
			"stage", taskID, "workspace", path, "error", err.Error(),
		)
		return
	}
	slog.Error(
		"workspace teardown failed; the working copy leaked and must be swept",
		"stage", taskID, "workspace", path, "error", err.Error(),
	)
}

// refuseLeakedEnvelope is the dispatch-side assertion of the #2931 canary,
// applied where the serialized envelope is first back in Go hands: at
// activity entry, before any provisioning or execution. The envelope has just
// crossed the engine dispatch boundary exactly as serialized, so scrubbing
// its marshaled bytes against the exact-value registry answers the canary's
// question — "does any known credential value appear in this dispatch
// payload?" — and a hit refuses the stage rather than executing with a leaked
// credential in reach. The refusal is deliberately NON-RETRYABLE: a retry
// re-dispatches the identical envelope, so retrying converts a security
// tripwire into a retry-budget burn with the leak intact.
func (a *Activities) refuseLeakedEnvelope(env apiv1.InvocationEnvelope) error {
	if a.Canary == nil {
		return nil
	}
	data, err := json.Marshal(env)
	if err != nil {
		return classifySeamError(fmt.Errorf("marshal dispatch envelope for credential canary: %w", err))
	}
	if scrubbed := a.Canary.Scrub(data); !bytes.Equal(scrubbed, data) {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf(
				"engine: dispatch canary (#2931): a registered credential value appears in the serialized dispatch envelope for stage %q; dispatch payloads must carry opaque references only — refusing to execute",
				env.TaskID,
			),
			FailureTypeStage,
			nil,
		)
	}
	return nil
}

// InvokeGoober executes an agentic task in the workspace the task declares
// (Task.Workspace; "" keeps the historical writable repo worktree), the same
// mode the walk's continuity selector decided the delta from — so a stage
// declaring repo-readonly is cut a detached read-only checkout, is handed no
// delta, and can never publish a base-rooted bundle over its predecessor's
// entry. This is the local runner's taskWorkspaceMode parity; before it the
// engine hard-coded the writable repo here and honoured the declaration only
// in the selector, which is two readings of one field.
//
// workspaceDelta (#3803) and workspace are TRAILING POSITIONAL arguments,
// like workspaceBranch before them, rather than a struct replacing all of
// them. The Go SDK decodes activity arguments positionally and zero-fills
// parameters the recorded payload does not carry (converter.FromPayloads
// stops at the shorter side), so an activity scheduled by the previous engine
// build — an in-flight run at deploy — executes here with workspaceDelta ==
// "" and workspace == "" and behaves exactly as it did, and a history
// recorded with the two-argument shape replays under this code
// (TestContinuityPreChangeHistoryReplays). A struct in the second position
// would fail to decode those payloads.
func (a *Activities) InvokeGoober(ctx context.Context, env apiv1.InvocationEnvelope, workspaceBranch string, workspaceDelta string, workspace apiv1.WorkspaceMode, onTimeout string) (stageActivityResult, error) {
	if a.Goober == nil {
		return stageActivityResult{}, classifySeamError(ErrNotConfigured)
	}
	if err := a.refuseLeakedEnvelope(env); err != nil {
		return stageActivityResult{}, err
	}
	if workspace == "" {
		workspace = apiv1.WorkspaceRepo
	}
	ws, err := a.provisionWorkspace(ctx, &env, workspace, false, workspaceBranch, workspaceDelta)
	if err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	defer removeWorkspace(ctx, env.TaskID, ws)
	res, err := a.Goober.Invoke(ctx, env)
	if err != nil {
		// #724 salvage: an agentic session that ran out of wall clock has not
		// necessarily done nothing. If the task declared onTimeout: salvage
		// and the agent left COMMITTED work behind, the run keeps that work
		// and lets the downstream verification stage judge it, rather than
		// throwing away a real implementation because the harness ran long.
		//
		// The decision has to be made here, in the process holding the
		// workspace: the diff is only observable for the workspace's lifetime,
		// and the deferred teardown above is about to end it.
		if salvaged, marker, ok := a.salvageOnTimeout(ctx, ws, env, workspace, onTimeout, err); ok {
			result := stageActivityResult{ResultEnvelope: salvaged, SalvageMarker: marker}
			if perr := publishWorkspaceDelta(ctx, ws, workspace, &result); perr != nil {
				return stageActivityResult{}, classifySeamError(perr)
			}
			result.SelfPlacement = selfStagePlacement()
			return a.scrubStageActivityResult(result)
		}
		return stageActivityResult{}, classifySeamError(err)
	}
	// #3374: a stage may not claim DEPENDENCY_NOT_MET without having inspected
	// the context pointers it was handed. Validated HERE rather than in the
	// workflow because the evidence is the harness transcript, which lives in
	// the blob plane an activity can read and workflow code cannot. The walk
	// routes on the rejection (contextNotInspectedRedispatch); this only
	// decides whether there is one.
	res = a.validateDependencyResult(ctx, env, res)
	result := stageActivityResult{ResultEnvelope: res}
	// #3366: capture what the workspace is about to take to the grave. Taken
	// BEFORE publishWorkspaceDelta so a stage that failed — the case where the
	// capture actually matters, since a success publishes a bundle the next
	// stage continues from — still leaves its work in the journal.
	result.UnpushedDiff = captureUnpushedDiff(ctx, ws, workspace, env)
	if err := publishWorkspaceDelta(ctx, ws, workspace, &result); err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	result.SelfPlacement = selfStagePlacement()
	return a.scrubStageActivityResult(result)
}

// salvageOnTimeout implements the #724 ruling for the engine's self arm: a
// timed-out agentic attempt on a task declaring onTimeout: salvage, whose
// workspace holds a non-empty committed diff, SUCCEEDS with the salvage marker
// rather than failing.
//
// Every one of those conditions is load-bearing. Only a timeout salvages (a
// crashed harness proves nothing about the tree); only a declared salvage
// policy salvages (the default remains fail, because most timed-out stages
// really have produced nothing usable); and only a non-empty diff salvages,
// which is what keeps this from turning "the agent did nothing for an hour"
// into a green stage. The verifying stage downstream is what judges the
// salvaged work — the marker exists so an operator can see that is what
// happened.
func (a *Activities) salvageOnTimeout(
	ctx context.Context,
	ws Workspace,
	env apiv1.InvocationEnvelope,
	workspace apiv1.WorkspaceMode,
	onTimeout string,
	cause error,
) (apiv1.ResultEnvelope, []byte, bool) {
	if onTimeout != apiv1.TaskOnTimeoutSalvage || !invoke.IsTimeout(cause) {
		return apiv1.ResultEnvelope{}, nil, false
	}
	capture := captureUnpushedDiff(ctx, ws, workspace, env)
	if capture == nil || len(capture.Diff) == 0 {
		return apiv1.ResultEnvelope{}, nil, false
	}
	marker, err := runner.SalvageOnTimeoutMarker(len(capture.Diff), cause.Error())
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, false
	}
	return runner.SalvagedResult(), marker, true
}

// captureUnpushedDiff asks a writable repo workspace for the patch it is
// holding. Best-effort by construction: a workspace that cannot report a diff
// (scratch, read-only, a test fake, any provisioner predating DiffReader)
// reports none, and every caller treats none as "nothing to record" rather
// than as a failure. Capturing work is strictly additive to a stage's outcome
// and must never be able to change it.
func captureUnpushedDiff(ctx context.Context, ws Workspace, mode apiv1.WorkspaceMode, env apiv1.InvocationEnvelope) *UnpushedDiffCapture {
	if !writableWorkspace(mode) {
		return nil
	}
	reader, ok := ws.(DiffReader)
	if !ok {
		return nil
	}
	branch, baseRef, err := reader.Head(ctx)
	if err != nil {
		return nil
	}
	if baseRef == "" {
		baseRef = env.BaseBranch
	}
	diff, err := reader.Diff(ctx, baseRef)
	if err != nil || len(diff) == 0 {
		return nil
	}
	return &UnpushedDiffCapture{Diff: diff, Branch: branch, BaseRef: baseRef}
}

// ReviewGoober executes an agentic reviewer gate. Like the local runner, the
// reviewer runs a real goober subprocess and therefore gets a repository
// workspace (unlike an automated gate) — in the mode the gate declares
// (AgenticGate.Workspace; "" keeps the historical writable repo worktree),
// continuing from the delta the walk selected for it (#3803). A reviewer
// never publishes: it returns a Verdict, not a stage result, and must not
// commit. Both new arguments are trailing positionals for the replay reason
// InvokeGoober documents.
func (a *Activities) ReviewGoober(ctx context.Context, env apiv1.InvocationEnvelope, workspaceBranch string, workspaceDelta string, workspace apiv1.WorkspaceMode, priorDiffDigest string, subjectAgentic bool) (GateReviewResult, error) {
	if a.Goober == nil {
		return GateReviewResult{}, classifySeamError(ErrNotConfigured)
	}
	if err := a.refuseLeakedEnvelope(env); err != nil {
		return GateReviewResult{}, err
	}
	if workspace == "" {
		workspace = apiv1.WorkspaceRepo
	}
	ws, err := a.provisionWorkspace(ctx, &env, workspace, false, workspaceBranch, workspaceDelta)
	if err != nil {
		return GateReviewResult{}, classifySeamError(err)
	}
	defer removeWorkspace(ctx, env.TaskID, ws)

	// The subject diff (#3384), read from the workspace this reviewer was
	// already given rather than from a second one. It is both the evidence the
	// reviewer judges and — through the two short-circuits below — the reason
	// a reviewer sometimes must not be asked at all.
	out := captureGateDiff(ctx, ws, workspace, env)
	if a.Canary != nil && len(out.Diff) > 0 {
		// Defence in depth, as in the local runner's recordReviewerDiff: a
		// commit that captured a registered credential must not carry it into
		// the journal on its way to the reviewer.
		out.Diff = a.Canary.Scrub(out.Diff)
	}
	if len(out.Diff) > 0 {
		ref, err := journal.ArtifactRef(out.Diff)
		if err != nil {
			return GateReviewResult{}, classifySeamError(fmt.Errorf("address reviewer diff for gate %q: %w", env.TaskID, err))
		}
		out.DiffDigest = ref.Digest
		// The workflow records the artifact from these same bytes, so the
		// pointer handed to the reviewer here and the one the journal commits
		// address one blob by construction, not by agreement.
		env.ContextPointers = append(env.ContextPointers, apiv1.ContextPointer{
			Name:      runner.ReviewerDiffPointerName(gateNameFromTaskID(env)),
			Integrity: apiv1.IntegrityDerived,
			Artifact: &apiv1.ArtifactPointer{
				Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
				MediaType: runner.ReviewerDiffMediaType, Integrity: apiv1.IntegrityDerived,
			},
		})
	}
	// The two short-circuits, placed exactly where internal/gate's Evaluate
	// places them: after the diff is known and BEFORE the reviewer is called.
	// A synthesized verdict here is the whole point — the negative assertion
	// the parity rows make is that a.Goober.Review is never reached.
	if out.Observed && len(out.Diff) == 0 && subjectAgentic {
		// #415: an agentic stage asked to change something changed nothing.
		// No reviewer can turn that into a pass. Scoped to an agentic subject
		// because a DETERMINISTIC one legitimately produces no diff.
		verdict := gate.EmptyDiffVerdict()
		out.Verdict = verdict
		out.EmptyDiff = true
		return out, nil
	}
	if out.DiffDigest != "" && priorDiffDigest != "" && out.DiffDigest == priorDiffDigest {
		// #316: the stage was sent back and produced a byte-identical tree, so
		// the reviewer could only repeat its previous verdict. Resolving here
		// is what stops a non-convergent loop from spending the whole repass
		// budget on reviewer calls with a foregone conclusion.
		verdict := gate.DuplicateDiffVerdict(out.DiffDigest, nil)
		out.Verdict = verdict
		out.DuplicateDiff = true
		return out, nil
	}
	verdict, err := a.Goober.Review(ctx, env)
	if err != nil {
		return GateReviewResult{}, classifySeamError(err)
	}
	scrubbed, err := a.scrubVerdict(verdict)
	if err != nil {
		return GateReviewResult{}, err
	}
	out.Verdict = scrubbed
	out.Reviewed = true
	return out, nil
}

// GateReviewResult is what an agentic gate evaluation produced: the verdict
// plus the diff evidence it was reached from.
//
// apiv1.Verdict is EMBEDDED so the JSON stays flat, which is what keeps this
// change replay-safe: a history recorded when this activity returned a bare
// apiv1.Verdict decodes into the embedded field with every added field at its
// zero value, and the workflow then behaves exactly as it did — no diff
// artifact, no digest, no short-circuit.
type GateReviewResult struct {
	apiv1.Verdict
	// Diff is the subject patch, recorded by the workflow as the reviewer-diff
	// artifact. Empty when the branch has not moved or the workspace cannot
	// report one.
	Diff []byte `json:"diff,omitempty"`
	// DiffDigest addresses Diff. Empty exactly when Diff is.
	DiffDigest string `json:"diffDigest,omitempty"`
	// Observed distinguishes "the branch has not moved" (Observed, empty Diff)
	// from "this workspace cannot tell us" (not Observed). Only the first may
	// fast-fail a gate.
	Observed bool `json:"observed,omitempty"`
	// EmptyDiff / DuplicateDiff record which short-circuit synthesized the
	// verdict. Reviewed is their complement: true only when the reviewer goober
	// actually ran, which is what the negative parity rows assert on.
	EmptyDiff     bool `json:"emptyDiff,omitempty"`
	DuplicateDiff bool `json:"duplicateDiff,omitempty"`
	Reviewed      bool `json:"reviewed,omitempty"`
}

// gateNameFromTaskID recovers the state name from an envelope's TaskID, which
// the walk composes as "<runID>:<state>".
func gateNameFromTaskID(env apiv1.InvocationEnvelope) string {
	return strings.TrimPrefix(env.TaskID, env.RunID+":")
}

// captureGateDiff reads the patch a gate's workspace is holding, in the
// GateReviewResult shape its caller returns.
func captureGateDiff(ctx context.Context, ws Workspace, mode apiv1.WorkspaceMode, env apiv1.InvocationEnvelope) GateReviewResult {
	capture := captureUnpushedDiff(ctx, ws, mode, env)
	if capture == nil {
		if _, ok := ws.(DiffReader); !ok || !writableWorkspace(mode) {
			return GateReviewResult{}
		}
		// A DiffReader that reported nothing HAS observed the branch: the
		// distinction is what licenses the empty-diff fast-fail.
		return GateReviewResult{Observed: true}
	}
	return GateReviewResult{Diff: capture.Diff, Observed: true}
}

// RunDeterministic executes a deterministic task in the workspace mode
// run.Workspace carries (repo by default, scratch on request). The walk
// resolves the task's declared precedence into run.Workspace before
// dispatch (Task.EffectiveWorkspace), so a task-level `workspace:` reaches
// this provisioner too. workspaceDelta is a trailing positional for the
// replay reason InvokeGoober documents.
func (a *Activities) RunDeterministic(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun, workspaceBranch string, workspaceDelta string) (stageActivityResult, error) {
	if a.Det == nil {
		return stageActivityResult{}, classifySeamError(ErrNotConfigured)
	}
	if err := a.refuseLeakedEnvelope(env); err != nil {
		return stageActivityResult{}, err
	}
	// Dispatch distinguishes absent from zero-value (#626): no stage may run
	// without a command or script, whatever the workflow handed us.
	if len(run.Command) == 0 && run.Script == "" {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: stage %q has an empty run command and script; refusing to execute (fail closed)", env.TaskID))
	}
	ws, err := a.provisionWorkspace(ctx, &env, run.Workspace, run.SyncBase, workspaceBranch, workspaceDelta)
	if err != nil {
		var conflict *worktree.BaseSyncConflictError
		if errors.As(err, &conflict) {
			// Mirror the local runner's dispatchTask (#813): a genuine base
			// merge conflict is a business failure the definition routes (a
			// status-equals gate dispatching remediation), never a dispatch
			// error burning the retry budget.
			//
			// The conflict DETAIL rides back on the result (#3882) so the walk
			// can commit it as the same "<stage>/base-sync-conflict.json"
			// artifact the local runner records. Without it the remediation
			// stage the gate dispatches is told only that a merge conflicted,
			// never which files conflicted — which is the entire actionable
			// content of the failure.
			detail, derr := json.Marshal(runner.BaseSyncConflictDetail{
				Code:             runner.BaseSyncConflictErrorCode,
				Message:          err.Error(),
				Branch:           conflict.Branch,
				BaseRef:          conflict.BaseRef,
				ConflictingFiles: conflict.ConflictingFiles,
			})
			if derr != nil {
				return stageActivityResult{}, classifySeamError(fmt.Errorf("encode base-sync conflict detail for stage %q: %w", env.TaskID, derr))
			}
			return a.scrubStageActivityResult(stageActivityResult{
				ResultEnvelope: apiv1.ResultEnvelope{
					Status:  apiv1.ResultFailure,
					Summary: runner.BaseSyncConflictSummary,
					Error: &apiv1.ErrorInfo{
						Code:      runner.BaseSyncConflictErrorCode,
						Message:   err.Error(),
						Retryable: true,
					},
				},
				BaseSyncConflict: detail,
				SelfPlacement:    selfStagePlacement(),
			})
		}
		return stageActivityResult{}, classifySeamError(err)
	}
	defer removeWorkspace(ctx, env.TaskID, ws)
	res, err := a.Det.Run(ctx, env, run)
	if err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	mutations, issues := readMutationSidecar(ws.Path())
	result := stageActivityResult{ResultEnvelope: res, Mutations: mutations, MutationIssues: issues}
	if err := publishWorkspaceDelta(ctx, ws, run.Workspace, &result); err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	result.SelfPlacement = selfStagePlacement()
	return a.scrubStageActivityResult(result)
}

// selfStagePlacement is the in-process arms' placement provenance (#3875): what
// the WORKER executing this activity knows about its own substrate, in the
// journal.Placement shape the local runner already journals for a self attempt.
//
// Computed HERE, in the activity, and never in the workflow. workflow code may
// not read os.Hostname or the environment — a value that is not in history is
// not replayable, and the plan's own constraint for this step is that recorded
// histories replay unchanged. Coming back on the activity result means the fact
// IS in history: the workflow journals a value it was told, and a replay
// journals the identical one. (workflow.SideEffect would also be replay-safe
// for NEW runs and would break every history recorded before it, by adding a
// marker command the old history has no record of.)
//
// nil under zero-declaration invariance, and nil is what the workflow treats as
// "journal nothing" — so an install that declares no placement keeps producing
// the journals it produced before this existed, on both paths.
func selfStagePlacement() *journal.Placement {
	if !runner.PlacementDeclaredInEnvironment() {
		return nil
	}
	placement := runner.SelfPlacement()
	return &placement
}

// publishWorkspaceDelta is the self arm's PUBLISH half (#3803): after a stage
// SUCCEEDED on a writable repo workspace, ask the workspace to bundle what
// the stage committed and stamp the digest on the result for the walk's
// continuity record. Only success publishes — a failed stage's half-finished
// commits are not a base for the next stage, and the engine retries it from
// the last good delta — and only a workspace that implements DeltaPublisher
// can (scratch and test fakes do not, and publish nothing).
//
// A publish FAILURE fails the stage: the commits exist and nothing else will
// carry them to a pod, so reporting success would strand exactly the diff
// this mechanism protects — the same rule the pod's dispatch-exec applies.
func publishWorkspaceDelta(ctx context.Context, ws Workspace, mode apiv1.WorkspaceMode, result *stageActivityResult) error {
	if result.Status != apiv1.ResultSuccess || !writableWorkspace(mode) {
		return nil
	}
	publisher, ok := ws.(DeltaPublisher)
	if !ok {
		return nil
	}
	pub, err := publisher.PublishDelta(ctx)
	if err != nil {
		return fmt.Errorf("engine: stage committed work that could not be carried to the next stage: %w", err)
	}
	result.WorkspaceDelta = pub.Digest
	result.WorkspaceDeltaBase = pub.Base
	result.WorkspaceDeltaTip = pub.Tip
	result.WorkspaceDeltaUnchanged = pub.Unchanged && pub.Digest == ""
	return nil
}

func (a *Activities) scrubStageActivityResult(result stageActivityResult) (stageActivityResult, error) {
	// Activity return values are persisted in Temporal history before the final
	// journal writer can scrub them. Redact at this boundary so history and the
	// later projection commit the same bytes and therefore the same digests.
	data, err := json.Marshal(result)
	if err != nil {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("marshal stage result for history scrubbing: %w", err))
	}
	data = a.scrubber().Scrub(data)
	var scrubbed stageActivityResult
	if err := json.Unmarshal(data, &scrubbed); err != nil {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("decode scrubbed stage result: %w", err))
	}
	return scrubbed, nil
}

func (a *Activities) scrubVerdict(verdict apiv1.Verdict) (apiv1.Verdict, error) {
	data, err := json.Marshal(verdict)
	if err != nil {
		return apiv1.Verdict{}, classifySeamError(fmt.Errorf("marshal verdict for history scrubbing: %w", err))
	}
	data = a.scrubber().Scrub(data)
	var scrubbed apiv1.Verdict
	if err := json.Unmarshal(data, &scrubbed); err != nil {
		return apiv1.Verdict{}, classifySeamError(fmt.Errorf("decode scrubbed verdict: %w", err))
	}
	return scrubbed, nil
}

func (a *Activities) scrubber() journal.Scrubber {
	if a.Scrubber != nil {
		return a.Scrubber
	}
	return journal.NewPatternScrubber()
}

func readMutationSidecar(workspace string) (facts []mutationFact, issues []string) {
	full, err := apiv1.ResolveContainedPath(workspace, mutationsSidecarFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("resolve sidecar path: %v", err)}
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("read sidecar: %v", err)}
	}
	for i, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var fact mutationFact
		if err := json.Unmarshal(line, &fact); err != nil {
			issues = append(issues, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		if fact.Provider == "" || fact.Kind == "" || fact.ID == "" {
			// The provider action has already happened. Keep malformed
			// provenance observable without converting success into failure.
			issues = append(issues, fmt.Sprintf("line %d: provider, kind, and id are required", i+1))
			continue
		}
		facts = append(facts, fact)
	}
	return facts, issues
}

// EvaluateAutomated runs an automated gate check. Automated gates are pure
// functions over env.Inputs and never receive a workspace, matching the local
// runner (#112) — no provisioning here.
func (a *Activities) EvaluateAutomated(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	if a.Auto == nil {
		return "", classifySeamError(ErrNotConfigured)
	}
	if err := a.refuseLeakedEnvelope(env); err != nil {
		return "", err
	}
	outcome, err := a.Auto.Evaluate(ctx, gate, env)
	if err != nil {
		return "", classifySeamError(err)
	}
	return outcome, nil
}

// validateDependencyResult applies the #3374 context-inspection check to a
// stage result that claims DEPENDENCY_NOT_MET, converting an unsubstantiated
// claim into the CONTEXT_NOT_INSPECTED rejection the walk re-dispatches on.
//
// The verdict is runner.ValidateDependencyNotMetTranscript, unchanged and
// unduplicated: only the transcript RESOLUTION differs between the lanes (a
// run directory locally, the worker's span store here), and that is all this
// method does.
func (a *Activities) validateDependencyResult(ctx context.Context, env apiv1.InvocationEnvelope, result apiv1.ResultEnvelope) apiv1.ResultEnvelope {
	if !runner.AppliesDependencyValidation(result, env.ContextPointers) {
		return result
	}
	// Fail open with no span source or no transcript pointer: see the Spans
	// field's contract. A claim this activity cannot check is a claim it must
	// not reject.
	if a.Spans == nil || result.Transcript == nil {
		return result
	}
	transcript, err := a.Spans.Get(ctx, result.Transcript.Digest)
	if err != nil {
		return result
	}
	return runner.RejectDependencyResult(result,
		runner.ValidateDependencyNotMetTranscript(transcript, env.ContextPointers, result))
}
