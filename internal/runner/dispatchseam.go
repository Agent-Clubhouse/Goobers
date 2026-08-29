package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacedelta"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// dispatchseam.go is decision 003 ruling 1, plan step 6: the daemon's runner
// keeps driving every trigger-started run, and a stage whose pinned placement
// resolved to a NON-self runner leaves dispatchTask through the StageDispatcher
// seam — after the runner's own input resolution (requireLabels/assignedTo
// defaulting, the bandit assignment, inputsFrom, fan-in completeness) and
// before any workspace is provisioned. A stage with no pin, or a self pin,
// takes today's arms byte for byte.
//
// What the seam is NOT: it never creates a pod, never speaks Temporal, never
// reads Kubernetes. Its production implementation (plan step 7,
// cmd/goobers/daemondispatch.go) starts the engine's DispatchOne workflow and
// blocks on it; the daemon stays a Temporal client (decision 011). This file
// owns everything on the runner's side of that boundary: which stage routes,
// what it is refused for, what the request carries, and what is journaled and
// carried forward when the attempt settles.
//
// Continuity (ruling 5): the daemon's managed mirror is the run's continuity
// record. Before a placed writable-repo attempt is dispatched, the run branch
// is bundled base..<branch> from the mirror and PUT into the blob store the
// daemon already mounts; the request carries the digest. After surrender the
// pod's own bundle is fetched back and applied to the mirror fast-forward-only
// (worktree.Manager.ApplyBundle), so the next self consumer — a self review
// gate, #3366's unpushed-diff capture, the next placed stage's bundle — sees
// the pod's commits by construction. A diverged mirror fails closed.
//
// Restart (ruling 6): an interrupted placed attempt N is settled on resume by
// the seam's Describe/Await, never re-dispatched — see adoptPlacedAttempt.

// ErrStageDispatcherUnavailable is returned (wrapped) when a stage pinned to a
// non-self runner is reached and Config.StageDispatcher is nil: every type-1
// and type-2 install, every one-shot `goobers run`/`signal`, and a daemon
// rolled back after a run was pinned. Fail closed with a name, never fall
// through to the self arm — a restriction-requiring stage must not silently
// run on the daemon host (decision 003 ruling 7).
var ErrStageDispatcherUnavailable = errors.New("runner: no stage dispatcher is configured for a stage pinned to a non-self runner")

// stageDispatchPreflightState is the walk state a run fails at, before any
// stage executes, when its pins need the seam and none is configured.
const stageDispatchPreflightState = "stage-dispatch-preflight"

// StageDispatcher is the seam a non-self-pinned stage attempt executes
// through (decision 003 ruling 1). Every method is invoked under the runner's
// attempt context, which never cancels on SIGTERM (the drain contract), so an
// implementation may block for the stage's whole duration.
//
// Error classification contract, shared with the self arms: an error marked
// invoke.InfrastructureFailure retries on the runner's bounded infrastructure
// budget with a fresh attempt number; any other error is policy-classed and
// consumes the stage's declared retry budget. The business outcome only ever
// arrives in StageDispatchResult.Result, never as an error — a stage whose
// pod reported failure returns a ResultFailure envelope and a nil error,
// exactly as a self executor does.
type StageDispatcher interface {
	// Dispatch places one attempt and blocks until it settles or fails.
	Dispatch(ctx context.Context, req StageDispatchRequest) (StageDispatchResult, error)
	// Describe reports the state of an attempt this seam was asked to
	// dispatch earlier — possibly by a previous daemon process (ruling 6).
	Describe(ctx context.Context, runID, stage string, attempt int) (StageAttemptState, error)
	// Await re-attaches to an attempt Describe reported Completed or Running
	// and returns its settled result. req identifies the attempt exactly as
	// the original Dispatch did (RunID, stage, Attempt); its resolved inputs
	// are informational — the attempt already runs with the inputs it was
	// dispatched with.
	Await(ctx context.Context, req StageDispatchRequest) (StageDispatchResult, error)
}

// StageAttemptState is what Describe reports about a dispatched attempt.
type StageAttemptState string

