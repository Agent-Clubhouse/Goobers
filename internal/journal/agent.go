package journal

import (
	"fmt"
	"sort"
	"time"
)

// AgentLifecycle is the deliberately small, engine-neutral lifecycle taxonomy.
type AgentLifecycle string

const (
	AgentStarted   AgentLifecycle = "started"
	AgentWaiting   AgentLifecycle = "waiting"
	AgentResumed   AgentLifecycle = "resumed"
	AgentCompleted AgentLifecycle = "completed"
	AgentFailed    AgentLifecycle = "failed"
	AgentCancelled AgentLifecycle = "cancelled"
)

const (
	AgentFidelityFull    = "full"
	AgentFidelityPartial = "partial"
	AgentFidelityNone    = "none"
)

// AgentUsage contains observed usage. Nil values mean that the adapter did not
// report that measure; zero is an observed zero.
type AgentUsage struct {
	InputTokens  *int64   `json:"inputTokens,omitempty"`
	OutputTokens *int64   `json:"outputTokens,omitempty"`
	CostUSD      *float64 `json:"costUsd,omitempty"`
}

// AgentProvenance is invocation-local identity and the latest known state of a
// nested agent. ParentID and DependsOn express the execution graph.
type AgentProvenance struct {
	Schema          string         `json:"schema"`
	ID              string         `json:"id"`
	ParentID        string         `json:"parentId,omitempty"`
	RunID           string         `json:"runId"`
	Stage           string         `json:"stage"`
	Attempt         int            `json:"attempt"`
	Plugin          string         `json:"plugin,omitempty"`
	Objective       string         `json:"objective,omitempty"`
	Coordinator     bool           `json:"coordinator,omitempty"`
	Worker          bool           `json:"worker,omitempty"`
	Leaf            bool           `json:"leaf,omitempty"`
	RequestedModel  string         `json:"requestedModel,omitempty"`
	ResolvedModel   string         `json:"resolvedModel,omitempty"`
	ReasoningEffort string         `json:"reasoningEffort,omitempty"`
	Lifecycle       AgentLifecycle `json:"lifecycle"`
	StartedAt       time.Time      `json:"startedAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
	Budget          AgentUsage     `json:"budget,omitempty"`
	Usage           AgentUsage     `json:"usage,omitempty"`
	Results         []Ref          `json:"results,omitempty"`
	DependsOn       []string       `json:"dependsOn,omitempty"`
	Fidelity        string         `json:"fidelity,omitempty"`
}

// PeerMessageMetadata describes only the orchestration effect of a peer
// message. Content is intentionally absent.
type PeerMessageMetadata struct {
	ID          string    `json:"id"`
	SenderID    string    `json:"senderId"`
	RecipientID string    `json:"recipientId"`
	OccurredAt  time.Time `json:"occurredAt"`
	Purpose     string    `json:"purpose"`
	Artifact    *Ref      `json:"artifact,omitempty"`
	ContentHash string    `json:"contentHash,omitempty"`
}

// ValidateAgentEvent rejects malformed adapter projections before they reach
// the durable journal. It intentionally does not inspect or retain message
// bodies because peer metadata has no body field.
func ValidateAgentEvent(event Event) error {
	switch event.Type {
	case EventAgentLifecycle:
		if event.Agent == nil {
			return fmt.Errorf("journal: agent lifecycle event has no agent")
		}
		return validateAgent(*event.Agent)
	case EventAgentMessage:
		if event.PeerMessage == nil || event.PeerMessage.ID == "" ||
			event.PeerMessage.SenderID == "" || event.PeerMessage.RecipientID == "" ||
			event.PeerMessage.Purpose == "" {
			return fmt.Errorf("journal: invalid peer-message metadata")
		}
		return nil
	default:
		return fmt.Errorf("journal: unsupported nested-agent event %q", event.Type)
	}
}

// AgentTree returns the latest agent state keyed by invocation ID. It accepts
// live journals, so unfinished agents remain visible to callers.
func AgentTree(events []Event) (map[string]AgentProvenance, error) {
	tree := make(map[string]AgentProvenance)
	for _, event := range events {
		if event.Type != EventAgentLifecycle {
			continue
		}
		if event.Agent == nil {
			return nil, fmt.Errorf("journal: agent lifecycle event has no agent")
		}
		if err := validateAgent(*event.Agent); err != nil {
			return nil, err
		}
		current, ok := tree[event.Agent.ID]
		if !ok || newerAgentEvent(event.Agent, &current) {
			tree[event.Agent.ID] = *event.Agent
		}
	}
	return tree, nil
}

// ActiveAgentTree returns agents that have not reached a terminal lifecycle.
func ActiveAgentTree(events []Event) (map[string]AgentProvenance, error) {
	scopeRun, scopeStage := "", ""
	for _, event := range events {
		if event.Type == EventAgentLifecycle && event.Agent != nil {
			scopeRun, scopeStage = event.Agent.RunID, event.Agent.Stage
			break
		}
	}
	return activeAgentTree(events, scopeRun, scopeStage, 0)
}

// ActiveAgentTreeForStage reconstructs the active tree for one in-flight stage
// attempt. Events from other runs and stages are deliberately ignored.
func ActiveAgentTreeForStage(events []Event, runID, stage string, attempt int) (map[string]AgentProvenance, error) {
	return activeAgentTree(events, runID, stage, attempt)
}

func activeAgentTree(events []Event, runID, stage string, attempt int) (map[string]AgentProvenance, error) {
	scoped := make([]Event, 0, len(events))
	for _, event := range events {
		if event.Type != "" && event.Type != EventAgentLifecycle || event.Agent == nil ||
			event.Agent.RunID != runID || event.Agent.Stage != stage ||
			(attempt > 0 && event.Agent.Attempt != attempt) {
			continue
		}
		scoped = append(scoped, event)
	}
	tree, err := AgentTree(scoped)
	if err != nil {
		return nil, err
	}
	for id, agent := range tree {
		switch agent.Lifecycle {
		case AgentCompleted, AgentFailed, AgentCancelled:
			delete(tree, id)
		}
	}
	return tree, nil
}

// RollupAgentUsage sums each invocation once, excluding coordinator usage when
// it is also represented by child totals and excluding non-final attempts.
func RollupAgentUsage(events []Event) AgentUsage {
	runID, stage := "", ""
	for _, event := range events {
		if event.Type == EventAgentLifecycle && event.Agent != nil {
			runID, stage = event.Agent.RunID, event.Agent.Stage
			break
		}
	}
	return rollupAgentUsage(events, runID, stage)
}

// RollupAgentUsageForStage rolls up worker usage for one run and stage.
func RollupAgentUsageForStage(events []Event, runID, stage string) AgentUsage {
	return rollupAgentUsage(events, runID, stage)
}

func rollupAgentUsage(events []Event, runID, stage string) AgentUsage {
	latest := make(map[string]AgentProvenance)
	for _, event := range events {
		if event.Type != "" && event.Type != EventAgentLifecycle || event.Agent == nil ||
			event.Agent.ID == "" || event.Agent.Coordinator ||
			(runID != "" && event.Agent.RunID != runID) ||
			(stage != "" && event.Agent.Stage != stage) {
			continue
		}
		current, ok := latest[event.Agent.ID]
		if !ok || newerAgentEvent(event.Agent, &current) {
			latest[event.Agent.ID] = *event.Agent
		} else if event.Agent.Attempt == current.Attempt {
			mergeAgentUsage(&current.Usage, event.Agent.Usage)
			latest[event.Agent.ID] = current
		}
	}
	var result AgentUsage
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		u := latest[key].Usage
		if u.InputTokens != nil {
			if result.InputTokens == nil {
				result.InputTokens = new(int64)
			}
			*result.InputTokens += *u.InputTokens
		}
		if u.OutputTokens != nil {
			if result.OutputTokens == nil {
				result.OutputTokens = new(int64)
			}
			*result.OutputTokens += *u.OutputTokens
		}
		if u.CostUSD != nil {
			if result.CostUSD == nil {
				result.CostUSD = new(float64)
			}
			*result.CostUSD += *u.CostUSD
		}
	}
	return result
}

func validateAgent(agent AgentProvenance) error {
	if agent.Schema != "goobers.dev/journal/agent/v1" || agent.ID == "" ||
		agent.RunID == "" || agent.Stage == "" || agent.Attempt < 1 {
		return fmt.Errorf("journal: invalid nested-agent identity %q", agent.ID)
	}
	switch agent.Lifecycle {
	case AgentStarted, AgentWaiting, AgentResumed, AgentCompleted, AgentFailed, AgentCancelled:
	default:
		return fmt.Errorf("journal: invalid nested-agent lifecycle %q", agent.Lifecycle)
	}
	return nil
}

func newerAgentEvent(candidate *AgentProvenance, current *AgentProvenance) bool {
	if candidate.Attempt != current.Attempt {
		return candidate.Attempt > current.Attempt
	}
	if candidate.UpdatedAt.After(current.UpdatedAt) {
		return true
	}
	return candidate.UpdatedAt.Equal(current.UpdatedAt) && candidate.Lifecycle != "" &&
		current.Lifecycle == ""
}

func mergeAgentUsage(dst *AgentUsage, src AgentUsage) {
	if dst.InputTokens == nil {
		dst.InputTokens = src.InputTokens
	}
	if dst.OutputTokens == nil {
		dst.OutputTokens = src.OutputTokens
	}
	if dst.CostUSD == nil {
		dst.CostUSD = src.CostUSD
	}
}
