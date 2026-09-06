package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
)

// dispatchstage.go is the mode-3 engine cutover (#3588): the seam through
// which a stage whose pinned placement resolved to a non-self runner executes
// in a dispatcher-created pod (goobernetes-architecture.md §3/§5,
// goobernetes-dispatcher.md) instead of in-process on the worker.
//
// The determinism constraint is load-bearing and shapes everything here
// (architecture D8: "placement visible to the workflow function must be a
// pure function of declared inputs"): the WORKFLOW never solves, never reads
// instance config, never touches Kubernetes. Placement is resolved once at
// run start (bootstrap.PinStagePlacements — the same WF-016 snapshot that
// pins the definition) and carried in RunInput; runTask reads it as data and
// routes the dispatch ACTIVITY, which is where all K8s I/O lives.

// PinnedPlacement is one task's resolved execution placement, pinned into
// RunInput at run start.
//
// The type itself now lives in internal/dispatcher (decision 003 ruling 2):
// the daemon's runner pins the same list and hands one entry to DispatchOne,
// so the contract belongs beside the eligible-runner set it carries rather
// than inside the engine that used to be its only consumer. This ALIAS — not
// a distinct named type — is what keeps every existing reference, every
// recorded Temporal history, and every persisted RunInput identical: an alias
// is the same type, so no conversion exists to get wrong.
type PinnedPlacement = dispatcher.PinnedPlacement

// remotePlacementFor returns the stage's pinned placement and whether it
// routes to the dispatch activity. A stage with no pinned placement, or one
// pinned to self, reports false — the caller's existing arms then run
// untouched, which is the zero-declaration invariance guard
// (goobernetes-architecture.md §11 item 1).
func remotePlacementFor(in RunInput, stage string) (PinnedPlacement, bool) {
	for i := range in.Placements {
		if in.Placements[i].Stage == stage {
			return in.Placements[i], !in.Placements[i].Self
		}
	}
	return PinnedPlacement{}, false
}

// DispatchStageInput is ActDispatchStage's activity input: the stage's fully
// built invocation envelope, the pinned placement facts, and — for a
// deterministic task — the pinned DeterministicRun content the pod actually
// executes (#3699). Pure data — the workflow resolves nothing at dispatch
// time; Run is read from the pinned Definition (apiv1.Task.Run), the same
// WF-016 snapshot Placement itself is pinned from, so carrying it here adds
// no new nondeterminism. Deliberately NOT added to apiv1.InvocationEnvelope
// (the DSL/CRD-shared wire contract): this type is Temporal activity input
// only, never DSL-visible.
//
// EXPORTED for decision 003 ruling 2: the daemon's runner builds one of these
// per placed stage attempt and starts DispatchOne with it, so the shape has to
// be nameable outside this package. The JSON TAGS ARE UNCHANGED by that
// export and must stay so — they are recorded verbatim in the
// ActivityTaskScheduled event of every history the engine has already
// written, and an existing history must replay identically
// (dispatchone_test.go's recorded-history fixture is the guard).
type DispatchStageInput struct {
	Envelope  apiv1.InvocationEnvelope `json:"envelope"`
	Placement PinnedPlacement          `json:"placement"`
	Run       *apiv1.DeterministicRun  `json:"run,omitempty"`
	// Workspace is the TASK-level declaration, which is the only place an
	// agentic stage can express one — it carries no DeterministicRun.
	Workspace apiv1.WorkspaceMode `json:"workspace,omitempty"`
	// WorkspaceDelta is the blob digest of what earlier stages of this run
	// committed (#3763), for this pod to continue from.
	WorkspaceDelta string `json:"workspaceDelta,omitempty"`
	// WorkspaceBranch is the run's CURRENT workspace-branch binding (#392):
	// empty while the run is on its derived run branch, and the rebound branch
	// once a stage has published the well-known `workspaceBranch` output —
	// pr-remediation's gather-pr-context binds it to the claimed PR's head, and
	// every stage after it (rebase-pr, remediation-checkpoint, implement,
	// local-ci, push-remediated) operates on the PR branch rather than the run
	// branch.
	//
	// It is threaded HERE rather than derived in the pod because the pod cannot
	// derive it: providers.BranchNameIn composes the run branch out of
	// workflow + runID, which is precisely the branch a rebound run is NOT on.
	// Before this field a pod stage in pr-remediation checked out
	// goobers/pr-remediation/<runid>, found none of the PR's commits, and
	// remediated a branch nobody was reviewing — while the same stage on the
	// self runner was correct, because the local runner has threaded this
	// binding since #392.
	//
	// omitempty, and the JSON name is new: an existing recorded history carries
	// no such key and decodes to "" — the pre-#392 behaviour byte for byte.
	WorkspaceBranch string `json:"workspaceBranch,omitempty"`
	// Review marks an agentic reviewer GATE evaluation (decision 001 rulings
	// 7–8): the pod drives the gate's reviewer goober in review mode and
	// surrenders a Verdict, which DispatchStage re-validates and returns as
	// DispatchStageResult.Verdict. Run must be nil (a review has no command)
	// and the activity refuses the combination. Additive and omitempty, so
	// every history recorded before it — task dispatches all — decodes with
	// Review false and replays through the task path unchanged.
	Review bool `json:"review,omitempty"`
	// OwningWorkflowID is the id of the Temporal workflow execution whose
	// activity is creating this pod — the one execution whose liveness
	// decides whether the attempt is still being driven.
	//
	// It is set by the WORKFLOW immediately before ExecuteActivity, from
	// workflow.GetInfo(ctx).WorkflowExecution.ID (deterministic and
	// replay-safe), never by the activity and never by the caller who builds
	// the payload: DispatchOne overwrites whatever the daemon put here with
	// its own execution id, so the field cannot be forged or left stale.
	// Both drivers therefore stamp the id that actually owns the dispatch:
	//
	//   - DispatchOne (runner-driven)  -> <runID>/<stage>/<attempt>
	//   - the engine's own Run walk    -> the run workflow's id, which is
	//     RunID for a directly started run and claimID+"-run" for a
	//     SCHEDULED one (ClaimScheduled's child; engine.go).
	//
	// That second shape is why this field exists rather than the sweep
	// composing an id from the pod's run/stage/attempt: RunScheduled rewrites
	// in.RunID to RunID(claimID), a sha256 prefix no describe can ever find
	// (liveness.go says so outright). A sweep composing ids from that hash
	// asks about two executions that do not exist, reads NotFound as "nobody
	// is driving this", and deletes the pod of a LIVE scheduled stage. The
	// owning id is the one identity that is never a lossy address.
	//
	// Empty is legal and means "unstamped": the dispatcher then stamps no
	// owning-workflow annotation, and an unstamped pod is one the orphan
	// sweep refuses to address at all (dispatcher.podAttempt) — left in place
	// for activeDeadlineSeconds to reclaim. Fail toward leaving, never toward
	// deleting. It carries the omitempty tag and is additive: an
	// ActivityTaskScheduled payload Temporal already wrote decodes with it
	// zero, which is exactly that unstamped case.
	OwningWorkflowID string `json:"owningWorkflowID,omitempty"`
}