const (
	// StageAttemptCompleted means the attempt settled with a result that
	// Await can return without blocking; the runner ADOPTS it (ruling 6).
	StageAttemptCompleted StageAttemptState = "completed"
	// StageAttemptRunning means the attempt is still executing; the runner
	// re-attaches through Await and adopts what it returns.
	StageAttemptRunning StageAttemptState = "running"
	// StageAttemptFailed means the dispatch itself failed with no adoptable
	// result; the runner takes today's interrupted-attempt path (N+1, infra).
	StageAttemptFailed StageAttemptState = "failed"
	// StageAttemptTimedOut means the dispatch exceeded its bound with no
	// adoptable result; same path as Failed.
	StageAttemptTimedOut StageAttemptState = "timed-out"
	// StageAttemptNotFound means the seam has no record of the attempt — the
	// daemon was interrupted before it was started, or the record expired;
	// same path as Failed.
	StageAttemptNotFound StageAttemptState = "not-found"
)

// adoptable reports whether the state carries (or will carry) a result the
// runner adopts as the attempt's outcome.
func (s StageAttemptState) adoptable() bool {
	return s == StageAttemptCompleted || s == StageAttemptRunning
}

// StageDispatchRequest is one placed stage attempt, fully resolved by the
// runner: everything the self arms would have handed an executor, minus the
// workspace (the pod provisions its own) and plus the pinned placement facts.
type StageDispatchRequest struct {
	// Task is the compiled task exactly as the walk sees it.
	Task apiv1.Task
	// Envelope is the invocation envelope after the runner's own input
	// resolution — backlog-query defaulting, the bandit assignment, inputsFrom
	// and fan-in completeness all applied to Envelope.Inputs. Envelope.
	// Workspace is deliberately empty.
	Envelope apiv1.InvocationEnvelope
	// Placement is the stage's pinned placement (never Self), in run.yaml's
	// spelling — dispatcher.PinnedPlacementFromJournal is the one conversion
	// to the dispatch wire type.
	Placement journal.PinnedPlacement
	// Run is the pinned DeterministicRun the pod executes; nil for an agentic
	// task.
	Run *apiv1.DeterministicRun
	// Workspace is the TASK-level workspace declaration (apiv1.Task.Workspace)
	// — the only place an agentic stage can express one. The effective mode is
	// apiv1.EffectiveWorkspace(Workspace, Run), the one resolution the engine's
	// dispatch input shares.
	Workspace apiv1.WorkspaceMode
	// WorkspaceBranch is the run branch (or the #392 rebound workspace
	// branch) a writable-repo attempt commits on.
	WorkspaceBranch string
	// BaseBranch is the run's base branch name.
	BaseBranch string
	// Attempt is the attempt number this dispatch carries (Envelope.Attempt
	// agrees), and AttemptClass its journal class.
	Attempt      int
	AttemptClass journal.AttemptClass
	// WorkspaceDelta is the blob digest of the run branch bundled from the
	// daemon mirror before dispatch; empty when the run has committed nothing
	// yet or the attempt's workspace is not a writable repo.
	WorkspaceDelta string
	// Journal is the runner's open handle on the run's journal, for the
	// implementation to loan to the live-journal writer for the attempt's
	// duration (livejournal.Writer.Adopt, ruling 5) so pod-plane emits append
	// through it instead of timing out on the run lock. The runner keeps
	// ownership; the implementation must never close it.
	Journal *journal.Run
}

// StageDispatchResult is a settled placed attempt.
type StageDispatchResult struct {
	// Result is the pod's surrendered envelope — the business outcome.
	Result apiv1.ResultEnvelope
	// Mutations are the provider mutations the stage reported committing.
	Mutations []MutationFact
	// MutationIssues names mutation records the pod could not read; journaled
	// best-effort exactly as a self attempt's sidecar issues are.
	MutationIssues []string
	// Placement is the substrate provenance of the attempt — which runner
	// served it, which pod carried it, which image ran, how long it queued —
	// journaled as runner.placement. Runner empty means the seam produced no
	// provenance (a refused placement has no runner to name).
	Placement journal.Placement
	// WorkspaceDelta is the blob digest of the bundle the pod surrendered
	// carrying what it committed, with the bundle's two SHAs beside it;
	// empty when the stage committed nothing. The runner applies it to the
	// mirror.
	WorkspaceDelta     string
	WorkspaceDeltaBase string
	WorkspaceDeltaTip  string
}

