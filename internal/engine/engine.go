package engine

import (
	"fmt"
	"sort"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
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
	// activityTimeout is the start-to-close timeout applied to every activity.
	// A constant (not wall-clock) keeps the workflow deterministic.
	activityTimeout = time.Hour
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
	// MaxRepasses overrides the shared repass budget (gate.DefaultMaxRepasses)
	// when > 0, mirroring the local runner's Config.MaxRepasses — pinned at
	// start like the rest of the run's policy (#624).
	MaxRepasses int `json:"maxRepasses,omitempty"`
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
	m, err := wf.Compile(
		wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec},
		wf.WithPreviewFeatures(in.previewFeaturesEnabled()),
	)
	if err != nil {
		return RunResult{}, err
	}
	rec, err := newRunJournal(ctx, in, m)
	if err != nil {
		return RunResult{}, err
	}
	rec.runStarted(ctx)
	rec.recordRunBranchUpfront(ctx, in)

	res, err := walk(ctx, in, m, rec)
	if err != nil {
		// A walk-level error is the engine's failTerminal (#305): record the
		// cause and the failed terminal in the projection, then fail the
		// workflow. A canceled run is the one exception — it has no terminal.
		if !temporal.IsCanceledError(err) && ctx.Err() == nil {
			rec.runFailedCause(ctx, "", "", err.Error())
			rec.runFinished(ctx, journal.PhaseFailed)
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
	return res, nil
}

func walk(ctx workflow.Context, in RunInput, m *wf.Machine, rec *runJournal) (RunResult, error) {
	logger := workflow.GetLogger(ctx)
	upstream := map[string]apiv1.ResultEnvelope{}
	// pointers accumulates every completed stage's artifacts as read-only
	// ContextPointers — the only channel through which a stage consumes prior
	// work (§2.4) — exactly as the local runner's walk does.
	var pointers []apiv1.ContextPointer
	// gateAttempts holds each gate's consecutive non-pass count — the same
	// per-run repass state gate.Evaluator.Attempts tracks locally.
	gateAttempts := map[string]int{}
	var lastStage string
	var lastResult apiv1.ResultEnvelope
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
			res, terr := runTask(ctx, in, m, t, pointers, lastResult, rec)
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
			outcome, verdict, gerr := evaluateGate(ctx, m, g, in, lastResult, pointers, gateAttempts, rec)
			if gerr != nil {
				return RunResult{}, gerr
			}
			gr, rerr := resolveGateOutcome(g, outcome, gateAttempts, maxRepassesFor(in))
			if rerr != nil {
				return RunResult{}, rerr
			}
			if jerr := rec.gateEvaluated(ctx, gr, verdict); jerr != nil {
				return RunResult{}, jerr
			}
			logger.Info("gate evaluated", "gate", g.Name, "outcome", gr.Outcome, "next", gr.Target, "attempt", gr.Attempt, "escalated", gr.Escalated)
			next, out, terminal := gateTransition(m, gr, lastStage, lastResult, upstream, steps)
			if terminal {
				return out, nil
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

func runTask(ctx workflow.Context, in RunInput, machine *wf.Machine, t apiv1.Task, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, rec *runJournal) (apiv1.ResultEnvelope, error) {
	inputs, err := wf.TaskInvocationInputs(machine, t)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("project task %q inputs: %w", t.Name, err)
	}
	limits, err := wf.TaskLimits(machine, t)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("project task %q limits: %w", t.Name, err)
	}
	env := buildInvocation(in, t.Name, t.Goal, inputs, t.Capabilities, limits, upstream)
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
	ctx = stageActivityContext(ctx, env.Limits)
	if t.Type == apiv1.TaskAgentic {
		return dispatchWithRetry(ctx, t, rec, env.ContextPointers, func(ctx workflow.Context) (apiv1.ResultEnvelope, error) {
			var res apiv1.ResultEnvelope
			err := workflow.ExecuteActivity(ctx, ActInvokeGoober, env).Get(ctx, &res)
			return res, err
		})
	}
	// Fail closed on an absent or zero-value run (#626/#156): a
	// DeterministicRun{} previously masked nil and dispatched an empty
	// command. The registry rejects these shapes at registration; this guard
	// covers a RunInput constructed without it.
	if t.Run == nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("task %q is deterministic but declares no DeterministicRun", t.Name)
	}
	if len(t.Run.Command) == 0 {
		return apiv1.ResultEnvelope{}, fmt.Errorf("task %q run declares no command; refusing to dispatch an empty command", t.Name)
	}
	run := *t.Run
	return dispatchWithRetry(ctx, t, rec, env.ContextPointers, func(ctx workflow.Context) (apiv1.ResultEnvelope, error) {
		var res apiv1.ResultEnvelope
		err := workflow.ExecuteActivity(ctx, ActRunDeterministic, env, run).Get(ctx, &res)
		return res, err
	})
}