// gatePodAttempt names the pod attempt number for one dispatch of a reviewer
// gate: the gate's dispatch ordinal within the run, counted across repasses
// and infrastructure retries alike. It has to be unique per (run, gate)
// because it is the surrender-plane key and the pod name (D1: one attempt,
// one pod), and it has to be a pure function of walk state because the
// workflow replays. The repass count alone cannot serve — it resets on pass
// — so the walk keeps a dedicated per-gate dispatch counter.
func gatePodAttempt(gateDispatches map[string]int, gate string) int {
	gateDispatches[gate]++
	return gateDispatches[gate]
}

// dispatchRemoteGate drives one non-self agentic reviewer gate through the
// dispatch activity (decision 001 rulings 7–8) and returns the verdict the
// pod surrendered. It is the gate-shaped twin of dispatchRemoteTask: the
// same activity, the same pinned queue, the same surrender plane — the
// difference is the completion contract (Review) and what comes back (a
// Verdict, never a stage result).
//
// The retry loop is the caller's evaluateWithInfraRetry, exactly as for the
// self arm's ActReviewGoober: each call here is one pod attempt, numbered
// from the walk's per-gate dispatch counter so a retried evaluation never
// reuses a surrendered key. The workspace is the gate's own declaration
// with the self arm's default (ReviewGoober: "" is the writable repo
// worktree) resolved HERE, once, before dispatch — the pod stamps no
// workspace for an empty mode, and a reviewer cut an empty directory would
// truthfully report the repository missing. workspaceDelta is what the
// continuity selector chose for this gate (selectGateDelta's nil-repoFrom
// arm); DispatchStage withholds it from a read-only mode as it does for
// tasks.
//
// workspaceBranch is the run's rebound branch (#392), threaded here for the
// same reason dispatchRemoteTask carries it: a reviewer gate placed in a pod
// during pr-remediation must read the claimed PR's head, not the derived run
// branch a pod would otherwise compose, or it reviews a tree nobody proposed.
// It is handed to DispatchStage unconditionally, exactly as the self arm
// passes workspaceBranch to ActReviewGoober unconditionally — DispatchStage's
// own writable-repo gate (workspace.IsWritableRepo(), dispatchstage.go) is
// what keeps it off a repo-readonly gate's attempt, the same gate a task's
// WorkspaceBranch already goes through, so the two arms cannot disagree about
// when the branch is safe to hand the pod.
//
// #3844's instance-root refusal list is command-keyed and a gate declares
// no command, so there is nothing of it to apply here; the pod entrypoint's
// backstop still stands for anything a reviewer's harness might spawn.
func dispatchRemoteGate(ctx workflow.Context, g apiv1.Gate, env apiv1.InvocationEnvelope, placement PinnedPlacement, workspaceBranch, workspaceDelta string, podAttempt int) (apiv1.Verdict, error) {
	workspace := g.EffectiveWorkspace()
	if workspace == "" {
		workspace = apiv1.WorkspaceRepo
	}
	attemptEnv := env
	attemptEnv.Attempt = int32(podAttempt)
	var result stageActivityResult
	// OwningWorkflowID is read here, inside the workflow, for the same reason
	// dispatchRemoteTask reads it in its own retry closure: this walk's
	// execution IS the attempt's driver, and a scheduled run's id
	// (claimID+"-run") cannot be reconstructed from the pod's labels alone.
	if err := workflow.ExecuteActivity(ctx, ActDispatchStage, DispatchStageInput{
		Envelope:         attemptEnv,
		Placement:        placement,
		Workspace:        workspace,
		WorkspaceDelta:   workspaceDelta,
		WorkspaceBranch:  workspaceBranch,
		Review:           true,
		OwningWorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,
	}).Get(ctx, &result); err != nil {
		return apiv1.Verdict{}, err
	}
	if result.Verdict == nil {
		// DispatchStage never returns a review result without one; a
		// version-skewed activity host that ignores Review would. Refuse
		// rather than route on an empty decision.
		return apiv1.Verdict{}, fmt.Errorf("gate %q: the dispatch activity returned no verdict for a review attempt (activity host predates gate placement?); refusing to route on nothing", g.Name)
	}
	if result.Verdict.Decision == "" {
		// The workflow-side re-assertion of the activity's fail-closed rule
		// (validateSurrenderedVerdict): an activity host that predates the
		// check could hand back a verdict with nothing to route on, and the
		// one field the walk branches on is re-read here so a version skew
		// between worker and workflow can never route a run on an empty
		// decision.
		return apiv1.Verdict{}, fmt.Errorf("gate %q: the dispatch activity returned a verdict with an empty decision for pod attempt %d; refusing to route the run on it (fail closed)", g.Name, podAttempt)
	}
	return *result.Verdict, nil
}