// MutationFact is one provider mutation a stage reported committing — the
// exported spelling of the sidecar record the self arms read, so the seam's
// result and the sidecar reader share one type (mutationFact is an alias).
type MutationFact struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// Runner-namespace keys and codes the seam authors.
const (
	// RunnerAnnotationWorkspaceDelta identifies a runner.annotation recording
	// that a placed attempt's surrendered workspace delta was applied to the
	// daemon mirror: the far-side record that the next consumer will see the
	// pod's commits at that SHA.
	RunnerAnnotationWorkspaceDelta = "workspace.delta"
	// errCodeWorkspaceDeltaPublish names a failure to bundle the run branch
	// from the mirror before dispatch.
	errCodeWorkspaceDeltaPublish = "workspace_delta_publish_failed"
	// errCodeWorkspaceDeltaApply names a failure to land the surrendered
	// delta on the mirror — including a diverged run branch, which is
	// refused with both SHAs rather than resolved by force.
	errCodeWorkspaceDeltaApply = "workspace_delta_apply_failed"
)

// placementFor returns the stage's pinned placement and whether it ROUTES to
// the seam. No pin, or a self pin, reports false — the caller's existing arms
// then run untouched, which is the zero-declaration invariance guard. Keyed
// by stage name, never by index (decision 001 ruling 6).
func placementFor(placements []journal.PinnedPlacement, stage string) (journal.PinnedPlacement, bool) {
	for i := range placements {
		if placements[i].Stage == stage {
			return placements[i], !placements[i].Self
		}
	}
	return journal.PinnedPlacement{}, false
}

// hasRoutedPlacement reports whether any pin routes to the seam.
func hasRoutedPlacement(placements []journal.PinnedPlacement) bool {
	for i := range placements {
		if !placements[i].Self {
			return true
		}
	}
	return false
}

// selfPinnedCapabilities filters the #735 toolchain preflight to the tokens a
// stage that still executes on THIS host declares. A toolchain only
// pod-pinned stages declare lives in those pods' images; probing the daemon
// host for it would fail a run over a stage the daemon never runs. A token
// declared by any self-pinned or unpinned stage, and a token no stage
// declares at all (the gaggle floor's own), stays. With no pins the filter is
// the identity.
func selfPinnedCapabilities(required []string, tasks []apiv1.Task, placements []journal.PinnedPlacement) []string {
	if len(required) == 0 || !hasRoutedPlacement(placements) {
		return required
	}
	podOnly := map[string]bool{}
	for i := range tasks {
		if _, routed := placementFor(placements, tasks[i].Name); routed {
			for _, c := range taskCapabilityTokens(tasks[i]) {
				podOnly[c] = true
			}
		}
	}
	for i := range tasks {
		if _, routed := placementFor(placements, tasks[i].Name); !routed {
			for _, c := range taskCapabilityTokens(tasks[i]) {
				delete(podOnly, c)
			}
		}
	}
	if len(podOnly) == 0 {
		return required
	}
	kept := make([]string, 0, len(required))
	for _, c := range required {
		if !podOnly[c] {
			kept = append(kept, c)
		}
	}
	return kept
}

// taskCapabilityTokens is the union of a stage's DSL 2.0 requiredCapabilities
// and DSL 3.0 runsOn.capabilities — the same two surfaces
// instance.WorkflowRequiredCapabilities unions into a run's requirement.
func taskCapabilityTokens(t apiv1.Task) []string {
	tokens := append([]string(nil), t.RequiredCapabilities...)
	if t.RunsOn != nil {
		tokens = append(tokens, t.RunsOn.Capabilities...)
	}
	return tokens
}

// journalRunHandle recovers the runner's open *journal.Run behind an
// executionJournal — the handle itself, or the one a parallel branch's
// journal wraps — for the seam to loan to the live-journal writer. Nil for
// any other implementation (a test double), in which case the seam receives
// no handle to adopt.
func journalRunHandle(jr executionJournal) *journal.Run {
	switch j := jr.(type) {
	case *journal.Run:
		return j
	case *branchJournal:
		return j.run
	}
	return nil
}

// baseBranchFor is the run's base branch name, defaulting to main exactly as
// buildEnvelope and createStageWorkspace do.
func baseBranchFor(in StartInput) string {
	if in.RepoRef.Branch != "" {
		return in.RepoRef.Branch
	}
	return "main"
}

