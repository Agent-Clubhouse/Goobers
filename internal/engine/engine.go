package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

// Run statuses.
const (
	StatusCompleted = "completed"
	StatusBlocked   = "blocked"
	StatusEscalated = "escalated"
	// StatusFailed is a run ended by an unresolved stage failure — the
	// engine's analogue of the local runner's PhaseFailed (#110/#710). The
	// workflow completes cleanly with this status (the failure is a business
	// outcome, recorded with its cause) rather than failing the workflow,
	// which is reserved for dispatch/walk errors.
	StatusFailed = "failed"
)

const (
	// maxSteps bounds the number of state transitions in a single run, a
	// last-resort guard against a definition that loops (WF-015 within a
	// run). Shared with the local runner so the ceilings cannot drift again
	// (#624: they had diverged 1000 vs 10000).
	maxSteps = runner.DefaultMaxSteps
	// activityTimeout is the start-to-close timeout applied to activities whose
	// stage declares no duration limit. A constant (not wall-clock) keeps the
	// workflow deterministic.
	activityTimeout = time.Hour
	// stageTimeoutGrace pads a declared limits.MaxDurationSeconds before it
	// becomes the Temporal StartToCloseTimeout. The worker-side runtime
	// self-enforces the declared limit and surfaces the overrun as
	// invoke.Timeout — a policy-classed stage failure, exactly like the local
	// runner's dispatch (#724/#622) — so the grace guarantees that
	// self-enforcement wins the race against Temporal's own timeout. A
	// temporal.TimeoutError is thereby reserved for genuine worker loss, never
	// a stage merely overrunning its declared budget.
	// stageScheduleToStart bounds how long a stage may sit on a task queue no
	// worker is serving. The SDK default is unlimited, so an unroutable stage
	// would hang the run silently rather than fail with the queue named.
	stageScheduleToStart = 15 * time.Minute
	stageTimeoutGrace    = 5 * time.Minute
)

// RunInput is the pinned input to a workflow run. Spec is a snapshot of the
// definition at the version the run started on, so the run is unaffected by later
// re-registrations (WF-016).
type RunInput struct {
	RunID                  string             `json:"runId"`
	Gaggle                 string             `json:"gaggle"`
	WorkflowName           string             `json:"workflowName"`
	Version                int                `json:"version"`
	DSLVersion             string             `json:"dslVersion,omitempty"`
	WorkflowDigest         string             `json:"workflowDigest"`
	PreviewFeaturesEnabled *bool              `json:"previewFeaturesEnabled,omitempty"`
	Spec                   apiv1.WorkflowSpec `json:"spec"`
	RepoRef                apiv1.RepoRef      `json:"repoRef"`
	Item                   *apiv1.BacklogItem `json:"item,omitempty"`
	// TriggerRef identifies the event or item that caused the run — the same
	// bounded scheduler metadata the local runner threads into every
	// envelope's triggerRef field (#621 envelope parity).
	TriggerRef string `json:"triggerRef,omitempty"`
	// TriggerKind is how the run was started (journal.TriggerKind vocabulary:
	// manual/schedule/signal/item). Pinned for the run.yaml identity the
	// history→journal projection writes (#629) and for the local runner's
	// deferred branch-provenance rule. Empty behaves like manual.
	TriggerKind string `json:"triggerKind,omitempty"`
	// BranchNamespace is the gaggle's configured run-branch namespace root
	// (GaggleSpec.BranchNamespace), stamped into every envelope exactly as the
	// local runner does. Empty means the default namespace.
	BranchNamespace string `json:"branchNamespace,omitempty"`
	// GateGooberCapabilities maps a reviewer goober name to its granted
	// capabilities, pinned at start like the rest of the run's policy. An
	// agentic gate's envelope carries the reviewer's own grants — AgenticGate
	// declares no stage-level capabilities — mirroring the local runner's
	// Config.GateGooberCapabilities (#294). Automated/human gates stay
	// uncredentialed.
	GateGooberCapabilities map[string][]string `json:"gateGooberCapabilities,omitempty"`
	// MaxRepasses is the legacy run-wide repass field retained for replay of
	// inputs created before RunControls.
	MaxRepasses int `json:"maxRepasses,omitempty"`
	// RunControls pins inherited workflow policy. MaxRepasses above remains a
	// compatibility field for persisted inputs created before this block.
	RunControls apiv1.RunControls `json:"runControls,omitempty"`
	// LiveJournal pins whether this run's journal is authored live through
	// the daemon's journal plane (DS4): activities emit journal events as
	// they happen, and the deterministic accumulation behind JournalQuery
	// becomes the repair/cross-check source rather than the authority (DS5).
	// Pinned input rather than worker config so replay is deterministic and
	// runs started before the live journal service existed keep projecting
	// exactly as before. False (the zero value) preserves today's behavior
	// byte for byte.
	LiveJournal bool `json:"liveJournal,omitempty"`
}