// dispatchRemoteTask drives one non-self task through the dispatch activity
// under the same retry loop, journaling, and integrity grading as the local
// arms. It re-asserts the deterministic-task fail-closed guards (#626/#156)
// with the local arm's exact diagnostics: a stage with no command is refused
// here too, never shipped to a pod.
//
// v1 scope (#3699): a task the pod cannot faithfully run is refused HERE,
// before a pod is ever created, rather than silently diverging from
// local-executor behavior in the pod (the shipped disposal gate would just as
// soon fail such an attempt after the fact — refusing at dispatch is the same
// outcome without spending a pod). Each is a stated, narrowing cut.
//
// CLOSED since: credential injection (#3722), journal/artifact emission
// (#3723), and goobers-CLI/providerstage wiring (this change) — the pod
// resolves its own credentials, emits its own journal, and receives the run
// context a CLI stage reads. The CLI refusal that stood here is gone.
//
// STILL OPEN, and the only remaining refusal below: no pod-side repo checkout,
// so a stage declaring a workspace other than scratch is still refused.
func dispatchRemoteTask(ctx workflow.Context, in RunInput, t apiv1.Task, rec *runJournal, env apiv1.InvocationEnvelope, placement PinnedPlacement, produced apiv1.Integrity, workspaceBranch, workspaceDelta string, deltaOut *deltaPublication) (apiv1.ResultEnvelope, error) {
	// An AGENTIC stage cannot execute in a stage pod: the pod entrypoint runs a
	// declared command or script (dispatchexec), and invoking a goober through
	// its harness has no pod-side path at all — the local arm reaches it via
	// ActInvokeGoober, which the dispatch activity does not have.
	//
	// Refused HERE for the same reason as every other cut in this function:
	// without it, an agentic task sails past the guards below (they are all
	// inside the deterministic branch), a pod IS created, and the entrypoint
	// finds no command and returns "stage_declaration_invalid" — an error that
	// blames the workflow author's YAML for a substrate limitation, after
	// spending a pod to say it.
	//
	// The runner inventory can already pin an agentic stage here: a
	// harnesses:[copilot] runner class resolves and places, so this is
	// reachable today rather than hypothetical.
	if t.Type == apiv1.TaskDeterministic {
		if t.Run == nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("task %q is deterministic but declares no DeterministicRun", t.Name)
		}
		if len(t.Run.Command) == 0 && t.Run.Script == "" {
			return apiv1.ResultEnvelope{}, fmt.Errorf("task %q run declares no command or script; refusing to dispatch an empty command or script", t.Name)
		}
		// Repo and scratch are both provisioned in-pod now; anything else is a
		// mode this substrate has never had, and running it as if it were
		// scratch would silently give the stage the wrong workspace.
		if ws := t.Run.Workspace; ws != "" && ws != apiv1.WorkspaceScratch && !ws.IsRepoBacked() {
			return apiv1.ResultEnvelope{}, fmt.Errorf("task %q declares workspace %q, which mode-3 dispatch cannot provision in a pod", t.Name, ws)
		}
		if executor.StageRequiresInstanceConfig(t.Run.Command) {
			return apiv1.ResultEnvelope{}, fmt.Errorf("task %q runs %v, which reads the instance config directory; a stage pod has no config directory — place this stage on a self runner", t.Name, t.Run.Command)
		}
		// Decision 003 ruling 3: a ledger-touching, journal-reading, or
		// telemetry-rollup-reading command — or a built-in stage KIND with no
		// pod-side execution path (external-telemetry; ci-poll gained one in
		// #3881, decision 005 step C5, and is no longer refused here) — needs the
		// daemon's instance root, which a stage pod does not have. Unlike the
		// guards above (a misdeclared workspace, an empty command — bugs in
		// how the stage was built), this is refused as a normal, JOURNALED
		// stage outcome: dispatchInstanceRootRefusal routes it through
		// dispatchWithRetry so stage.finished carries the named code and
		// Task.ContinueOnError / gate branching apply exactly as they would
		// to a real executor failure, rather than hard-failing the whole run
		// over a placement the substrate cannot yet honour. No activity is
		// executed, so ActDispatchStage's dispatcher is never reached and no
		// pod is ever created.
		//
		// Read from env.Inputs, not t.Inputs: a stage may declare its kind
		// dynamically via inputsFrom (internal/workflow/v_3_0/timeoutcoherence.go
		// treats task.InputsFrom[boundedwait.InputKind] as legal-but-unprovable
		// statically), and runTask has already resolved that overlay into
		// env.Inputs immediately before routing here — t.Inputs alone would
		// miss a dynamically-resolved external-telemetry kind and let
		// a pod be created for it.
		if kind := resolvedKindInput(env); executor.StageRequiresInstanceRoot(t.Run.Command, kind) {
			return dispatchInstanceRootRefusal(ctx, in, t, rec, env.ContextPointers, deltaOut, instanceRootRefusalReason(t.Name, t.Run.Command, kind))
		}
	}
	return dispatchWithRetry(ctx, in, t, rec, env.ContextPointers, func(ctx workflow.Context, attempt int) (stageActivityResult, error) {
		var result stageActivityResult
		attemptEnv := env
		attemptEnv.Attempt = int32(attempt)
		// OwningWorkflowID is read here, inside the workflow, because this
		// walk's execution IS the attempt's driver: for a scheduled run that
		// is claimID+"-run", which no id composed from the pod's labels or
		// annotations can reconstruct (RunScheduled rewrote RunID to a hash).
		err := workflow.ExecuteActivity(ctx, ActDispatchStage, DispatchStageInput{
			Envelope:         attemptEnv,
			Placement:        placement,
			Run:              t.Run,
			Workspace:        t.Workspace,
			WorkspaceDelta:   workspaceDelta,
			WorkspaceBranch:  workspaceBranch,
			OwningWorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,
		}).Get(ctx, &result)
		result.Integrity = produced
		return result, err
	}, deltaOut)
}