// runBranchFor is the branch a writable-repo attempt of this run commits on:
// the #392 rebound workspace branch when one is in force, else the run's own
// branch — the same resolution createStageWorkspace applies.
func (r *Runner) runBranchFor(in StartInput, workspaceBranch string) string {
	if workspaceBranch != "" {
		return workspaceBranch
	}
	return providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
}

// refusePlacedStage applies the named refusals a placed stage must clear
// before the seam is consulted — the same guards the engine's
// dispatchRemoteTask applies, so the two dispatch paths cannot disagree on
// which stages are refused (decision 003 ruling 3):
//
//   - a deterministic stage with no run, or with neither command nor script,
//     and a workspace mode the pod substrate has never had, are returned as
//     ERRORS: bugs in how the stage was built, policy-classed;
//   - a stage that reads the instance config directory is an error too;
//   - a stage that needs the daemon's instance root — a ledger, journal-read
//     or telemetry-rollup command, or a built-in kind with no pod-side path
//     (executor.StageRequiresInstanceRoot) — is returned as a JOURNALED
//     ResultFailure carrying executor.StageRequiresInstanceRootCode, so
//     Task.ContinueOnError and gate branching apply exactly as they would to
//     a real executor failure.
//
// env.Inputs is read, not t.Inputs: a stage may declare its kind dynamically
// via inputsFrom, and the caller has already resolved that overlay.
func refusePlacedStage(t apiv1.Task, env apiv1.InvocationEnvelope) (*apiv1.ResultEnvelope, error) {
	if ws := apiv1.EffectiveWorkspace(t.Workspace, t.Run); ws != "" && ws != apiv1.WorkspaceScratch && !ws.IsRepoBacked() {
		return nil, fmt.Errorf("task %q declares workspace %q, which stage dispatch cannot provision in a pod", t.Name, ws)
	}
	if t.Type != apiv1.TaskDeterministic {
		return nil, nil
	}
	if t.Run == nil {
		return nil, fmt.Errorf("task %q is deterministic but declares no DeterministicRun", t.Name)
	}
	if len(t.Run.Command) == 0 && t.Run.Script == "" {
		return nil, fmt.Errorf("task %q run declares no command or script; refusing to dispatch an empty command or script", t.Name)
	}
	if executor.StageRequiresInstanceConfig(t.Run.Command) {
		return nil, fmt.Errorf("task %q runs %v, which reads the instance config directory; a stage pod has no config directory — place this stage on a self runner", t.Name, t.Run.Command)
	}
	kind, _ := env.Inputs[executor.InputKind].(string)
	kind = strings.TrimSpace(kind)
	if !executor.StageRequiresInstanceRoot(t.Run.Command, kind) {
		return nil, nil
	}
	reason := fmt.Sprintf(
		"task %q runs %v, which reads or writes the daemon's instance root (the file claim ledger, a merge lock, or an on-disk run journal); a stage pod has none — place this stage on a self runner",
		t.Name, t.Run.Command)
	if kind != "" && kind != executor.KindShell {
		reason = fmt.Sprintf(
			"task %q declares inputs.kind=%q, a built-in stage kind with no pod-side execution path (internal/executor dispatches it in-process only); place this stage on a self runner",
			t.Name, kind)
	}
	return &apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: "stage requires the daemon's instance root; refused before dispatch",
		Error: &apiv1.ErrorInfo{
			Code:      executor.StageRequiresInstanceRootCode,
			Message:   reason,
			Retryable: false,
		},
	}, nil
}

// placedAttemptRequest builds the seam request for one attempt from
// already-resolved task inputs: the workspace-free envelope core, the
// declared-input overlay (inputsFrom, fan-in), and the attempt facts the self
// arms stamp after buildEnvelope. No workspace is provisioned.
func (r *Runner) placedAttemptRequest(jr executionJournal, in StartInput, t apiv1.Task, pin journal.PinnedPlacement, taskInputs map[string]string, taskLimits apiv1.Limits, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, completed stageOutputs, fanIn *parallelExec, attempt int, class journal.AttemptClass, instructionAddendum, workspaceBranch string) (StageDispatchRequest, error) {
	env := r.stageEnvelope(in, t.Name, t.Goal, taskInputs, t.Capabilities, taskLimits, upstream)
	env.MinimumIntegrity = t.MinimumIntegrity
	env.Attempt = int32(attempt)
	env.OwnershipBoundary = "task:" + t.Name
	env.PolicyActions = append([]string(nil), t.PolicyActions...)
	env.NestedAgentPolicy = t.NestedAgentPolicy
	if t.NestedAgentPolicy != nil {
		parent := apiv1.StagePlatformAuthority(env, "result")
		env.ParentPlatformPolicy = &parent
	}
	env.InstructionAddendum = instructionAddendum
	if err := resolveDeclaredInputs(&env, t, in, upstreamResult, completed, fanIn); err != nil {
		return StageDispatchRequest{}, err
	}
	return StageDispatchRequest{
		Task:            t,
		Envelope:        env,
		Placement:       pin,
		Run:             t.Run,
		Workspace:       t.Workspace,
		WorkspaceBranch: r.runBranchFor(in, workspaceBranch),
		BaseBranch:      baseBranchFor(in),
		Attempt:         attempt,
		AttemptClass:    class,
		Journal:         journalRunHandle(jr),
	}, nil
}