func (in RunInput) previewFeaturesEnabled() bool {
	if in.PreviewFeaturesEnabled == nil {
		// Inputs persisted before this policy existed were already admitted under
		// preview-permissive compilation and must retain that behavior on replay.
		return true
	}
	return *in.PreviewFeaturesEnabled
}

// RunResult is the terminal outcome of a workflow run.
type RunResult struct {
	Status     string                          `json:"status"`
	FinalState string                          `json:"finalState,omitempty"`
	Outputs    map[string]apiv1.ResultEnvelope `json:"outputs,omitempty"`
	Steps      int                             `json:"steps"`
	// FailureCode/FailureMessage carry a StatusFailed run's stage-reported
	// cause — the local runner's Result.FailureCode/FailureMessage parity
	// (#710). Empty for every other status.
	FailureCode    string `json:"failureCode,omitempty"`
	FailureMessage string `json:"failureMessage,omitempty"`
}

// HumanGateSignal is the Temporal signal name a human gate waits on for its
// decision (the decision string is used as the gate outcome).
func HumanGateSignal(gateName string) string {
	return "gate:" + gateName
}

const temporalHumanGateUnsupported = "engine: human gates require occurrence-bound Temporal signals and are not supported yet"

// Run is the engine's Temporal workflow function. It walks the pinned definition
// as a state machine: tasks invoke activities to produce result envelopes; gates
// evaluate and branch. It performs no wall-clock reads or randomness — all side
// effects are in activities.
//
// Alongside the walk it accumulates the run's journal projection (#629) as
// deterministic workflow state, exposed through JournalQuery: every stage
// attempt, gate verdict, and terminal outcome is recorded exactly where the
// local runner journals its own, so the projected runs/<id>/ record is
// indistinguishable from a local run's on the conformance surface.
func Run(ctx workflow.Context, in RunInput) (RunResult, error) {
	return run(ctx, in, nil)
}

// ClaimScheduled converts a Schedule action into an exactly-once run start.
// Temporal forbids WorkflowIDReusePolicy on Schedule actions, so the action
// workflow claims the fire with a child ID whose reuse policy rejects duplicates.
func ClaimScheduled(ctx workflow.Context, in RunInput) (RunResult, error) {
	claimID := workflow.GetInfo(ctx).WorkflowExecution.ID
	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            scheduledRunWorkflowID(claimID),
		TaskQueue:             workflow.GetInfo(ctx).TaskQueueName,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})
	in.RunID = claimID
	var result RunResult
	err := workflow.ExecuteChildWorkflow(childCtx, RunScheduled, in).Get(childCtx, &result)
	if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
		return RunResult{}, nil
	}
	return result, err
}

// scheduledRunWorkflowIDSuffix distinguishes a scheduled run's child workflow
// id from its Schedule action claim id. The claim liveness probe
// (liveness.go) inverts this mapping, so both sides share the constant.
const scheduledRunWorkflowIDSuffix = "-run"

func scheduledRunWorkflowID(claimID string) string {
	return claimID + scheduledRunWorkflowIDSuffix
}

// RunScheduled binds a timestamped schedule claim to the run and records the
// nominal fire in the scheduler-journal projection.
func RunScheduled(ctx workflow.Context, in RunInput) (RunResult, error) {
	workflowID := workflow.GetInfo(ctx).WorkflowExecution.ID
	claimID := in.RunID
	if claimID == "" {
		claimID = workflowID
	}
	fireTime, err := scheduledFireTime(in.TriggerRef, claimID)
	if err != nil {
		return RunResult{}, err
	}
	// Temporal's claim ID carries an RFC3339 timestamp (and therefore colons).
	// Hash it into the same portable trace/run ID shape every other starter uses.
	in.RunID = RunID(claimID)
	in.TriggerKind = string(journal.TriggerSchedule)
	return run(ctx, in, &fireTime)
}

func scheduledFireTime(scheduleID, workflowID string) (time.Time, error) {
	// Temporal Schedules append "-"+nominal.UTC().Format(time.RFC3339) to the
	// configured action ID and reject reuse of the resulting workflow ID.
	prefix := scheduleID + "-"
	if scheduleID == "" || !strings.HasPrefix(workflowID, prefix) {
		return time.Time{}, fmt.Errorf("engine: scheduled workflow ID %q does not encode schedule %q", workflowID, scheduleID)
	}
	fireTime, err := time.Parse(time.RFC3339, strings.TrimPrefix(workflowID, prefix))
	if err != nil {
		return time.Time{}, fmt.Errorf("engine: scheduled workflow ID %q has invalid fire time: %w", workflowID, err)
	}
	return fireTime, nil
}