// resolvedKindInput reads the stage's resolved executor.InputKind ("kind")
// input from env.Inputs — the fully-resolved input map runTask builds by
// overlaying any inputsFrom onto the static declaration (engine.go) — rather
// than the task's static Inputs alone, so a dynamically-resolved kind is
// visible to the instance-root refusal exactly as it will be to the shell
// executor at actual run time (executor.stringInput reads the same map).
func resolvedKindInput(env apiv1.InvocationEnvelope) string {
	kind, _ := env.Inputs[executor.InputKind].(string)
	return strings.TrimSpace(kind)
}

// instanceRootRefusalReason names the command (or stage kind) and why, for
// stage.finished's ErrorInfo.Message — the operator-facing half of the
// refusal, matching the "blame the substrate, not the workflow author" style
// of the guards above it in dispatchRemoteTask.
func instanceRootRefusalReason(taskName string, command []string, kind string) string {
	if kind != "" && kind != executor.KindShell {
		return fmt.Sprintf(
			"task %q declares inputs.kind=%q, a built-in stage kind with no pod-side execution path (internal/executor dispatches it in-process only); place this stage on a self runner",
			taskName, kind,
		)
	}
	return fmt.Sprintf(
		"task %q runs %v, which reads or writes the daemon's instance root (the file claim ledger, a merge lock, or an on-disk run journal); a stage pod has none — place this stage on a self runner",
		taskName, command,
	)
}

// dispatchInstanceRootRefusal journals a stage.finished FAILURE for a stage
// refused before dispatch — the ActDispatchStage activity is never executed,
// so the dispatcher is never consulted and no pod is ever created. Routed
// through dispatchWithRetry (rather than returned as a plain error, as the
// workspace/config/empty-command guards above do) so the outcome is a
// normal, journaled ResultEnvelope: it respects Task.ContinueOnError and
// gate branching exactly as a real executor failure would, instead of
// hard-failing the whole run over a placement choice the substrate cannot
// yet honour. The retry loop never actually fires: dispatchWithRetry only
// retries a non-nil ACTIVITY error, and this synthesizes a clean (err ==
// nil) ResultFailure on the first attempt — exactly the same shape a real
// executor's ordinary command failure returns (shell.go's Run doc comment).
// The delta out-param is threaded through unchanged so the refusal's synthetic
// attempt reports the same "published nothing" publication any non-writable
// stage does: a refused stage never ran, so it has no commits to hand on, and
// the walk's continuity record must see an empty digest rather than inherit the
// previous stage's by omission.
func dispatchInstanceRootRefusal(ctx workflow.Context, in RunInput, t apiv1.Task, rec *runJournal, pointers []apiv1.ContextPointer, deltaOut *deltaPublication, reason string) (apiv1.ResultEnvelope, error) {
	return dispatchWithRetry(ctx, in, t, rec, pointers, func(workflow.Context, int) (stageActivityResult, error) {
		return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "stage requires the daemon's instance root; refused before a pod was created",
			Error: &apiv1.ErrorInfo{
				Code:      executor.StageRequiresInstanceRootCode,
				Message:   reason,
				Retryable: false,
			},
		}}, nil
	}, deltaOut)
}

// StageDispatcher is the mode-3 substrate seam the dispatch activity executes
// through: place one stage attempt on the eligible runner set, supervise it,
// confirm surrender, dispose the pod. *dispatcher.Dispatcher satisfies it;
// tests fake it.
type StageDispatcher interface {
	Dispatch(ctx context.Context, attempt dispatcher.Attempt, eligible []dispatcher.RunnerSpec) (dispatcher.Report, error)
}

type needsHumanAssigneeProvider interface {
	NeedsHumanAssignee() string
}

func needsHumanAssignee(dispatcher StageDispatcher) string {
	provider, ok := dispatcher.(needsHumanAssigneeProvider)
	if !ok {
		return ""
	}
	return strings.TrimSpace(provider.NeedsHumanAssignee())
}

func stampDeterministicRun(attempt *dispatcher.Attempt, run *apiv1.DeterministicRun) {
	if run == nil {
		return
	}
	attempt.Command = run.Command
	attempt.Script = run.Script
	attempt.Env = run.Env
}

// stageWantsRunContext reports whether a mode-3 stage attempt should be
// stamped with the run's operational identity (GOOBERS_RUN_ID, GOOBERS_GAGGLE,
// etc.) — the mode-3 mirror of the local runner's same decision
// (internal/executor/shell.go). True either when the stage's command IS the
// goobers CLI, or when the stage opted in explicitly via
// run.InjectRunContext (#3484) because it wraps the CLI in another process
// (Command[0] names the wrapper, not "goobers") but still needs the same
// context its nested invocation does.
func stageWantsRunContext(run *apiv1.DeterministicRun) bool {
	return run != nil && (executor.StageInvokesGoobersCLI(run.Command) || run.InjectRunContext)
}