// dispatchPlacedTask is dispatchTask's pod branch: refuse what cannot be
// placed, carry the run branch forward, hand the attempt to the seam, and
// settle what comes back. Returns exactly what the self arms return to
// dispatchTask's caller — an envelope, its mutation facts, and a
// classified error.
func (r *Runner) dispatchPlacedTask(ctx context.Context, jr executionJournal, in StartInput, t apiv1.Task, pin journal.PinnedPlacement, taskInputs map[string]string, taskLimits apiv1.Limits, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, completed stageOutputs, fanIn *parallelExec, attempt int, class journal.AttemptClass, instructionAddendum, workspaceBranch string, branchRecorded *bool) (apiv1.ResultEnvelope, []mutationFact, error) {
	seam := r.cfg.StageDispatcher
	if seam == nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("%w: stage %q is pinned to queue %q", ErrStageDispatcherUnavailable, t.Name, pin.Queue)
	}
	req, err := r.placedAttemptRequest(jr, in, t, pin, taskInputs, taskLimits, upstream, upstreamResult, completed, fanIn, attempt, class, instructionAddendum, workspaceBranch)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, err
	}
	refusal, err := refusePlacedStage(t, req.Envelope)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, err
	}
	if refusal != nil {
		return *refusal, nil, nil
	}
	if err := recordContextManifest(jr, req.Envelope, t.Name, attempt, class); err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q: record context manifest: %w", t.Name, err)
	}
	if apiv1.EffectiveWorkspace(t.Workspace, t.Run).IsWritableRepo() {
		digest, err := r.publishRunBranchDelta(ctx, in, req.WorkspaceBranch, req.BaseBranch)
		if err != nil {
			return apiv1.ResultEnvelope{}, nil, invoke.InfrastructureFailure(codedStageFailure(errCodeWorkspaceDeltaPublish, fmt.Errorf("task %q: publish run branch for dispatch: %w", t.Name, err)))
		}
		req.WorkspaceDelta = digest
	}
	res, err := seam.Dispatch(ctx, req)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, err
	}
	return r.settlePlacedAttempt(ctx, jr, in, t, req, res, workspaceBranch, branchRecorded)
}

// publishRunBranchDelta bundles base..<branch> from the daemon mirror and
// puts it in the blob store, returning the digest — or "" when the mirror
// holds no run branch yet, or holds one with nothing beyond base (a run whose
// earlier stages committed nothing has nothing to carry).
func (r *Runner) publishRunBranchDelta(ctx context.Context, in StartInput, branch, base string) (string, error) {
	if r.cfg.WorkspaceDeltas == nil {
		return "", errors.New("no workspace-delta blob store is configured")
	}
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return "", err
	}
	b, err := r.cfg.Worktrees.BundleRunBranch(ctx, repoURL, branch, base)
	if err != nil {
		if errors.Is(err, worktree.ErrRunBranchAbsent) || errors.Is(err, worktree.ErrRunBranchUnchanged) {
			return "", nil
		}
		return "", err
	}
	if err := r.cfg.WorkspaceDeltas.Put(ctx, b.Digest, b.Data); err != nil {
		return "", fmt.Errorf("put workspace delta %s: %w", b.Digest, err)
	}
	return b.Digest, nil
}