func run(ctx workflow.Context, in RunInput, scheduledAt *time.Time) (RunResult, error) {
	in.Item = normalizeItemIntegrity(in.Item)
	m, err := wf.Compile(
		wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec},
		wf.WithPreviewFeatures(in.previewFeaturesEnabled()),
	)
	if err != nil {
		return RunResult{}, err
	}
	for _, g := range in.Spec.Gates {
		if g.Evaluator == apiv1.EvaluatorHuman {
			return RunResult{}, fmt.Errorf("%s: gate %q", temporalHumanGateUnsupported, g.Name)
		}
	}
	rec, err := newRunJournal(ctx, in, m)
	if err != nil {
		return RunResult{}, err
	}
	rec.runStarted(ctx)
	if scheduledAt != nil {
		rec.triggerFiredAt(*scheduledAt, in)
	}
	rec.recordRunBranchUpfront(ctx, in)

	var res RunResult
	// The opening emission creates the run journal at the daemon (first-emit
	// creation, §8) — from here the run is live: stall detection, SSE, and
	// the portal see it mid-flight. Failure to open the live journal fails
	// the run, the same stance the local runner takes when journal.Create
	// fails: a run whose product output cannot be authored does not execute.
	err = rec.emitPending(ctx)
	if err == nil {
		res, err = walk(ctx, in, m, rec)
	}
	if err != nil {
		// A walk-level error is the engine's failTerminal (#305): record the
		// cause and the failed terminal in the projection, then fail the
		// workflow. A canceled run is the one exception — it has no terminal.
		if !temporal.IsCanceledError(err) && ctx.Err() == nil {
			rec.runFailedCause(ctx, "", "", err.Error())
			rec.runFinished(ctx, journal.PhaseFailed)
			rec.emitTerminal(ctx)
		}
		return RunResult{}, err
	}
	if res.Status == StatusFailed {
		// Mirror finishStageFailure (#710): the stage-attributed run_failed
		// cause precedes the terminal marker.
		rec.runFailedCause(ctx, res.FinalState, res.FailureCode, res.FailureMessage)
	}
	phase, err := phaseForStatus(res.Status)
	if err != nil {
		return RunResult{}, err
	}
	rec.runFinished(ctx, phase)
	rec.emitTerminal(ctx)
	return res, nil
}

func walk(ctx workflow.Context, in RunInput, m *wf.Machine, rec *runJournal) (RunResult, error) {
	logger := workflow.GetLogger(ctx)
	upstream := map[string]apiv1.ResultEnvelope{}
	// pointers accumulates every completed stage's artifacts as read-only
	// ContextPointers — the only channel through which a stage consumes prior
	// work (§2.4) — exactly as the local runner's walk does.
	var pointers []apiv1.ContextPointer
	// Gate attempts recover interrupted evaluators; repass attempts enforce the
	// run budget cumulatively by completed target stage.
	gateAttempts := map[string]int{}
	repassAttempts := map[string]int{}
	var lastStage string
	var lastResult apiv1.ResultEnvelope
	var workspaceBranch string
	state := in.Spec.Start
	steps := 0

	for {
		switch state {
		case wf.TerminalComplete:
			return RunResult{Status: StatusCompleted, Outputs: upstream, Steps: steps}, nil
		case wf.TargetAbort:
			return RunResult{Status: StatusBlocked, Outputs: upstream, Steps: steps}, nil
		case wf.TargetEscalate:
			return RunResult{Status: StatusEscalated, Outputs: upstream, Steps: steps}, nil
		}

		steps++
		if steps > maxSteps {
			return RunResult{}, fmt.Errorf("workflow %q exceeded max steps (%d): possible loop", in.WorkflowName, maxSteps)
		}

		if t, ok := m.Task(state); ok {
			res, terr := runTask(ctx, in, m, t, pointers, lastResult, workspaceBranch, rec)
			if terr != nil {
				return RunResult{}, terr
			}
			if res.Status == apiv1.ResultFailure && t.ContinueOnError {
				// Outputs from a tolerated failure are discarded so downstream
				// stages cannot consume partial results (Task.ContinueOnError,
				// same discard the local runner applies).
				res.Outputs = nil
			}
			upstream[t.Name] = res
			pointers = append(pointers, contextPointersFor(t.Name, res.Artifacts)...)
			lastStage, lastResult = t.Name, res
			if res.Status != apiv1.ResultFailure || !t.ContinueOnError {
				branch, err := selectedWorkspaceBranch(t, res, in.BranchNamespace)
				if err != nil {
					return RunResult{}, fmt.Errorf("stage %q selected workspace branch: %w", t.Name, err)
				}
				if branch != "" {
					workspaceBranch = branch
				}
			}
			logger.Info("task complete", "task", t.Name, "status", res.Status)
			next, out, terminal := taskOutcome(ctx, m, t, res, upstream, steps, rec)
			if terminal {
				return out, nil
			}
			state = next
			continue
		}

		if g, ok := m.Gate(state); ok {
			// A repo-using run reaching a gate before any attempt proved the
			// branch exists still owes provenance (walk's gate arm parity).
			rec.recordRunBranch(ctx)
			// The machine remains at this gate until its evaluator records a
			// verdict — the same durable wait marker the local runner persists
			// before dispatch.
			rec.gatePaused(ctx, g.Name)
			outcome, verdict, gerr := evaluateGate(ctx, m, g, in, lastResult, pointers, workspaceBranch, gateAttempts, rec)
			if gerr != nil {
				return RunResult{}, gerr
			}
			_, reentry := upstream[wfTarget(g, outcome)]
			gr, rerr := resolveGateOutcome(g, outcome, reentry, gateAttempts, repassAttempts, maxRepassesFor(in))
			if rerr != nil {
				return RunResult{}, rerr
			}
			verdictArtifact, jerr := rec.gateEvaluated(ctx, gr, verdict)
			if jerr != nil {
				return RunResult{}, jerr
			}
			// Gate boundary emission: the verdict (and its artifact) become
			// live before the walk moves on. Exhausting the emit budget here
			// fails the run — a gate decision that cannot be journaled must
			// not silently route the run (§8's fail-closed stance); the
			// terminal is then backfilled by the repair projection.
			if err := rec.emitPending(ctx); err != nil {
				return RunResult{}, err
			}
			logger.Info("gate evaluated", "gate", g.Name, "outcome", gr.Outcome, "next", gr.Target, "attempt", gr.Attempt, "escalated", gr.Escalated)
			next, out, terminal := gateTransition(m, gr, lastStage, lastResult, upstream, steps)
			if terminal {
				return out, nil
			}
			if verdictArtifact != nil {
				// #412: the next dispatch — most commonly a repass back to the
				// stage that produced the subject this gate just evaluated —
				// must actually receive the reviewer's verdict as context, not
				// infer "something needs to change" from git. The local
				// runner's walk appends the same "<gate>.verdict" pointer on
				// both its retry route and its advance path.
				pointers = append(pointers, apiv1.ContextPointer{
					Name: g.Name + ".verdict", Integrity: verdictArtifact.Integrity, Artifact: verdictArtifact,
				})
			}
			state = next
			continue
		}

		return RunResult{}, fmt.Errorf("unknown state %q", state)
	}
}