// DispatchStage executes one mode-3 stage attempt: it hands the attempt to
// the dispatcher (which creates, supervises, and disposes the pod) and then
// marshals the pod's surrendered blob back into the stageActivityResult the
// engine consumes — the same shape InvokeGoober / RunDeterministic produce
// for a local stage, so everything downstream (gate verdict routing, mutation
// journaling, repass state, scrubbing) is substrate-blind.
//
// No workspace is provisioned here, on purpose: the pod provisions its own
// (architecture §5 item 5); a local working copy would be dead weight the
// remote stage never sees.
func (a *Activities) DispatchStage(ctx context.Context, input DispatchStageInput) (stageActivityResult, error) {
	if a.Dispatcher == nil || a.Surrenders == nil {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("mode-3 stage dispatch for %q requires a dispatcher and a surrender store: %w", input.Envelope.TaskID, ErrNotConfigured))
	}
	if err := a.refuseLeakedEnvelope(input.Envelope); err != nil {
		return stageActivityResult{}, err
	}
	if input.Placement.Self {
		// The workflow routes self placements to the local arms; reaching this
		// activity with one means the routing was tampered with or mis-built.
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: stage %q placement resolved to self; self placements execute on the local path, never via DispatchStage (fail closed)", input.Envelope.TaskID))
	}
	// input.Run is set exactly for a deterministic dispatch (agentic stages
	// carry a nil Run and are unaffected below). Re-assert
	// dispatchRemoteTask's v1-scope guards here too — the same "trust the
	// workflow-side refusal happened, but re-check anyway" idiom the self-
	// placement check above already applies, and the activity boundary is
	// where a version-skewed or hand-built input would actually surface.
	// NOT re-asserted at this boundary, deliberately, unlike the guards below.
	// DispatchStageInput carries no task type, so the activity cannot tell an
	// agentic stage from a deterministic one whose Run it simply was not given
	// — the pod reads its command from the spec the dispatcher already stamped.
	// Inferring "Run == nil means agentic" would refuse legitimate inputs. The
	// workflow-side refusal is what prevents the pod; making this boundary
	// re-assert too would mean threading the task type through the activity
	// input, which is a wire-contract change worth its own decision.
	if input.Review && input.Run != nil {
		// A review is a goober invocation driven to a verdict; a declared
		// command has no verdict to write. The workflow never builds this
		// shape (dispatchRemoteGate sets Review with no Run), so reaching it
		// means a hand-built or version-skewed input — refused before a pod
		// exists rather than after a command ran under a review kit.
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: stage %q is marked review but carries a DeterministicRun; a reviewer gate invokes a goober and never runs a command (fail closed)", input.Envelope.TaskID))
	}
	if input.Run != nil {
		if ws := input.Run.Workspace; ws != "" && ws != apiv1.WorkspaceScratch && !ws.IsRepoBacked() {
			return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: stage %q declares workspace %q, which mode-3 dispatch cannot provision in a pod (fail closed)", input.Envelope.TaskID, ws))
		}
		if executor.StageRequiresInstanceConfig(input.Run.Command) {
			return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: stage %q reads the instance config directory, which a stage pod does not have (fail closed)", input.Envelope.TaskID))
		}
	}
	attempt := dispatcher.Attempt{
		RunID:          input.Envelope.RunID,
		Gaggle:         input.Envelope.Gaggle,
		Workflow:       input.Envelope.WorkflowID,
		Stage:          strings.TrimPrefix(input.Envelope.TaskID, input.Envelope.RunID+":"),
		Number:         int(input.Envelope.Attempt),
		LedgerTouching: input.Placement.LedgerTouching,
		CPU:            input.Placement.CPU,
		Memory:         input.Placement.Memory,
		Disk:           input.Placement.Disk,
		Restrictions:   input.Placement.Restrictions,
		// The driver the orphan sweep must describe. Taken from the input the
		// WORKFLOW stamped, never from activity.GetInfo: this activity is also
		// invoked directly in tests, and a stamp the workflow owns is the same
		// value on the first attempt and on every activity retry.
		OwningWorkflowID: input.OwningWorkflowID,
		// The runner-capability requirement, for the Windows identity stamp
		// (#3619) — distinct from the credential Capabilities set below.
		RunsOnCapabilities: input.Placement.Capabilities,
	}
	stampDeterministicRun(&attempt, input.Run)
	// Declared credential capabilities travel as NAMES; the pod resolves them
	// against the credential plane at stage start (DS9/DS10), so no secret
	// rides the dispatch payload or the pod spec.
	attempt.Capabilities = input.Envelope.Capabilities

	// An agentic stage executes by invoking a goober rather than running a
	// command, so the pod needs the whole invocation. It travels as a verified
	// claim check (internal/agentickit) — the dispatcher publishes the kit and
	// stamps only its digest, keeping the run's goal and ownership boundary off
	// a pod spec that anything with namespace read can list.
	if input.Run == nil {
		attempt.Agentic = true
		envelope := input.Envelope
		attempt.Envelope = &envelope
		// The completion contract rides the attempt into the kit writer, which
		// stamps it as agentickit.Kit.Mode inside the verified claim check.
		attempt.Review = input.Review
	}

	// A goobers-CLI stage needs the run's operational identity to do its job:
	// providerRepo() reads GOOBERS_REPO_* to learn which repository it was
	// routed to, and providers.BranchName composes the run branch from the
	// workflow and run. On a self runner the executor injects these for exactly
	// this stage class (shell.go, injectRunContext); mode 3 does the same here.
	//
	// Deliberately an EXPLICIT allowlist of names rather than marshalling
	// input.Envelope.RepoRef: this value reaches a pod spec, and an allowlist
	// cannot silently start shipping a field someone adds to RepoRef later.
	// (The config-side instance.RepoRef already carries Token and Auth; the
	// envelope's does not, and this is what keeps that distinction from
	// mattering.)
	// A repo workspace needs the same routed-repository facts a CLI stage does,
	// because the in-pod executor uses them to CHECK THE REPOSITORY OUT. They
	// are stamped for both and stripped from the stage's own environment for
	// everything except a CLI stage (DispatcherRunIdentityEnv), so a stage
	// running the project's build still cannot see the live run (#322).
	// An AGENTIC task has no DeterministicRun, so its workspace is declared on
	// the TASK — that field exists for exactly this case ("stages that cannot
	// express it through Run — i.e. agentic tasks"). Reading only Run.Workspace
	// left every agentic stage with no workspace mode stamped, so the pod's
	// checkout no-opped and the agent ran against an empty directory.
	//
	// MEASURED: the agent reported "README.md was not found at
	// /workspace/README.md" for a repository whose root contains one. It was
	// right, and the substrate was wrong — the worst shape of bug, since a
	// well-behaved agent reports the absence rather than failing loudly.
	//
	// Run.Workspace takes precedence when both are set, matching the field's
	// documented contract — apiv1.EffectiveWorkspace is the one place that
	// precedence is written, shared with the walk's continuity selector so
	// the pod's workspace and the delta it is handed can never be decided
	// from two different readings of one declaration.
	needsRepoContext := false
	workspace := apiv1.EffectiveWorkspace(input.Workspace, input.Run)
	if workspace != "" {
		attempt.Workspace = string(workspace)
	}
	// What earlier stages committed (#3763). Only meaningful for a workspace
	// the pod can commit into; handing it to a scratch or read-only stage
	// would be a no-op at best and a silent rewrite of a read-only stage's
	// pinned base at worst.
	if workspace.IsWritableRepo() {
		attempt.WorkspaceDelta = input.WorkspaceDelta
		// The rebound branch and the base sync are stamped on exactly the same
		// arm as the delta, and for one reason: they all describe what happens
		// to a WRITABLE run branch. A repo-readonly stage is detached at the
		// pinned base on every substrate — the local runner ignores a rebound
		// branch for it (createStageWorkspace's read-only arm passes Branch:
		// "") — so stamping either here would be the pod quietly reading
		// something the self runner does not.
		attempt.WorkspaceBranch = strings.TrimSpace(input.WorkspaceBranch)
		// SyncBase is already pinned inside input.Run (apiv1.DeterministicRun,
		// #813) and travels with it; it is lifted onto the attempt because the
		// pod spec is where the in-pod checkout reads its provisioning
		// instructions, and Run is not stamped there in full. An agentic stage
		// carries no DeterministicRun and therefore never syncs base, matching
		// the local runner (dispatchTask reads t.Run.SyncBase).
		attempt.SyncBase = input.Run != nil && input.Run.SyncBase
	}
	// A repo workspace has to be CLONED, and cloning a private repository needs
	// a credential. The pod used to take that from the stage's declared
	// business capabilities, so provisioning silently depended on the stage
	// happening to declare a repo-shaped one — open-pr declares
	// provider:pr:write alone and could not run in a pod at all (#3770).
	//
	// Naming it here rather than widening the stage's capabilities keeps the
	// two separate: the pod mints this for the checkout and never exports it to
	// the stage's environment, so a stage does not gain repository authority by
	// needing a working tree. The worker has always behaved this way — it
	// provisions worktrees with instance credentials, not the stage's.
	if workspace.IsRepoBacked() && !declaresRepoCapability(attempt.Capabilities) {
		attempt.CheckoutCapability = string(capability.RepoPush)
	}
	needsRepoContext = workspace.IsRepoBacked() || (input.Run != nil && workspace == "")
	// Gating the stamp on input.Run != nil would defeat needsRepoContext for the
	// exact case it was added to serve: an agentic task HAS no DeterministicRun,
	// so requiring one skipped the block and stamped no repository at all. The
	// CLI question still needs Run — it inspects Run.Command — but the repo
	// question does not, so the two are separated rather than nested.
	cliStage := stageWantsRunContext(input.Run)
	if cliStage || needsRepoContext {
		attempt.CLIStage = cliStage
		attempt.RunContext = map[string]string{}
		if repo := input.Envelope.RepoRef; repo.Provider != "" {
			attempt.RunContext[executor.RepoProviderEnvVar] = string(repo.Provider)
			attempt.RunContext[executor.RepoOwnerEnvVar] = repo.Owner
			attempt.RunContext[executor.RepoNameEnvVar] = repo.Name
			if repo.Project != "" {
				attempt.RunContext[executor.RepoProjectEnvVar] = repo.Project
			}
		}
		if ns := input.Envelope.BranchNamespace; ns != "" {
			attempt.RunContext[executor.BranchNamespaceEnvVar] = ns
		}
		if base := input.Envelope.BaseBranch; base != "" {
			attempt.RunContext[executor.BaseBranchEnvVar] = base
		}
		if trigger := input.Envelope.TriggerRef; trigger != "" {
			attempt.RunContext[executor.TriggerRefEnvVar] = trigger
		}
		if assignee := needsHumanAssignee(a.Dispatcher); assignee != "" {
			attempt.RunContext[executor.NeedsHumanAssigneeEnvVar] = assignee
		}
	}

	// Declared inputs travel to the pod so the stage reads GOOBERS_INPUT_<KEY>
	// exactly as it would locally, and so the in-pod executor can find the
	// declared resultFile it must lift into Outputs.
	if len(input.Envelope.Inputs) > 0 {
		attempt.Inputs = make(map[string]string, len(input.Envelope.Inputs))
		for key, value := range input.Envelope.Inputs {
			if rendered, ok := renderInputValue(value); ok {
				attempt.Inputs[key] = rendered
			}
		}
	}
	if input.Envelope.Limits.MaxDurationSeconds > 0 {
		attempt.Timeout = time.Duration(input.Envelope.Limits.MaxDurationSeconds) * time.Second
	}

	report, err := a.Dispatcher.Dispatch(ctx, attempt, input.Placement.Eligible)
	// Defense-in-depth for the settled-outcome invariant (#3588): once the
	// dispatcher has confirmed surrender, the pod's surrendered envelope is the
	// authoritative outcome — so ANY dispatcher error that arrives with
	// SurrenderConfirmed (a confirmed ErrStageFailed, a post-surrender dispose
	// fault, or any future post-surrender fault) must still be projected from
	// that envelope, never bypassed into an infra/policy retry that would
	// discard the result and re-dispatch an already-settled (possibly MUTATING)
	// stage. Only an error that left surrender UNconfirmed classifies as a
	// dispatch fault here. Keying off SurrenderConfirmed rather than the error
	// kind alone closes the whole class of post-surrender dispatcher errors.
	if err != nil && !errors.Is(err, dispatcher.ErrStageFailed) && !report.SurrenderConfirmed {
		return stageActivityResult{}, classifyDispatchError(err)
	}
	if err == nil && report.Local {
		// SelectRunner resolved self inside an eligible set the workflow routed
		// remotely — the pin and the dispatcher disagree. Fail closed rather
		// than silently executing nothing.
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: stage %q resolved to the self runner inside DispatchStage; the pinned placement and the dispatcher's selection disagree (fail closed)", input.Envelope.TaskID))
	}

	// ErrStageFailed arrives with surrender CONFIRMED (the dispatcher checks
	// the gate before classifying the phase), so both the success and the
	// stage-failed paths read the pod's own surrendered envelope: a failed
	// stage's ResultFailure is a business outcome the definition routes, with
	// exact parity to the local executor returning a failure envelope rather
	// than an error.
	surrendered, rerr := dispatcher.ReadSurrenderedResult(ctx, a.Surrenders, attempt.RunID, attempt.Stage, attempt.Number)
	if rerr != nil {
		// The gate confirmed surrender yet the result is unreadable: the
		// substrate lost or garbled the outputs after the stage did its work.
		// Infra-classed — the attempt retries on a fresh pod (D1), never
		// burning the policy budget on a data-plane fault.
		if errors.Is(rerr, dispatcher.ErrNoSurrender) {
			return stageActivityResult{}, classifySeamError(invoke.InfrastructureFailure(fmt.Errorf("engine: surrender confirmed for stage %q attempt %d but the surrendered result is absent: %w", input.Envelope.TaskID, attempt.Number, rerr)))
		}
		return stageActivityResult{}, classifySeamError(invoke.InfrastructureFailure(rerr))
	}
	if surrendered.Result.Status == "" {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: surrendered result for stage %q attempt %d carries no status; refusing to project a partial envelope (fail closed)", input.Envelope.TaskID, attempt.Number))
	}
	if input.Review {
		return a.reviewActivityResult(input, attempt.Number, surrendered, report)
	}
	if surrendered.Verdict != nil {
		// A task attempt has no reviewer; a verdict here means the pod ran
		// under a review kit it was not dispatched with. Refused rather than
		// dropped: a silently ignored verdict is exactly the shape a
		// substituted surrender document would take to look harmless.
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: surrendered result for task stage %q attempt %d carries a verdict; only a review attempt surrenders one (fail closed)", input.Envelope.TaskID, attempt.Number))
	}
	return a.scrubStageActivityResult(stageActivityResult{
		ResultEnvelope:     surrendered.Result,
		Mutations:          surrenderedMutationFacts(surrendered.Mutations),
		MutationIssues:     surrendered.MutationIssues,
		WorkspaceDelta:     surrendered.WorkspaceDelta,
		WorkspaceDeltaBase: surrendered.WorkspaceDeltaBase,
		WorkspaceDeltaTip:  surrendered.WorkspaceDeltaTip,
		// "Unchanged" is a positive claim about the branch — the pod checked
		// and found no commits beyond base — so it is REPORTED by the pod
		// (dispatch-exec, beside the digest it did not publish), never
		// inferred here from an absent digest: a stage image that predates
		// the field surrenders nothing about it and is journaled as nothing,
		// not as a verified fact.
		WorkspaceDeltaUnchanged: surrendered.WorkspaceDeltaUnchanged && surrendered.WorkspaceDelta == "",
		Placement:               placementProvenance(report),
	})
}

