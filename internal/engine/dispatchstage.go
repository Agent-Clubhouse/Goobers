package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
// RunInput at run start. The zero value never occurs in a pinned list; an
// absent entry (or an empty Placements list — every zero-declaration and
// local-mode instance) leaves the stage on the legacy self path, byte for
// byte.
type PinnedPlacement struct {
	// Stage is the task name the placement binds to.
	Stage string `json:"stage"`
	// Self marks a placement that resolved to the daemon/worker host: the
	// stage executes through the existing InvokeGoober / RunDeterministic
	// arms, exactly as before this field existed. The dispatcher models the
	// same outcome as Local=true/no-pod, so self stays a first-class
	// placement rather than a special case.
	Self bool `json:"self,omitempty"`
	// Queue is the per-(gaggle × runner-type) task queue the dispatch
	// activity is routed onto (dispatcher.QueueName of the runner
	// SelectRunner picks — D9). Empty inherits the workflow's queue.
	Queue string `json:"queue,omitempty"`
	// Eligible is the solver's eligible runner set for this stage, in
	// inventory order — dispatch consumes eligibility, it never re-derives it
	// (goobernetes-dispatcher.md §2).
	Eligible []dispatcher.RunnerSpec `json:"eligible,omitempty"`
	// LedgerTouching, CPU, Memory, Disk, and Restrictions are the
	// dispatcher.Attempt requirement facts, carried from the run-start solve
	// (runnersolve.StageRequirement) because the workflow cannot recompute
	// them mid-run.
	LedgerTouching bool     `json:"ledgerTouching,omitempty"`
	CPU            string   `json:"cpu,omitempty"`
	Memory         string   `json:"memory,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	Restrictions   []string `json:"restrictions,omitempty"`
}

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

// dispatchStageInput is ActDispatchStage's activity input: the stage's fully
// built invocation envelope, the pinned placement facts, and — for a
// deterministic task — the pinned DeterministicRun content the pod actually
// executes (#3699). Pure data — the workflow resolves nothing at dispatch
// time; Run is read from the pinned Definition (apiv1.Task.Run), the same
// WF-016 snapshot Placement itself is pinned from, so carrying it here adds
// no new nondeterminism. Deliberately NOT added to apiv1.InvocationEnvelope
// (the DSL/CRD-shared wire contract): this type is Temporal activity input
// only, never DSL-visible.
type dispatchStageInput struct {
	Envelope  apiv1.InvocationEnvelope `json:"envelope"`
	Placement PinnedPlacement          `json:"placement"`
	Run       *apiv1.DeterministicRun  `json:"run,omitempty"`
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
func dispatchRemoteTask(ctx workflow.Context, t apiv1.Task, rec *runJournal, env apiv1.InvocationEnvelope, placement PinnedPlacement, produced apiv1.Integrity) (apiv1.ResultEnvelope, error) {
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
	if t.Type == apiv1.TaskAgentic {
		return apiv1.ResultEnvelope{}, fmt.Errorf("task %q is agentic; mode-3 dispatch does not yet run a goober harness in a stage pod — place agentic stages on a self runner", t.Name)
	}
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
	}
	return dispatchWithRetry(ctx, t, rec, env.ContextPointers, func(ctx workflow.Context, attempt int) (stageActivityResult, error) {
		var result stageActivityResult
		attemptEnv := env
		attemptEnv.Attempt = int32(attempt)
		err := workflow.ExecuteActivity(ctx, ActDispatchStage, dispatchStageInput{Envelope: attemptEnv, Placement: placement, Run: t.Run}).Get(ctx, &result)
		result.Integrity = produced
		return result, err
	})
}

// StageDispatcher is the mode-3 substrate seam the dispatch activity executes
// through: place one stage attempt on the eligible runner set, supervise it,
// confirm surrender, dispose the pod. *dispatcher.Dispatcher satisfies it;
// tests fake it.
type StageDispatcher interface {
	Dispatch(ctx context.Context, attempt dispatcher.Attempt, eligible []dispatcher.RunnerSpec) (dispatcher.Report, error)
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
func (a *Activities) DispatchStage(ctx context.Context, input dispatchStageInput) (stageActivityResult, error) {
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
	// dispatchStageInput carries no task type, so the activity cannot tell an
	// agentic stage from a deterministic one whose Run it simply was not given
	// — the pod reads its command from the spec the dispatcher already stamped.
	// Inferring "Run == nil means agentic" would refuse legitimate inputs. The
	// workflow-side refusal is what prevents the pod; making this boundary
	// re-assert too would mean threading the task type through the activity
	// input, which is a wire-contract change worth its own decision.
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
	}
	if input.Run != nil {
		attempt.Command = input.Run.Command
		attempt.Script = input.Run.Script
		attempt.Env = input.Run.Env
	}
	// Declared credential capabilities travel as NAMES; the pod resolves them
	// against the credential plane at stage start (DS9/DS10), so no secret
	// rides the dispatch payload or the pod spec.
	attempt.Capabilities = input.Envelope.Capabilities

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
	needsRepoContext := false
	if input.Run != nil {
		attempt.Workspace = string(input.Run.Workspace)
		needsRepoContext = input.Run.Workspace.IsRepoBacked() || input.Run.Workspace == ""
	}
	if input.Run != nil && (executor.StageInvokesGoobersCLI(input.Run.Command) || needsRepoContext) {
		attempt.CLIStage = executor.StageInvokesGoobersCLI(input.Run.Command)
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
	return a.scrubStageActivityResult(stageActivityResult{
		ResultEnvelope: surrendered.Result,
		Mutations:      surrenderedMutationFacts(surrendered.Mutations),
		MutationIssues: surrendered.MutationIssues,
	})
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