// taskOutcome applies the local runner's #110 stage-status ruling to a
// finished task's result, mirroring internal/runner.(*Runner).taskOutcome:
// success advances to Next; failure advances when ContinueOnError is set or
// Next is a gate (which branches on the honest failed status), otherwise the
// run fails; blocked halts the walk at the escalated terminal (#544 — a
// schema-valid producer value, never punished as a failure); no-work
// short-circuits straight to completed regardless of Next (#233 — a stage
// that correctly found nothing must not hand a downstream agentic stage an
// empty subject). A successful task's Next may itself be a reserved terminal
// target (@abort/@escalate, #123).
func taskOutcome(ctx workflow.Context, m *wf.Machine, t apiv1.Task, result apiv1.ResultEnvelope, upstream map[string]apiv1.ResultEnvelope, steps int, rec *runJournal) (next string, out RunResult, terminal bool) {
	switch result.Status {
	case apiv1.ResultBlocked:
		rec.blocked(ctx, t.Name, result)
		return "", RunResult{Status: StatusEscalated, FinalState: t.Name, Outputs: upstream, Steps: steps}, true
	case apiv1.ResultFailure:
		if t.ContinueOnError {
			rec.toleratedFailure(ctx, t.Name)
			break
		}
		if _, isGate := m.Gate(t.Next); t.Next != "" && isGate {
			return t.Next, RunResult{}, false
		}
		code, message := failureCause(result.Error)
		return "", RunResult{Status: StatusFailed, FinalState: t.Name, FailureCode: code, FailureMessage: message, Outputs: upstream, Steps: steps}, true
	case apiv1.ResultNoWork:
		return "", RunResult{Status: StatusCompleted, FinalState: t.Name, Outputs: upstream, Steps: steps}, true
	}
	switch t.Next {
	case wf.TerminalComplete:
		return "", RunResult{Status: StatusCompleted, FinalState: t.Name, Outputs: upstream, Steps: steps}, true
	case wf.TargetAbort:
		return "", RunResult{Status: StatusBlocked, FinalState: t.Name, Outputs: upstream, Steps: steps}, true
	case wf.TargetEscalate:
		return "", RunResult{Status: StatusEscalated, FinalState: t.Name, Outputs: upstream, Steps: steps}, true
	}
	return t.Next, RunResult{}, false
}