// reviewActivityResult projects a REVIEW attempt's surrender (decision 001
// rulings 7–8) into the result the workflow routes on. It is the pod-path
// twin of ReviewGoober's return, and it is deliberately stricter than the
// task projection above it: a task's surrendered envelope is a business
// outcome the definition branches on whatever its status, but a verdict IS
// the branch, so every way it could be absent, empty, or malformed fails
// the attempt closed here.
//
//   - The pod's own session failed (ResultFailure, no verdict): the failure
//     is returned as an ERROR — the self arm's ReviewGoober does the same
//     when Goober.Review errors — classed by the pod's own Retryable
//     marking, so a substrate fault (kit, credential, checkout, context)
//     retries on a fresh pod under the gate's evaluator retry bound and a
//     harness failure fails the run. The two kit-FETCH codes are classed
//     here regardless of that marking; see reviewKitFetchFailure (#3888).
//   - No verdict on a successful session: refused. Nothing to route on.
//   - An empty Decision, or a verdict the shared verdict schema rejects:
//     refused (#3838's shape — a substituted surrender blob must never
//     route control flow). The schema is the same one harness.Executor.
//     Review validates against in the pod; this is the engine's own read
//     of it, because the pod's validation is exactly what a substituted
//     document bypasses.
//   - A workspace delta beside a verdict: refused. A reviewer never
//     publishes (ReviewGoober), and a review pod that surrendered commits
//     has done something no reviewer does.
func (a *Activities) reviewActivityResult(input DispatchStageInput, number int, surrendered dispatcher.SurrenderedResult, report dispatcher.Report) (stageActivityResult, error) {
	stage := input.Envelope.TaskID
	if surrendered.Result.Status != apiv1.ResultSuccess {
		code, message := failureCause(surrendered.Result.Error)
		if code == "" {
			code = "agentic_review_failed"
		}
		err := fmt.Errorf("engine: reviewer gate %q attempt %d failed in its pod: %s: %s", stage, number, code, message)
		retryable := surrendered.Result.Error != nil && surrendered.Result.Error.Retryable
		if retryable || reviewKitFetchFailure(code) {
			return stageActivityResult{}, classifySeamError(invoke.InfrastructureFailure(err))
		}
		return stageActivityResult{}, classifySeamError(err)
	}
	if surrendered.Verdict == nil {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: reviewer gate %q attempt %d surrendered no verdict; refusing to route the run on nothing (fail closed)", stage, number))
	}
	if surrendered.WorkspaceDelta != "" || surrendered.WorkspaceDeltaUnchanged {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: reviewer gate %q attempt %d surrendered a workspace delta; a reviewer never publishes commits (fail closed)", stage, number))
	}
	if err := validateSurrenderedVerdict(*surrendered.Verdict); err != nil {
		return stageActivityResult{}, classifySeamError(fmt.Errorf("engine: reviewer gate %q attempt %d surrendered an invalid verdict: %w (fail closed)", stage, number, err))
	}
	verdict := *surrendered.Verdict
	return a.scrubStageActivityResult(stageActivityResult{
		ResultEnvelope: surrendered.Result,
		Verdict:        &verdict,
		Placement:      placementProvenance(report),
	})
}

