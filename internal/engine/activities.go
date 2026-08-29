package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
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
	// Goobers-Review/Goobernetes-v1/decisions/0002): it asserts that no known
	// credential value appears in a serialized dispatch envelope, and refuses
	// to execute the stage when one does. Wire the EXACT-VALUE registry
	// (journal.RegistryScrubber — the same registry every resolver-issued and
	// credential-plane-minted value is registered with), never the pattern
	// net: a pattern can false-positive on legitimate stage inputs, and a
	// canary that can misfire on issue text would train operators to bypass
	// it. Nil disables the canary (the local runner path, which never
	// serializes an envelope off-process).
	Canary journal.Scrubber
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

type stageActivityResult struct {
	// Embed the legacy activity result so its JSON stays flat and histories
	// recorded before mutation metadata was added remain replay-decodable.
	apiv1.ResultEnvelope
	Mutations      []mutationFact `json:"mutations,omitempty"`
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
}

type mutationFact struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Operation string `json:"operation,omitempty"`
}

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
		return nil, fmt.Errorf("provision workspace for stage %q: %w", env.TaskID, err)
	}
	if ws == nil || ws.Path() == "" {
		removeWorkspace(ctx, ws)
		return nil, fmt.Errorf("workspace provisioner returned no path for stage %q (the closed invocation schema requires workspace)", env.TaskID)
	}
	env.Workspace = ws.Path()
	return ws, nil
}

// removeWorkspace tears one attempt's working copy down. Best-effort by
// design: a teardown failure never overrides the stage's own result/error
// (the local runner's additive removeErr contract, issue #136); until the
// history→journal projection (#629) exists there is no journal to surface it
// to. Detached from ctx so an already-expired attempt still cleans up.
func removeWorkspace(ctx context.Context, ws Workspace) {
	if ws == nil {
		return
	}
	_ = ws.Remove(context.WithoutCancel(ctx))
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
func (a *Activities) InvokeGoober(ctx context.Context, env apiv1.InvocationEnvelope, workspaceBranch string, workspaceDelta string, workspace apiv1.WorkspaceMode) (stageActivityResult, error) {
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
	defer removeWorkspace(ctx, ws)
	res, err := a.Goober.Invoke(ctx, env)
	if err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	result := stageActivityResult{ResultEnvelope: res}
	if err := publishWorkspaceDelta(ctx, ws, workspace, &result); err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	return a.scrubStageActivityResult(result)
}

// ReviewGoober executes an agentic reviewer gate. Like the local runner, the
// reviewer runs a real goober subprocess and therefore gets a repository
// workspace (unlike an automated gate) — in the mode the gate declares
// (AgenticGate.Workspace; "" keeps the historical writable repo worktree),
// continuing from the delta the walk selected for it (#3803). A reviewer
// never publishes: it returns a Verdict, not a stage result, and must not
// commit. Both new arguments are trailing positionals for the replay reason
// InvokeGoober documents.
func (a *Activities) ReviewGoober(ctx context.Context, env apiv1.InvocationEnvelope, workspaceBranch string, workspaceDelta string, workspace apiv1.WorkspaceMode) (apiv1.Verdict, error) {
	if a.Goober == nil {
		return apiv1.Verdict{}, classifySeamError(ErrNotConfigured)
	}
	if err := a.refuseLeakedEnvelope(env); err != nil {
		return apiv1.Verdict{}, err
	}
	if workspace == "" {
		workspace = apiv1.WorkspaceRepo
	}
	ws, err := a.provisionWorkspace(ctx, &env, workspace, false, workspaceBranch, workspaceDelta)
	if err != nil {
		return apiv1.Verdict{}, classifySeamError(err)
	}
	defer removeWorkspace(ctx, ws)
	verdict, err := a.Goober.Review(ctx, env)
	if err != nil {
		return apiv1.Verdict{}, classifySeamError(err)
	}
	return a.scrubVerdict(verdict)
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
			// error burning the retry budget. The conflict-detail artifact
			// stays a local-runner surface for now — the projection (#629)
			// commits workflow-derived bytes only.
			return a.scrubStageActivityResult(stageActivityResult{
				ResultEnvelope: apiv1.ResultEnvelope{
					Status:  apiv1.ResultFailure,
					Summary: "base synchronization conflicted; the implementation branch was preserved for remediation",
					Error: &apiv1.ErrorInfo{
						Code:      runner.BaseSyncConflictErrorCode,
						Message:   err.Error(),
						Retryable: true,
					},
				},
			})
		}
		return stageActivityResult{}, classifySeamError(err)
	}
	defer removeWorkspace(ctx, ws)
	res, err := a.Det.Run(ctx, env, run)
	if err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	mutations, issues := readMutationSidecar(ws.Path())
	result := stageActivityResult{ResultEnvelope: res, Mutations: mutations, MutationIssues: issues}
	if err := publishWorkspaceDelta(ctx, ws, run.Workspace, &result); err != nil {
		return stageActivityResult{}, classifySeamError(err)
	}
	return a.scrubStageActivityResult(result)
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