// gateTransition maps a resolved gate branch to the walk's next move,
// mirroring internal/runner.(*Runner).gateTransition: @abort ends blocked,
// @escalate ends escalated, and a terminal-complete branch applies the #849
// ruling — a non-pass gate must not hide an unresolved stage failure, while
// a passing gate has affirmatively cleared that same result.
func gateTransition(m *wf.Machine, gr gateResult, lastStage string, lastResult apiv1.ResultEnvelope, upstream map[string]apiv1.ResultEnvelope, steps int) (next string, out RunResult, terminal bool) {
	switch gr.Target {
	case wf.TargetAbort:
		return "", RunResult{Status: StatusBlocked, FinalState: gr.Gate, Outputs: upstream, Steps: steps}, true
	case wf.TargetEscalate:
		return "", RunResult{Status: StatusEscalated, FinalState: gr.Gate, Outputs: upstream, Steps: steps}, true
	case wf.TerminalComplete:
		subject, _ := m.Task(lastStage)
		if lastResult.Status == apiv1.ResultFailure && !subject.ContinueOnError && gr.Outcome != gate.OutcomePass {
			code, message := failureCause(lastResult.Error)
			return "", RunResult{Status: StatusFailed, FinalState: lastStage, FailureCode: code, FailureMessage: message, Outputs: upstream, Steps: steps}, true
		}
		return "", RunResult{Status: StatusCompleted, FinalState: gr.Gate, Outputs: upstream, Steps: steps}, true
	}
	return gr.Target, RunResult{}, false
}

// failureCause mirrors the local runner's failureCauseFrom (#710): a failed
// stage's own code/message, with a stable fallback when the stage reported
// no detail.
func failureCause(e *apiv1.ErrorInfo) (code, message string) {
	if e == nil || e.Message == "" {
		return "", "stage reported failure with no error detail"
	}
	return e.Code, e.Message
}

func runTask(ctx workflow.Context, in RunInput, machine *wf.Machine, t apiv1.Task, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, workspaceBranch string, rec *runJournal) (apiv1.ResultEnvelope, error) {
	upstream = apiv1.SelectContextPointers(upstream, t.ContextFrom)
	inputs, err := wf.TaskInvocationInputs(machine, t)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("project task %q inputs: %w", t.Name, err)
	}
	limits, err := wf.TaskLimits(machine, t)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("project task %q limits: %w", t.Name, err)
	}
	env := buildInvocation(in, t.Name, t.Goal, inputs, t.Capabilities, limits, upstream, t.Goober)
	env.MinimumIntegrity = t.MinimumIntegrity
	env.Attempt = 1
	env.OwnershipBoundary = "task:" + t.Name
	env.PolicyActions = append([]string(nil), t.PolicyActions...)
	env.NestedAgentPolicy = t.NestedAgentPolicy
	if t.NestedAgentPolicy != nil {
		parent := apiv1.StagePlatformAuthority(env, "result")
		env.ParentPlatformPolicy = &parent
	}
	// Both admission checks run before dispatch, matching the local runner.
	// The engine resolves inputsFrom only against the immediately preceding
	// task, so every such value is graded by that task's produced provenance;
	// Outputs are bare scalars and carry none of their own (TBH-4).
	integrityErr := apiv1.ValidateInputIntegrity(env.Item, env.ContextPointers, env.MinimumIntegrity)
	if integrityErr == nil {
		integrityErr = apiv1.ValidateResolvedInputIntegrity(
			engineInputGrades(t, upstreamResult), env.MinimumIntegrity)
	}
	if err := integrityErr; err != nil {
		admission := &apiv1.IntegrityAdmissionError{}
		if !errors.As(err, &admission) {
			return apiv1.ResultEnvelope{}, err
		}
		rec.integrityRefused(ctx, t.Name, admission)
		return apiv1.ResultEnvelope{}, fmt.Errorf("engine: refuse stage %q: %w", t.Name, admission)
	}
	// InputsFrom overlays the immediately preceding task's declared outputs on
	// top of the static Inputs (#132). A declared outputKey missing upstream
	// fails the stage closed — the declaration is a contract, not a hint —
	// matching the local runner's dispatchTask. Keys are walked sorted so the
	// first-missing error is deterministic under replay.
	for _, inputKey := range sortedKeys(t.InputsFrom) {
		outputKey := t.InputsFrom[inputKey]
		v, ok := upstreamResult.Outputs[outputKey]
		if !ok {
			return apiv1.ResultEnvelope{}, fmt.Errorf("task %q: inputsFrom %q: upstream output %q not found", t.Name, inputKey, outputKey)
		}
		env.Inputs[inputKey] = v
	}
	ctx = stageActivityContextOn(ctx, env.Limits, t.RequiredCapabilities)
	produced := engineProducedIntegrity(t, env, upstreamResult)
	if t.Type == apiv1.TaskAgentic {
		// Graded inside the closure: dispatchWithRetry journals stage.finished
		// from what the closure returns, so setting it afterwards would leave
		// the journal ungraded and diverge from the local runner.
		return dispatchWithRetry(ctx, t, rec, env.ContextPointers, func(ctx workflow.Context, attempt int) (stageActivityResult, error) {
			var result stageActivityResult
			attemptEnv := env
			attemptEnv.Attempt = int32(attempt)
			err := workflow.ExecuteActivity(ctx, ActInvokeGoober, attemptEnv, workspaceBranch).Get(ctx, &result)
			result.Integrity = produced
			return result, err
		})
	}
	// Fail closed on an absent or zero-value run (#626/#156): a
	// DeterministicRun{} previously masked nil and dispatched an empty run. The
	// registry rejects these shapes at registration; this guard covers a
	// RunInput constructed without it.
	if t.Run == nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("task %q is deterministic but declares no DeterministicRun", t.Name)
	}
	if len(t.Run.Command) == 0 && t.Run.Script == "" {
		return apiv1.ResultEnvelope{}, fmt.Errorf("task %q run declares no command or script; refusing to dispatch an empty command or script", t.Name)
	}
	run := *t.Run
	return dispatchWithRetry(ctx, t, rec, env.ContextPointers, func(ctx workflow.Context, attempt int) (stageActivityResult, error) {
		var result stageActivityResult
		attemptEnv := env
		attemptEnv.Attempt = int32(attempt)
		err := workflow.ExecuteActivity(ctx, ActRunDeterministic, attemptEnv, run, workspaceBranch).Get(ctx, &result)
		result.Integrity = produced
		return result, err
	})
}

