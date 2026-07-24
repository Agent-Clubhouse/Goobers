package engine

import (
	"fmt"
	"sort"
	"time"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

// Run statuses.
const (
	StatusCompleted = "completed"
	StatusBlocked   = "blocked"
	StatusEscalated = "escalated"
)

const (
	// maxSteps bounds the number of state transitions in a single run, a
	// last-resort guard against a definition that loops (WF-015 within a run).
	maxSteps = 1000
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
func Run(ctx workflow.Context, in RunInput) (RunResult, error) {
	logger := workflow.GetLogger(ctx)
	m, err := wf.Compile(
		wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec},
		wf.WithPreviewFeatures(in.previewFeaturesEnabled()),
	)
	if err != nil {
		return RunResult{}, err
	}

	upstream := map[string]apiv1.ResultEnvelope{}
	// pointers accumulates every completed stage's artifacts as read-only
	// ContextPointers — the only channel through which a stage consumes prior
	// work (§2.4) — exactly as the local runner's walk does.
	var pointers []apiv1.ContextPointer
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
			res, terr := runTask(ctx, in, m, t, pointers, lastResult)
			if terr != nil {
				return RunResult{}, terr
			}
			upstream[t.Name] = res
			pointers = append(pointers, contextPointersFor(t.Name, res.Artifacts)...)
			lastResult = res
			logger.Info("task complete", "task", t.Name, "status", res.Status)
			state = t.Next
			continue
		}

		if g, ok := m.Gate(state); ok {
			outcome, gerr := evaluateGate(ctx, m, g, in, lastResult, pointers)
			if gerr != nil {
				return RunResult{}, gerr
			}
			target, ok := wf.BranchTarget(g, outcome)
			if !ok {
				return RunResult{}, fmt.Errorf("gate %q produced outcome %q with no defined branch (never a silent pass)", g.Name, outcome)
			}
			logger.Info("gate evaluated", "gate", g.Name, "outcome", outcome, "next", target)
			switch target {
			case wf.TargetAbort:
				return RunResult{Status: StatusBlocked, FinalState: g.Name, Outputs: upstream, Steps: steps}, nil
			case wf.TargetEscalate:
				return RunResult{Status: StatusEscalated, FinalState: g.Name, Outputs: upstream, Steps: steps}, nil
			}
			state = target
			continue
		}

		return RunResult{}, fmt.Errorf("unknown state %q", state)
	}
}

func runTask(ctx workflow.Context, in RunInput, machine *wf.Machine, t apiv1.Task, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope) (apiv1.ResultEnvelope, error) {
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
	var res apiv1.ResultEnvelope
	if t.Type == apiv1.TaskAgentic {
		if err := workflow.ExecuteActivity(ctx, ActInvokeGoober, env).Get(ctx, &res); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		return res, nil
	}
	run := apiv1.DeterministicRun{}
	if t.Run != nil {
		run = *t.Run
	}
	if err := workflow.ExecuteActivity(ctx, ActRunDeterministic, env, run).Get(ctx, &res); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return res, nil
}

func evaluateGate(ctx workflow.Context, machine *wf.Machine, g apiv1.Gate, in RunInput, subject apiv1.ResultEnvelope, upstream []apiv1.ContextPointer) (string, error) {
	limits, err := wf.GateLimits(machine, g)
	if err != nil {
		return "", fmt.Errorf("project gate %q limits: %w", g.Name, err)
	}
	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		conf := apiv1.AutomatedGate{}
		if g.Automated != nil {
			conf = *g.Automated
		}
		// An automated gate gets no workspace, capabilities, or context
		// pointers — its checks are pure functions over env.Inputs alone,
		// matching the local runner (#112).
		env := buildInvocation(in, g.Name, "gate: "+g.Name, nil, nil, limits, nil)
		ctx := stageActivityContext(ctx, env.Limits)
		var outcome string
		if err := workflow.ExecuteActivity(ctx, ActEvaluateAutomated, conf, env).Get(ctx, &outcome); err != nil {
			return "", err
		}
		return outcome, nil

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
		var verdict apiv1.Verdict
		if err := workflow.ExecuteActivity(ctx, ActReviewGoober, env).Get(ctx, &verdict); err != nil {
			return "", err
		}
		return string(verdict.Decision), nil

	case apiv1.EvaluatorHuman:
		var decision string
		workflow.GetSignalChannel(ctx, HumanGateSignal(g.Name)).Receive(ctx, &decision)
		return decision, nil

	default:
		return "", fmt.Errorf("gate %q has unknown evaluator %q", g.Name, g.Evaluator)
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
	timeout := activityTimeout
	if limits.MaxDurationSeconds > 0 {
		timeout = time.Duration(limits.MaxDurationSeconds) * time.Second
	}
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: timeout})
}
