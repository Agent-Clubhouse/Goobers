package runner

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/workflow"
)

// dispatchTerminalError retains the actual exhausted dispatch budget without
// inferring exhaustion from an ordinary ResultFailure or an attempt number.
type dispatchTerminalError struct {
	err             error
	stage           string
	class           journal.AttemptClass
	attempts, limit int
}

func (e *dispatchTerminalError) Error() string { return e.err.Error() }
func (e *dispatchTerminalError) Unwrap() error { return e.err }

func newTerminalCause(phase journal.RunPhase) *journal.TerminalCause {
	c := &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: phase, SelectorKind: "condition"}
	switch phase {
	case journal.PhaseAborted:
		c.Classification, c.Code, c.Target = journal.TerminalDefinedAbort, "defined_abort", workflow.TargetAbort
	case journal.PhaseEscalated:
		c.Classification, c.Code, c.Target = journal.TerminalEscalation, "escalated", workflow.TargetEscalate
	case journal.PhaseFailed:
		c.Classification, c.Code = journal.TerminalInfrastructureFailure, "run_failed"
	default:
		return nil
	}
	return c
}

// captureTerminalCause runs before best-effort cleanup can append unrelated
// errors. Explicit failure paths supply their causal event; gate/task terminal
// paths use the last decision for finalState in the current lifecycle.
func captureTerminalCause(jr *journal.Run, phase journal.RunPhase, finalState string, supplied *journal.TerminalCause) (*journal.TerminalCause, error) {
	if phase == journal.PhaseCompleted {
		return nil, nil
	}
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return nil, err
	}
	events, err := rd.Events()
	if err != nil {
		return nil, err
	}
	id, err := rd.Identity()
	if err != nil {
		return nil, err
	}
	var machine *workflow.Machine
	for _, input := range id.Inputs {
		if input.Name != journal.PinnedWorkflowDefinitionInputName {
			continue
		}
		machine, err = PinnedWorkflowMachine(rd, id)
		if err != nil {
			return nil, err
		}
		break
	}
	return BuildTerminalCause(events, phase, finalState, supplied, machine, id.RunControls, rd.ArtifactBytes)
}

// BuildTerminalCause derives a diagnostic from a deterministic event snapshot.
// Both local and Temporal drivers call it with their immutable workflow pins;
// it performs no I/O except the supplied content-addressed artifact reader.
func BuildTerminalCause(events []journal.Event, phase journal.RunPhase, finalState string, supplied *journal.TerminalCause, machine *workflow.Machine, controls *apiv1.RunControls, artifactBytes func(journal.Ref) ([]byte, error)) (*journal.TerminalCause, error) {
	c := copyTerminalCause(phase, supplied)
	if c == nil {
		return nil, nil
	}
	c.Phase = phase
	events = terminalLifecycleEvents(events)
	if supplied == nil {
		finalState = terminalDecisionState(events, finalState)
		if err := classifyTerminalCause(c, events, finalState, artifactBytes); err != nil {
			return nil, err
		}
	}
	applyTerminalBudgets(c, events, machine, controls)
	return c, nil
}

func copyTerminalCause(phase journal.RunPhase, supplied *journal.TerminalCause) *journal.TerminalCause {
	if supplied == nil {
		return newTerminalCause(phase)
	}
	c := *supplied
	if c.Retry != nil {
		budget := *c.Retry
		c.Retry = &budget
	}
	if c.Poll != nil {
		budget := *c.Poll
		c.Poll = &budget
	}
	if c.Repass != nil {
		budget := *c.Repass
		c.Repass = &budget
	}
	return &c
}

func terminalLifecycleEvents(events []journal.Event) []journal.Event {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case journal.EventRunResumed, journal.EventStageRerunRequested:
			return events[i+1:]
		case journal.EventGateOverridden:
			return events[i:]
		}
	}
	return events
}

// A gate can route through a successful disposition task before the terminal.
// Keep that gate's actual selected target as the cause.
func terminalDecisionState(events []journal.Event, finalState string) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != journal.EventStageFinished || e.Stage != finalState {
			continue
		}
		if e.Status == string(apiv1.ResultSuccess) {
			for j := i - 1; j >= 0; j-- {
				prior := events[j]
				if prior.Type != journal.EventGateEvaluated && prior.Type != journal.EventGateOverridden {
					continue
				}
				if prior.Target == finalState {
					return prior.Gate
				}
				break
			}
		}
		break
	}
	return finalState
}

func classifyTerminalCause(c *journal.TerminalCause, events []journal.Event, finalState string, artifactBytes func(journal.Ref) ([]byte, error)) error {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if !e.KnownSchema() {
			continue
		}
		// A real interruption wins over an earlier gate or stage decision.
		if terminalInterruption(c, e, finalState) {
			return nil
		}
		if (e.Type == journal.EventGateEvaluated || e.Type == journal.EventGateOverridden) && e.Gate == finalState {
			return terminalGateDecision(c, e, artifactBytes)
		}
		if e.Type == journal.EventParallelFinished && e.Parallel == finalState {
			c.Selector, c.Branch, c.Target, c.CausalEventSeq = e.Parallel, e.Branch, e.Target, e.Seq
			return nil
		}
		if e.Type == journal.EventStageFinished && e.Stage == finalState {
			c.SelectorKind, c.Selector, c.Branch, c.CausalEventSeq = "stage", e.Stage, e.Branch, e.Seq
			if e.Error != nil {
				c.Code, c.Message = e.Error.Code, e.Error.Message
			}
			return nil
		}
	}
	return nil
}

