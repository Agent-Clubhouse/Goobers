package harness

import (
	"maps"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/mcpio"
)

// WithDecisionGate lets the instance's decisionGate.agentTool grant the decide
// MCP tool to named agent tasks. Without it, or for a task that is not listed,
// no stage ever sees the tool.
func WithDecisionGate(settings *decisiongate.Settings) Option {
	return func(e *Executor) { e.decisionGate = settings }
}

func (e *Executor) decideAccess(env apiv1.InvocationEnvelope) *mcpio.DecideAccess {
	s := e.decisionGate
	if s == nil || s.EffectiveMode() == decisiongate.ModeOff || !s.AgentTool.Granted(env.TaskID) {
		return nil
	}
	grant := mcpio.DecideAccess{Settings: *s, MaxCalls: s.AgentTool.EffectiveMaxCalls()}
	grant.Settings.Gate.Thresholds = maps.Clone(s.Gate.Thresholds)
	if grant.Validate() != nil {
		return nil
	}
	return &grant
}