// settlePlacedAttempt is what happens once the seam returns a settled
// attempt: fail closed on a partial envelope, journal the real placement
// provenance, land the surrendered delta on the mirror (and record the run
// branch it created), and surface mutation-record issues the way the self
// arm does. Shared by the live dispatch and by resume's adoption so an
// adopted attempt is journaled exactly as a live one.
func (r *Runner) settlePlacedAttempt(ctx context.Context, jr executionJournal, in StartInput, t apiv1.Task, req StageDispatchRequest, res StageDispatchResult, workspaceBranch string, branchRecorded *bool) (apiv1.ResultEnvelope, []mutationFact, error) {
	if res.Result.Status == "" {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q attempt %d: the dispatched result carries no status; refusing to project a partial envelope (fail closed)", t.Name, req.Attempt)
	}
	if res.Placement.Runner != "" {
		if err := jr.Append(journal.PlacementEvent(t.Name, req.Attempt, req.AttemptClass, res.Placement)); err != nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal placement for %q: %w", t.Name, err)
		}
	}
	if len(res.MutationIssues) > 0 {
		// Best-effort, matching the self arm (issue #228/#2029): the
		// mutation already happened for real; a lost record must be
		// observable, never fatal.
		_ = jr.Append(journal.Event{
			Type: journal.EventError, Stage: t.Name, Attempt: req.Attempt, AttemptClass: req.AttemptClass,
			Error: &journal.ErrorDetail{Code: "mutation_sidecar_read_failed", Message: strings.Join(res.MutationIssues, "; ")},
		})
	}
	if res.WorkspaceDelta != "" {
		outcome, err := r.applySurrenderedDelta(ctx, in, req, res, workspaceBranch)
		if err != nil {
			return apiv1.ResultEnvelope{}, nil, codedStageFailure(errCodeWorkspaceDeltaApply, fmt.Errorf("task %q attempt %d: %w", t.Name, req.Attempt, err))
		}
		if err := jr.Append(journal.Event{
			Type: journal.EventRunnerAnnotation, Stage: t.Name, Attempt: req.Attempt, AttemptClass: req.AttemptClass,
			Runner: map[string]any{
				"kind":    RunnerAnnotationWorkspaceDelta,
				"digest":  res.WorkspaceDelta,
				"base":    res.WorkspaceDeltaBase,
				"tip":     res.WorkspaceDeltaTip,
				"branch":  req.WorkspaceBranch,
				"outcome": outcome.Outcome.String(),
				"before":  outcome.Before,
				"after":   outcome.After,
			},
		}); err != nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal workspace delta for %q: %w", t.Name, err)
		}
		if branchRecorded != nil && !*branchRecorded && machineUsesRepo(in.Machine) && outcome.After != "" {
			branchInput := in
			branchInput.WorkspaceBranch = req.WorkspaceBranch
			branchInput.WorkspaceBranchSHA = outcome.After
			if err := r.recordRunBranch(jr, branchInput); err != nil {
				return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal run branch for %q: %w", in.RunID, err)
			}
			*branchRecorded = true
		}
	}
	return res.Result, res.Mutations, nil
}

// applySurrenderedDelta fetches the pod's bundle from the blob store,
// verifies it, and lands it on the mirror's run branch fast-forward-only.
func (r *Runner) applySurrenderedDelta(ctx context.Context, in StartInput, req StageDispatchRequest, res StageDispatchResult, workspaceBranch string) (worktree.ApplyOutcome, error) {
	if r.cfg.WorkspaceDeltas == nil {
		return worktree.ApplyOutcome{}, errors.New("no workspace-delta blob store is configured to read the surrendered delta from")
	}
	data, err := r.cfg.WorkspaceDeltas.Get(ctx, res.WorkspaceDelta)
	if err != nil {
		return worktree.ApplyOutcome{}, fmt.Errorf("get surrendered workspace delta %s: %w", res.WorkspaceDelta, err)
	}
	b, err := workspacedelta.Load(data, res.WorkspaceDelta)
	if err != nil {
		return worktree.ApplyOutcome{}, err
	}
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return worktree.ApplyOutcome{}, err
	}
	return r.cfg.Worktrees.ApplyBundle(ctx, worktree.ApplyBundleOptions{
		RepoURL:             repoURL,
		Branch:              req.WorkspaceBranch,
		BaseRef:             req.BaseBranch,
		OwnerRunID:          in.RunID,
		AcquireRemoteBranch: workspaceBranch != "",
	}, b, nil)
}