// verdictValidator compiles the shared envelope schemas once per process —
// the same api/validate validator harness.Executor builds — so the engine's
// re-validation of a surrendered verdict reads the identical
// verdict.schema.json the pod's harness validated against.
//
// The singleton is read from every dispatch activity the worker runs
// concurrently; api/validate.Validator is safe for that by contract (#3887).
var verdictValidator = sync.OnceValues(validate.New)

// reviewKitFetchFailure reports whether a surrendered failure code names the
// pod's kit FETCH — the two refusals a stage pod makes before it holds a kit
// at all (dispatcher.CodeAgenticKitMissing, CodeAgenticKitUnavailable).
//
// Both are substrate faults on the way to the reviewer, never the reviewer's
// own outcome: a pod created without a kit digest is a dispatch/podspec fault,
// and a digest the blob plane would not serve is a blob-plane transport fault
// — the same class context materialization already carries. Neither says
// anything about the change under review, and a gate has no branch for "the
// reviewer never started", so both belong on the gate's evaluator retry bound
// with a fresh pod rather than failing the run.
//
// They are classed HERE rather than pod-side because the pod cannot class
// them: both returns precede the kit, so they precede kit.IsReview(), and the
// pod surrenders them with Retryable=false. input.Review is the engine's own
// knowledge, so the engine applies it (#3888). This runs only on the review
// arm — a task dispatch's surrendered failure stays the business outcome the
// definition routes on, untouched.
func reviewKitFetchFailure(code string) bool {
	switch code {
	case dispatcher.CodeAgenticKitMissing, dispatcher.CodeAgenticKitUnavailable:
		return true
	}
	return false
}