// evaluateGate dispatches one gate evaluation and returns the evaluator
// outcome plus, for an agentic gate, the reviewer's full Verdict (journaled as
// the verdict artifact alongside gate.evaluated, mirroring internal/gate's
// recordVerdict).
func evaluateGate(ctx workflow.Context, machine *wf.Machine, g apiv1.Gate, in RunInput, subject apiv1.ResultEnvelope, upstream []apiv1.ContextPointer, workspaceBranch string, gateAttempts map[string]int, rec *runJournal) (string, *apiv1.Verdict, error) {
	limits, err := wf.GateLimits(machine, g)
	if err != nil {
		return "", nil, fmt.Errorf("project gate %q limits: %w", g.Name, err)
	}
	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		conf := apiv1.AutomatedGate{}
		if g.Automated != nil {
			conf = *g.Automated
		}
		// An automated gate gets no workspace, capabilities, or context
		// pointers — its checks are pure functions over env.Inputs alone,
		// matching the local runner (#112). Per the runner-contract
		// convention (internal/gate/automated.go): a gate never receives the
		// subject stage's ResultEnvelope over the wire envelope (§2.4), so
		// the subject's status and small outputs are flattened into the
		// gate's own Inputs before dispatch.
		env := buildInvocation(in, g.Name, "gate: "+g.Name, nil, nil, limits, nil, "")
		env.Inputs, err = gate.AutomatedInputs(subject)
		if err != nil {
			return "", nil, fmt.Errorf("project gate %q inputs: %w", g.Name, err)
		}
		ctx := stageActivityContext(ctx, env.Limits)
		rec.gateStarted(ctx, g.Name, gateAttempts[g.Name]+1)
		// Pre-evaluation emission: gate.paused + gate.started go live before
		// the evaluator dispatches, so a run waiting at a gate is visible
		// waiting at that gate.
		if err := rec.emitPending(ctx); err != nil {
			return "", nil, err
		}
		var outcome string
		if err := evaluateWithInfraRetry(ctx, g, rec, func(ctx workflow.Context) error {
			return workflow.ExecuteActivity(ctx, ActEvaluateAutomated, conf, env).Get(ctx, &outcome)
		}); err != nil {
			return "", nil, err
		}
		return outcome, nil, nil

	case apiv1.EvaluatorAgentic:
		// The reviewer runs a real goober subprocess, so — unlike an
		// automated/human gate — it needs its capability-scoped credentials
		// (#294). AgenticGate carries no stage-level capabilities, so they are
		// sourced from the reviewer goober's own grants, pinned at start.
		var gateCaps []string
		var reviewerGoober string
		if g.Agentic != nil {
			reviewerGoober = g.Agentic.Goober
			gateCaps = in.GateGooberCapabilities[reviewerGoober]
		}
		env := buildInvocation(in, g.Name, "gate: "+g.Name, nil, gateCaps, limits, upstream, reviewerGoober)
		ctx := stageActivityContext(ctx, env.Limits)
		rec.gateStarted(ctx, g.Name, gateAttempts[g.Name]+1)
		// Pre-evaluation emission, as on the automated arm above.
		if err := rec.emitPending(ctx); err != nil {
			return "", nil, err
		}
		var verdict apiv1.Verdict
		if err := evaluateWithInfraRetry(ctx, g, rec, func(ctx workflow.Context) error {
			return workflow.ExecuteActivity(ctx, ActReviewGoober, env, workspaceBranch).Get(ctx, &verdict)
		}); err != nil {
			return "", nil, err
		}

		return string(verdict.Decision), &verdict, nil

	case apiv1.EvaluatorHuman:
		var decision string
		workflow.GetSignalChannel(ctx, HumanGateSignal(g.Name)).Receive(ctx, &decision)
		return decision, nil, nil

	default:
		return "", nil, fmt.Errorf("gate %q has unknown evaluator %q", g.Name, g.Evaluator)
	}
}

