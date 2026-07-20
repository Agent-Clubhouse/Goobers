package engine

import (
	"fmt"
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
	RunID          string             `json:"runId"`
	Gaggle         string             `json:"gaggle"`
	WorkflowName   string             `json:"workflowName"`
	Version        int                `json:"version"`
	WorkflowDigest string             `json:"workflowDigest"`
	Spec           apiv1.WorkflowSpec `json:"spec"`
	RepoRef        apiv1.RepoRef      `json:"repoRef"`
	Item           *apiv1.BacklogItem `json:"item,omitempty"`
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
	m, err := wf.Compile(wf.Definition{Name: in.WorkflowName, Version: in.Version, Spec: in.Spec})
	if err != nil {
		return RunResult{}, err
	}

	upstream := map[string]apiv1.ResultEnvelope{}
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
			res, terr := runTask(ctx, in, t)
			if terr != nil {
				return RunResult{}, terr
			}
			upstream[t.Name] = res
			logger.Info("task complete", "task", t.Name, "status", res.Status)
			state = t.Next
			continue
		}

		if g, ok := m.Gate(state); ok {
			outcome, gerr := evaluateGate(ctx, g, in)
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

func runTask(ctx workflow.Context, in RunInput, t apiv1.Task) (apiv1.ResultEnvelope, error) {
	env := buildInvocation(in, t.Name, t.Goal, t.Inputs, wf.TaskLimits(t))
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

func evaluateGate(ctx workflow.Context, g apiv1.Gate, in RunInput) (string, error) {
	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		conf := apiv1.AutomatedGate{}
		if g.Automated != nil {
			conf = *g.Automated
		}
		env := buildInvocation(in, g.Name, "automated gate: "+g.Name, nil, wf.GateLimits(g))
		ctx := stageActivityContext(ctx, env.Limits)
		var outcome string
		if err := workflow.ExecuteActivity(ctx, ActEvaluateAutomated, conf, env).Get(ctx, &outcome); err != nil {
			return "", err
		}
		return outcome, nil

	case apiv1.EvaluatorAgentic:
		env := buildInvocation(in, g.Name, "review gate: "+g.Name, nil, wf.GateLimits(g))
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

// buildInvocation assembles a stage invocation envelope. Under the V0 stage
// contract it does not inject upstream stage results: a stage consumes prior work
// only through journal-backed ContextPointers (ARCHITECTURE.md §2.4). This
// superseded Temporal engine has no local journal to point into, so it passes no
// context pointers; the local runner (#17) populates them. The run's per-stage
// results still aggregate into RunResult.Outputs for the run's own return value.
func buildInvocation(in RunInput, stateName, goal string, inputs map[string]string, limits apiv1.Limits) apiv1.InvocationEnvelope {
	env := apiv1.InvocationEnvelope{
		TaskID:     in.RunID + ":" + stateName,
		WorkflowID: in.WorkflowName,
		RunID:      in.RunID,
		Gaggle:     in.Gaggle,
		Goal:       goal,
		RepoRef:    in.RepoRef,
		Item:       in.Item,
		Limits:     limits,
	}
	if len(inputs) > 0 {
		env.Inputs = make(map[string]interface{}, len(inputs))
		for k, v := range inputs {
			env.Inputs[k] = v
		}
	}
	return env
}

func stageActivityContext(ctx workflow.Context, limits apiv1.Limits) workflow.Context {
	timeout := activityTimeout
	if limits.MaxDurationSeconds > 0 {
		timeout = time.Duration(limits.MaxDurationSeconds) * time.Second
	}
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: timeout})
}