func terminalInterruption(c *journal.TerminalCause, e journal.Event, finalState string) bool {
	if e.Type != journal.EventError || e.Error == nil {
		return false
	}
	switch e.Error.Code {
	case RunCanceledErrorCode:
		c.Classification = journal.TerminalOperatorAbort
	case RunDurationExceededErrorCode:
		c.Classification = journal.TerminalPolicyExhaustion
	case "blocked_by_agent":
		if e.Stage != finalState {
			return false
		}
		c.SelectorKind = "stage"
	case RunStalledErrorCode, StageInterruptedErrorCode:
		// Preserve the terminal escalation classification.
	default:
		return false
	}
	c.Selector, c.Code, c.Message, c.CausalEventSeq = e.Stage, e.Error.Code, e.Error.Message, e.Seq
	return true
}

func terminalGateDecision(c *journal.TerminalCause, e journal.Event, artifactBytes func(journal.Ref) ([]byte, error)) error {
	c.SelectorKind, c.Selector, c.Branch = "gate", e.Gate, e.Branch
	c.Verdict, c.Target, c.CausalEventSeq = e.Verdict, e.Target, e.Seq
	c.Message = e.Rationale
	if code, ok := e.Runner["reason"].(string); ok && code != "" {
		c.Code = code
	}
	switch c.Code {
	case gate.ReasonRepassBudgetExhausted, gate.ReasonPollingBudgetExhausted:
		c.Classification = journal.TerminalPolicyExhaustion
	case gate.ReasonInfrastructureBudgetExhausted:
		c.Classification = journal.TerminalInfrastructureFailure
	}
	if e.Ref != nil && c.Message == "" {
		data, err := artifactBytes(*e.Ref)
		if err != nil {
			return fmt.Errorf("terminal verdict: %w", err)
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal(data, &verdict); err != nil {
			return fmt.Errorf("terminal verdict: %w", err)
		}
		c.Message = verdict.Rationale
		if c.Message == "" {
			c.Message = verdict.Summary
		}
	}
	c.Repass = &journal.TerminalBudget{Consumed: terminalCount(e.Runner["repassAttempt"])}
	if c.Code == gate.ReasonPollingBudgetExhausted {
		c.Poll = &journal.TerminalBudget{Consumed: terminalCount(e.Runner["pollAttempt"])}
	}
	return nil
}

func applyTerminalBudgets(c *journal.TerminalCause, events []journal.Event, machine *workflow.Machine, controls *apiv1.RunControls) {
	if machine == nil {
		// Historical test/recovery journals may lack the immutable definition;
		// do not invent a configured allowance for them.
		c.Repass, c.Poll = nil, nil
		return
	}
	// Budgets come from the immutable definition, never today's configuration.
	applyTerminalGateBudgets(c, machine, controls)
	retryStage := terminalRetryStage(c, events)
	task, ok := machine.Task(retryStage)
	if !ok || (c.SelectorKind != "stage" && c.SelectorKind != "gate") || c.Retry != nil {
		return
	}
	// Recognized non-retryable dispositions bypass the Next gate. Its escalation
	// branch can select completion while the resulting phase remains escalated.
	if c.SelectorKind == "stage" && c.Phase == journal.PhaseEscalated && escalateErrorCodes[c.Code] {
		c.Target = taskEscalationTarget(machine, task)
	}
	allowed := 0
	if task.Retry != nil {
		allowed = max(0, int(task.Retry.MaxAttempts)-1)
	}
	c.Retry = &journal.TerminalBudget{Consumed: terminalPolicyRetries(c, events, retryStage), Allowed: allowed}
}

func applyTerminalGateBudgets(c *journal.TerminalCause, machine *workflow.Machine, controls *apiv1.RunControls) {
	g, ok := machine.Gate(c.Selector)
	if !ok || c.SelectorKind != "gate" {
		return
	}
	inherited := 0
	if controls != nil {
		inherited = int(controls.MaxRepasses)
	}
	allowed := runcontrol.MaxRepassesForGate(g, inherited)
	if c.Code == gate.ReasonInfrastructureBudgetExhausted {
		allowed = runcontrol.DefaultMaxInfrastructureRepasses
	}
	if c.Poll != nil {
		c.Poll.Allowed = runcontrol.MaxTimeoutPollsForGate(g)
		c.Poll.Consumed = min(c.Poll.Consumed, c.Poll.Allowed)
	}
	if c.Repass != nil {
		c.Repass.Allowed = allowed
		// The rejected (limit+1) re-entry never executed.
		c.Repass.Consumed = min(c.Repass.Consumed, allowed)
	}
}

func terminalRetryStage(c *journal.TerminalCause, events []journal.Event) string {
	if c.SelectorKind != "gate" {
		return c.Selector
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Seq < c.CausalEventSeq && e.Type == journal.EventStageFinished && e.Branch == c.Branch {
			return e.Stage
		}
	}
	return ""
}

func terminalPolicyRetries(c *journal.TerminalCause, events []journal.Event, stage string) int {
	consumed := 0
	for _, e := range events {
		if e.Type != journal.EventStageStarted || e.Stage != stage || e.Branch != c.Branch || e.Seq > c.CausalEventSeq {
			continue
		}
		if e.AttemptClass == journal.AttemptPolicy {
			consumed++
		} else if e.AttemptClass != journal.AttemptInfra {
			consumed = 0
		}
	}
	return consumed
}

func terminalCount(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}
