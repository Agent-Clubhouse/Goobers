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
	c := supplied
	if c == nil {
		c = newTerminalCause(phase)
	} else {
		copy := *c
		c = &copy
		if c.Retry != nil {
			b := *c.Retry
			c.Retry = &b
		}
		if c.Poll != nil {
			b := *c.Poll
			c.Poll = &b
		}
		if c.Repass != nil {
			b := *c.Repass
			c.Repass = &b
		}
	}
	if c == nil {
		return nil, nil
	}
	c.Phase = phase
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunResumed || events[i].Type == journal.EventStageRerunRequested {
			events = events[i+1:]
			break
		}
		if events[i].Type == journal.EventGateOverridden {
			events = events[i:]
			break
		}
	}
	if supplied == nil {
		// A gate can route through a successful disposition task before the
		// terminal. Keep that gate's actual selected target as the cause.
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
						finalState = prior.Gate
					}
					break
				}
			}
			break
		}
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i]
			if !e.KnownSchema() {
				continue
			}
			// A real interruption wins over an earlier gate or stage decision.
			if e.Type == journal.EventError && e.Error != nil {
				switch e.Error.Code {
				case RunCanceledErrorCode:
					c.Classification = journal.TerminalOperatorAbort
				case RunDurationExceededErrorCode:
					c.Classification = journal.TerminalPolicyExhaustion
				case "blocked_by_agent":
					if e.Stage != finalState {
						continue
					}
					c.SelectorKind = "stage"
				case RunStalledErrorCode, StageInterruptedErrorCode:
					// Preserve the terminal escalation classification.
				default:
					continue
				}
				c.Selector, c.Code, c.Message, c.CausalEventSeq = e.Stage, e.Error.Code, e.Error.Message, e.Seq
				break
			}
			if (e.Type == journal.EventGateEvaluated || e.Type == journal.EventGateOverridden) && e.Gate == finalState {
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
						return nil, fmt.Errorf("terminal verdict: %w", err)
					}
					var verdict apiv1.Verdict
					if err := json.Unmarshal(data, &verdict); err != nil {
						return nil, fmt.Errorf("terminal verdict: %w", err)
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
				break
			}
			if e.Type == journal.EventParallelFinished && e.Parallel == finalState {
				c.Selector, c.Branch, c.Target, c.CausalEventSeq = e.Parallel, e.Branch, e.Target, e.Seq
				break
			}
			if e.Type == journal.EventStageFinished && e.Stage == finalState {
				c.SelectorKind, c.Selector, c.Branch, c.CausalEventSeq = "stage", e.Stage, e.Branch, e.Seq
				if e.Error != nil {
					c.Code, c.Message = e.Error.Code, e.Error.Message
				}
				break
			}
		}
	}
	// Budgets come from the immutable definition, never today's configuration.
	if machine != nil {
		if g, ok := machine.Gate(c.Selector); ok && c.SelectorKind == "gate" {
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
		retryStage := c.Selector
		if c.SelectorKind == "gate" {
			retryStage = ""
			for i := len(events) - 1; i >= 0; i-- {
				e := events[i]
				if e.Seq < c.CausalEventSeq && e.Type == journal.EventStageFinished && e.Branch == c.Branch {
					retryStage = e.Stage
					break
				}
			}
		}
		if task, ok := machine.Task(retryStage); ok && (c.SelectorKind == "stage" || c.SelectorKind == "gate") && c.Retry == nil {
			allowed := 0
			if task.Retry != nil {
				allowed = max(0, int(task.Retry.MaxAttempts)-1)
			}
			consumed := 0
			for _, e := range events {
				if e.Type != journal.EventStageStarted || e.Stage != retryStage || e.Branch != c.Branch || e.Seq > c.CausalEventSeq {
					continue
				}
				if e.AttemptClass == journal.AttemptPolicy {
					consumed++
				} else if e.AttemptClass != journal.AttemptInfra {
					consumed = 0
				}
			}
			c.Retry = &journal.TerminalBudget{Consumed: consumed, Allowed: allowed}
		}
	} else {
		// Historical test/recovery journals may lack the immutable definition;
		// do not invent a configured allowance for them.
		c.Repass, c.Poll = nil, nil
	}
	return c, nil
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