// evaluateGate dispatches one gate evaluation and returns the evaluator
// outcome plus, for an agentic gate, the reviewer's full Verdict (journaled as
// the verdict artifact alongside gate.evaluated, mirroring internal/gate's
// recordVerdict).
func evaluateGate(ctx workflow.Context, machine *wf.Machine, g apiv1.Gate, in RunInput, subject apiv1.ResultEnvelope, upstream []apiv1.ContextPointer, gateAttempts map[string]int, rec *runJournal) (string, *apiv1.Verdict, error) {
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
		env := buildInvocation(in, g.Name, "gate: "+g.Name, nil, nil, limits, nil)
		env.Inputs = make(map[string]interface{}, 1+len(subject.Outputs))
		env.Inputs[gate.InputKeyStatus] = string(subject.Status)
		for k, v := range subject.Outputs {
			env.Inputs[k] = v
		}
		ctx := stageActivityContext(ctx, env.Limits)
		rec.gateStarted(ctx, g.Name, gateAttempts[g.Name]+1)
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
		if g.Agentic != nil {
			gateCaps = in.GateGooberCapabilities[g.Agentic.Goober]
		}
		env := buildInvocation(in, g.Name, "gate: "+g.Name, nil, gateCaps, limits, upstream)
		ctx := stageActivityContext(ctx, env.Limits)
		rec.gateStarted(ctx, g.Name, gateAttempts[g.Name]+1)
		var verdict apiv1.Verdict
		if err := evaluateWithInfraRetry(ctx, g, rec, func(ctx workflow.Context) error {
			return workflow.ExecuteActivity(ctx, ActReviewGoober, env).Get(ctx, &verdict)
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

// buildInvocation assembles a stage invocation envelope to the closed
// invocation schema, mirroring the local runner's buildEnvelope
// (internal/runner/run.go) field for field: identity, trigger, branch
// namespace, goal, repo, item, read-only context pointers, capability grants,
// limits, and static inputs (#621). The one field deliberately absent here is
// Workspace: provisioning a working copy is a side effect, so the activity
// host provisions one fresh per attempt and stamps it into the envelope
// before the stage executes (Activities.provisionWorkspace) — failing closed,
// never dispatching a partial envelope.
func buildInvocation(in RunInput, stateName, goal string, taskInputs map[string]string, capabilities []string, limits apiv1.Limits, upstream []apiv1.ContextPointer) apiv1.InvocationEnvelope {
	inputs := make(map[string]interface{}, len(taskInputs))
	for k, v := range taskInputs {
		inputs[k] = v
	}
	return apiv1.InvocationEnvelope{
		TaskID:          in.RunID + ":" + stateName,
		WorkflowID:      in.WorkflowName,
		RunID:           in.RunID,
		TriggerRef:      in.TriggerRef,
		Gaggle:          in.Gaggle,
		BranchNamespace: in.BranchNamespace,
		Goal:            goal,
		RepoRef:         in.RepoRef,
		Item:            in.Item,
		ContextPointers: upstream,
		Capabilities:    capabilities,
		Limits:          limits,
		Inputs:          inputs,
	}
}

// contextPointersFor converts a finished stage's artifacts into the read-only
// context pointers handed to downstream stages, mirroring the local runner's
// contextPointersFor (internal/runner/run.go) so both runners name upstream
// evidence identically.
func contextPointersFor(stageName string, artifacts []apiv1.ArtifactPointer) []apiv1.ContextPointer {
	out := make([]apiv1.ContextPointer, 0, len(artifacts))
	for i := range artifacts {
		a := artifacts[i]
		out = append(out, apiv1.ContextPointer{Name: fmt.Sprintf("%s.artifact[%d]", stageName, i), Artifact: &a})
	}
	return out
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
	return workflow.WithActivityOptions(ctx, stageActivityOptions(limits))
}

// stageActivityOptions builds the options every engine activity dispatches
// under. The RetryPolicy is always explicit with a single attempt, so
// Temporal's unlimited-attempts default is structurally unreachable for any
// task shape (#622/#156); retry orchestration lives in dispatchWithRetry,
// which enforces the local runner's split policy/infrastructure budgets.
func stageActivityOptions(limits apiv1.Limits) workflow.ActivityOptions {
	timeout := activityTimeout
	if limits.MaxDurationSeconds > 0 {
		timeout = time.Duration(limits.MaxDurationSeconds) * time.Second
	}
	return workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}