// adoptPlacedAttempt settles an interrupted placed attempt on resume
// (decision 003 ruling 6): the pod outlives the daemon, so attempt N may have
// completed or may still be running. Describe decides — Completed or Running
// adopts what Await returns, journaled exactly as a live attempt (placement
// provenance, delta applied, stage.finished); Failed, TimedOut or NotFound
// reports adopted=false so the caller takes today's interrupted path (N+1 on
// the infrastructure budget). An unreadable state fails the resume closed:
// starting N+1 over an attempt that may already have mutated a provider is
// the double execution this seam exists to prevent.
//
// Inputs are re-resolved without a fresh bandit assignment — the attempt
// already runs with the inputs it was dispatched with, and journaling a
// second assignment for the same attempt would be a lie.
func (r *Runner) adoptPlacedAttempt(ctx context.Context, jr executionJournal, in StartInput, t apiv1.Task, pin journal.PinnedPlacement, attempt int, class journal.AttemptClass, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, completed stageOutputs, fanIn *parallelExec, workspaceBranch string, branchRecorded *bool) (apiv1.ResultEnvelope, []apiv1.ContextPointer, bool, error) {
	seam := r.cfg.StageDispatcher
	if seam == nil {
		return apiv1.ResultEnvelope{}, nil, false, fmt.Errorf("%w: cannot settle interrupted attempt %d of stage %q", ErrStageDispatcherUnavailable, attempt, t.Name)
	}
	state, err := seam.Describe(ctx, in.RunID, t.Name, attempt)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, false, fmt.Errorf("describe dispatched attempt %d of stage %q: %w", attempt, t.Name, err)
	}
	if !state.adoptable() {
		return apiv1.ResultEnvelope{}, nil, false, nil
	}
	upstream = apiv1.SelectContextPointers(upstream, t.ContextFrom)
	taskInputs, err := workflow.TaskInvocationInputs(in.Machine, t)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, false, fmt.Errorf("project stage %q inputs: %w", t.Name, err)
	}
	taskInputs = defaultBacklogQueryAssignedTo(t, taskInputs, r.cfg.BacklogQueryAssignedTo)
	taskInputs = defaultBacklogQueryRequireLabels(t, taskInputs, r.cfg.BacklogQueryRequireLabels)
	taskLimits, err := workflow.TaskLimits(in.Machine, t)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, false, fmt.Errorf("project stage %q limits: %w", t.Name, err)
	}
	req, err := r.placedAttemptRequest(jr, in, t, pin, taskInputs, taskLimits, upstream, upstreamResult, completed, fanIn, attempt, class, "", workspaceBranch)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, false, err
	}
	attemptCtx, heartbeat := r.startStageHeartbeat(ctx, jr, t.Name, attempt, class)
	res, awaitErr := seam.Await(attemptCtx, req)
	var result apiv1.ResultEnvelope
	var mutations []mutationFact
	if awaitErr == nil {
		result, mutations, awaitErr = r.settlePlacedAttempt(ctx, jr, in, t, req, res, workspaceBranch, branchRecorded)
	}
	if err := finishTaskDispatch(jr, heartbeat, t.Name, attempt, class, mutations, nil); err != nil {
		return apiv1.ResultEnvelope{}, nil, false, err
	}
	if awaitErr != nil {
		// The attempt failed after all: record why, the way runTask records
		// a failed dispatch, and let the caller take the interrupted path.
		errorCode, errorClass := classifyDispatchFailure(awaitErr)
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: t.Name, Attempt: attempt, AttemptClass: class,
			Error: &journal.ErrorDetail{Code: "executor_error", Message: awaitErr.Error()},
			Runner: map[string]any{
				retryFailureClassKey: string(dispatchRetryFailureClass(awaitErr)),
				stageErrorCodeKey:    errorCode,
				stageErrorClassKey:   string(errorClass),
			},
		}); aerr != nil {
			return apiv1.ResultEnvelope{}, nil, false, fmt.Errorf("runner: journal executor error for %q: %w", t.Name, aerr)
		}
		return apiv1.ResultEnvelope{}, nil, false, nil
	}
	result, err = r.settleStageResult(jr, in, t, attempt, class, result, upstream, upstreamResult, completed, fanIn)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, false, err
	}
	return result, contextPointersFor(t.Name, result.Artifacts), true, nil
}