func selectedWorkspaceBranch(t apiv1.Task, result apiv1.ResultEnvelope, namespace string) (string, error) {
	if t.Type != apiv1.TaskDeterministic {
		return "", nil
	}
	raw, exists := result.Outputs[runner.WorkspaceBranchOutput]
	if !exists {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", runner.WorkspaceBranchOutput, raw)
	}
	branch := strings.TrimSpace(value)
	if branch == "" {
		return "", nil
	}
	normalizedNamespace := providers.NormalizeBranchNamespace(namespace)
	if !strings.HasPrefix(branch, normalizedNamespace) {
		return "", fmt.Errorf("%s %q is outside namespace %q", runner.WorkspaceBranchOutput, branch, normalizedNamespace)
	}
	return branch, nil
}

// buildInvocation assembles a stage invocation envelope to the closed
// invocation schema, mirroring the local runner's buildEnvelope
// (internal/runner/run.go) field for field: identity, trigger, branch
// namespace, base branch, goal, repo, item, read-only context pointers,
// capability grants, limits, and static inputs (#621). The one field
// deliberately absent here is
// Workspace: provisioning a working copy is a side effect, so the activity
// host provisions one fresh per attempt and stamps it into the envelope
// before the stage executes (Activities.provisionWorkspace) — failing closed,
// never dispatching a partial envelope.
//
// goober is the ONE field the local runner's buildEnvelope deliberately
// omits and the engine must set (#2904): the local runner dispatches from
// the workflow Definition and hands the goober name to its executor factory
// directly, so its envelope never needed to carry it. A Temporal worker has
// only the envelope — invoke.Goober.Invoke(ctx, env) is the whole signature
// the worker seam dispatches through — so leaving it empty here strands
// every agentic activity with no goober identity to route on. Empty for a
// deterministic task or an automated gate.
func buildInvocation(in RunInput, stateName, goal string, taskInputs map[string]string, capabilities []string, limits apiv1.Limits, upstream []apiv1.ContextPointer, goober string) apiv1.InvocationEnvelope {
	inputs := make(map[string]interface{}, len(taskInputs))
	for k, v := range taskInputs {
		inputs[k] = v
	}
	// BaseBranch mirrors the local runner's fallback (internal/runner/run.go,
	// #2087): RepoRef.Branch is the branch every worktree is actually forked
	// from, defaulting to "main" when unset.
	baseBranch := in.RepoRef.Branch
	if baseBranch == "" {
		baseBranch = "main"
	}
	return apiv1.InvocationEnvelope{
		TaskID:          in.RunID + ":" + stateName,
		WorkflowID:      in.WorkflowName,
		RunID:           in.RunID,
		TriggerRef:      in.TriggerRef,
		Gaggle:          in.Gaggle,
		BranchNamespace: in.BranchNamespace,
		BaseBranch:      baseBranch,
		Goal:            goal,
		Goober:          goober,
		RepoRef:         in.RepoRef.EnvelopeRef(),
		Item:            in.Item,
		ContextPointers: upstream,
		Capabilities:    capabilities,
		Limits:          limits,
		Inputs:          inputs,
	}
}

func normalizeItemIntegrity(item *apiv1.BacklogItem) *apiv1.BacklogItem {
	if item == nil || item.Integrity.Valid() {
		return item
	}
	normalized := *item
	normalized.Integrity = apiv1.IntegrityUnapproved
	return &normalized
}

// contextPointersFor converts a finished stage's artifacts into the read-only
// context pointers handed to downstream stages, mirroring the local runner's
// contextPointersFor (internal/runner/run.go) so both runners name upstream
// evidence identically.
func contextPointersFor(stageName string, artifacts []apiv1.ArtifactPointer) []apiv1.ContextPointer {
	out := make([]apiv1.ContextPointer, 0, len(artifacts))
	for i := range artifacts {
		a := artifacts[i]
		if !a.Integrity.Valid() {
			a.Integrity = apiv1.IntegrityDerived
		}
		out = append(out, apiv1.ContextPointer{
			Name: fmt.Sprintf("%s.artifact[%d]", stageName, i), Integrity: a.Integrity, Artifact: &a,
		})
	}
	return out
}