// validateSurrenderedVerdict is the engine's fail-closed read of a verdict
// that crossed the surrender plane: a non-empty Decision first (the one field
// the walk routes on, checked explicitly so the refusal names it even if the
// schema ever loosens), then the whole document against the verdict schema.
func validateSurrenderedVerdict(verdict apiv1.Verdict) error {
	if strings.TrimSpace(string(verdict.Decision)) == "" {
		return errors.New("verdict carries an empty decision")
	}
	v, err := verdictValidator()
	if err != nil {
		return fmt.Errorf("verdict schema unavailable: %w", err)
	}
	data, err := json.Marshal(verdict)
	if err != nil {
		return fmt.Errorf("marshal verdict for validation: %w", err)
	}
	if err := v.ValidateEnvelope("verdict", data); err != nil {
		return fmt.Errorf("verdict does not satisfy the verdict schema: %w", err)
	}
	return nil
}

// placementProvenance lifts the dispatcher's report into the result's
// provenance block (decision 003, "placement provenance in the dispatch
// result"). It is built from the REPORT, never from the pinned placement the
// activity was handed: the point of the block is to record what the substrate
// did, so echoing the request back would make it evidence of nothing.
//
// A Local report yields nil — DispatchStage already refuses that case as a
// pin/selection disagreement, and nil keeps "provenance present" equivalent to
// "a pod ran this".
func placementProvenance(report dispatcher.Report) *StagePlacement {
	if report.Local {
		return nil
	}
	return &StagePlacement{
		Runner:       report.Runner,
		Pod:          report.Pod,
		Image:        report.Image,
		QueuedAt:     report.QueuedAt,
		PodStartedAt: report.PodStartedAt,
	}
}

// declaresRepoCapability reports whether the stage already holds a repo-shaped
// capability, in which case the checkout has one and no separate mint is
// needed. Matches gitToken's rule exactly — "repo" only — so the two cannot
// disagree about what counts: a stage declaring github:issues:read must not be
// read as already having repository access.
func declaresRepoCapability(capabilities []string) bool {
	for _, c := range capabilities {
		if strings.Contains(strings.ToLower(c), "repo") {
			return true
		}
	}
	return false
}

// surrenderedMutationFacts converts the surrendered wire shape to the
// engine's own mutation facts, field for field.
func surrenderedMutationFacts(mutations []dispatcher.SurrenderedMutation) []mutationFact {
	if len(mutations) == 0 {
		return nil
	}
	facts := make([]mutationFact, 0, len(mutations))
	for _, m := range mutations {
		facts = append(facts, mutationFact{
			Provider: m.Provider, Kind: m.Kind, ID: m.ID, URL: m.URL, Operation: m.Operation,
		})
	}
	return facts
}

// classifyDispatchError commits a dispatcher error's attempt class into the
// typed application error the workflow's retry loop reads back (#622).
//
// Deterministic dispatch refusals — selection, image skew, restriction or
// label mismatches — are policy-classed: redispatching the identical attempt
// reproduces them byte for byte, so an infra retry would only burn budget.
// EVERYTHING ELSE on this seam is infrastructure by design: "a pod or node
// killed mid-stage classifies as an infra attempt (non-normative, retried on
// the infra budget with a fresh pod)" (goobernetes-architecture.md §5
// item 8) — capacity timeouts, pod create/supervise/dispose failures, and an
// unconfirmed surrender are all substrate faults, never the stage's own
// report. The business outcome only ever arrives via the surrendered
// envelope, so the local seam's "everything unmarked is policy" convention
// does not transplant here.
func classifyDispatchError(err error) error {
	var selection *dispatcher.SelectionError
	var skew *dispatcher.SkewError
	var restriction *dispatcher.RestrictionMismatchError
	var label *dispatcher.LabelOverrideError
	if errors.As(err, &selection) || errors.As(err, &skew) || errors.As(err, &restriction) || errors.As(err, &label) {
		return classifySeamError(err)
	}
	return classifySeamError(invoke.InfrastructureFailure(err))
}

// renderInputValue flattens a declared input to its stage-environment string.
// Scalars render; structured values do NOT — a map or slice has no faithful
// single-variable spelling, and inventing one (JSON, comma-joined) would make
// the pod disagree with the local executor in a way no error would report.
// Skipping is the honest choice: the variable is absent rather than wrong.
func renderInputValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		// JSON numbers decode as float64; render integers without a ".0" tail
		// so a declared 30 does not reach the stage as "30".
		if typed == math.Trunc(typed) && math.Abs(typed) < 1e15 {
			return strconv.FormatInt(int64(typed), 10), true
		}
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	default:
		return "", false
	}
}
