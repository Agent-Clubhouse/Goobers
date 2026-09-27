package fleetdiagnostics

import (
	"errors"
	"strings"
	"time"
)

func (f *fields) mcpHealth(observed time.Time) *MCPHealth {
	if _, ok := f.values["requiredMcpState"]; !ok {
		return nil
	}
	m := &MCPHealth{State: f.text("requiredMcpState", true), Coverage: f.text("requiredMcpCoverage", true), Reason: f.text("requiredMcpReason", false), ActiveCount: f.optionalNumber("requiredMcpActiveCount"), ObservedAt: f.optionalStamp("requiredMcpObservedAt"), Workflow: f.text("requiredMcpWorkflow", false), Stage: f.text("requiredMcpStage", false), Adapter: f.text("requiredMcpAdapter", false)}
	if branch := f.optionalNumber("requiredMcpBranch"); branch != nil {
		if *branch > 1<<20 {
			f.err = errors.New("invalid MCP branch")
		} else {
			m.Branch = int(*branch)
		}
	}
	if !validMCPHealth(*m, observed) {
		f.err = errors.New("invalid MCP condition")
	}
	return m
}

func validMCPHealth(m MCPHealth, observed time.Time) bool {
	if !oneOf(m.State, "active", "recovered", "unknown") || !oneOf(m.Coverage, "complete", "partial", "unknown") {
		return false
	}
	if !oneOf(m.Reason, "", "transport_failure", "required_tool_unavailable", "authentication_failure", "tool_authorization_failure") || !oneOf(m.Adapter, "", "copilot-cli", "claude-code") {
		return false
	}
	if m.ObservedAt != nil && m.ObservedAt.After(observed) {
		return false
	}
	if m.State == "active" {
		return m.Reason != "" && m.ObservedAt != nil && m.ActiveCount != nil && *m.ActiveCount > 0 && *m.ActiveCount <= 256
	}
	if m.State == "recovered" {
		return m.Reason == "" && m.Coverage == "complete" && m.ObservedAt != nil && m.ActiveCount != nil && *m.ActiveCount == 0
	}
	return m.Reason == "" && m.ActiveCount == nil
}

func mcpReport(source *MCPHealth, live bool) *MCPHealth {
	if source == nil {
		return nil
	}
	result := *source
	if source.ObservedAt != nil {
		at := *source.ObservedAt
		result.ObservedAt = &at
	}
	if source.ActiveCount != nil {
		count := *source.ActiveCount
		result.ActiveCount = &count
	}
	if !live {
		result.State, result.Reason, result.Coverage, result.ActiveCount = "unknown", "", "unknown", nil
	}
	return &result
}

func recordMCPTransition(e *entry, health *MCPHealth, now time.Time) {
	if health == nil {
		return
	}
	state := health.State + ":" + health.Reason
	if state == e.lastMCPState {
		return
	}
	e.transitions = append(e.transitions, Transition{At: now, Condition: "required_mcp", From: e.lastMCPState, To: health.State, Reason: health.Reason})
	if len(e.transitions) > MaxTransitions {
		e.transitions = append([]Transition(nil), e.transitions[len(e.transitions)-MaxTransitions:]...)
	}
	e.lastMCPState = state
}

// DecodeMCPHealth validates the optional MCP subset of a retained observation.
// It rejects unknown MCP fields and never copies arbitrary journal payloads.
func DecodeMCPHealth(attrs map[string]any, observed time.Time) (*MCPHealth, error) {
	if len(attrs) > 48 {
		return nil, errors.New("too many diagnostic fields")
	}
	f := &fields{values: make(map[string]any)}
	for key, value := range attrs {
		if strings.HasPrefix(key, "requiredMcp") {
			f.values[key] = value
		}
	}
	health := f.mcpHealth(observed)
	return health, f.finish()
}