func normalizeArtifactIntegrity(taskType apiv1.TaskType, artifacts []apiv1.ArtifactPointer) []apiv1.ArtifactPointer {
	for i := range artifacts {
		if taskType == apiv1.TaskAgentic || !artifacts[i].Integrity.Valid() {
			artifacts[i].Integrity = apiv1.IntegrityDerived
		}
	}
	return artifacts
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stageActivityContext(ctx workflow.Context, limits apiv1.Limits) workflow.Context {
	return workflow.WithActivityOptions(ctx, stageActivityOptions(limits, ""))
}

// stageActivityOptions builds the options every engine activity dispatches
// under. The RetryPolicy is always explicit with a single attempt, so
// Temporal's unlimited-attempts default is structurally unreachable for any
// task shape (#622/#156); retry orchestration lives in dispatchWithRetry,
// which enforces the local runner's split policy/infrastructure budgets. A
// declared duration limit is padded with stageTimeoutGrace so the worker's
// own policy-classed enforcement of that limit always fires first.
// taskQueue empty means inherit the workflow's queue, which is what every
// stage did before per-stage placement existed and what an all-linux instance
// still does.
//
// ScheduleToStartTimeout is set because the SDK's default is unlimited: a stage
// routed to a queue no worker serves would otherwise wait forever, with the run
// simply never progressing and nothing to look at. Bounded, it fails with a
// timeout naming the queue.
func stageActivityOptions(limits apiv1.Limits, taskQueue string) workflow.ActivityOptions {
	timeout := activityTimeout
	if limits.MaxDurationSeconds > 0 {
		timeout = time.Duration(limits.MaxDurationSeconds)*time.Second + stageTimeoutGrace
	}
	return workflow.ActivityOptions{
		TaskQueue:              taskQueue,
		StartToCloseTimeout:    timeout,
		ScheduleToStartTimeout: stageScheduleToStart,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// stageActivityContextOn is stageActivityContext with per-stage placement: the
// activity is dispatched to the task queue its platform capability names,
// instead of inheriting the workflow's.
//
// This is the whole of per-stage routing. Temporal supports a TaskQueue per
// ACTIVITY (ActivityOptions.TaskQueue), so the workflow keeps running on one
// queue while individual stages are polled by workers elsewhere — which is why
// engine.NewTemporalStarter taking a single queue was never the obstacle it
// looked like. It is the workflow's queue, and that is correct.
func stageActivityContextOn(ctx workflow.Context, limits apiv1.Limits, capabilities []string) workflow.Context {
	return workflow.WithActivityOptions(ctx, stageActivityOptions(limits, stageTaskQueue(ctx, capabilities)))
}

// stageTaskQueue derives a stage's task queue from its declared platform
// capability: a stage naming os=<goos> is polled from "<workflow queue>-<goos>",
// and anything else inherits the workflow's own queue. Unlabelled therefore
// means linux, which is the documented default, and an all-linux instance needs
// no extra queues.
//
// Returning empty means inherit; it is not an error. A workflow with no
// platform capabilities behaves exactly as before this existed.
func stageTaskQueue(ctx workflow.Context, capabilities []string) string {
	suffix := platformQueueSuffix(capabilities)
	if suffix == "" {
		return ""
	}
	return workflow.GetInfo(ctx).TaskQueueName + "-" + suffix
}

// platformQueueSuffix is the queue suffix a stage's capabilities ask for, or
// empty to inherit. Split out from stageTaskQueue so the placement rule is
// testable as a pure function rather than only through a workflow environment.
func platformQueueSuffix(capabilities []string) string {
	for _, c := range capabilities {
		goos, ok := strings.CutPrefix(c, "os=")
		if !ok || goos == "" || goos == "linux" {
			continue
		}
		return goos
	}
	return ""
}

// engineInputGrades maps each inputsFrom entry to the provenance of the task
// that produced it. The engine resolves only against the immediately preceding
// task's Outputs, so every entry carries that task's grade.
func engineInputGrades(t apiv1.Task, upstreamResult apiv1.ResultEnvelope) map[string]apiv1.Integrity {
	if len(t.InputsFrom) == 0 {
		return nil
	}
	grades := make(map[string]apiv1.Integrity, len(t.InputsFrom))
	for inputKey := range t.InputsFrom {
		grades[inputKey] = upstreamResult.Integrity
	}
	return grades
}

// engineProducedIntegrity grades what a task emitted, on the same rule the local
// runner applies: the weakest input it was admitted with, with agentic output
// always contributing IntegrityDerived.
func engineProducedIntegrity(t apiv1.Task, env apiv1.InvocationEnvelope, upstreamResult apiv1.ResultEnvelope) apiv1.Integrity {
	grades := make([]apiv1.Integrity, 0, len(env.ContextPointers)+len(t.InputsFrom)+2)
	if t.Type == apiv1.TaskAgentic {
		grades = append(grades, apiv1.IntegrityDerived)
	}
	if env.Item != nil && env.Item.Integrity != "" {
		grades = append(grades, env.Item.Integrity)
	}
	for i := range env.ContextPointers {
		grade := env.ContextPointers[i].Integrity
		if grade == "" && env.ContextPointers[i].Artifact != nil {
			grade = env.ContextPointers[i].Artifact.Integrity
		}
		if grade != "" {
			grades = append(grades, grade)
		}
	}
	if len(t.InputsFrom) > 0 && upstreamResult.Integrity != "" {
		grades = append(grades, upstreamResult.Integrity)
	}
	if len(grades) == 0 {
		return apiv1.IntegrityTrusted
	}
	return apiv1.WeakestIntegrity(grades...)
}
