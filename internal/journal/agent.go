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

// AgentTree returns the latest agent state keyed by invocation ID. It accepts
// live journals, so unfinished agents remain visible to callers.
func AgentTree(events []Event) (map[string]AgentProvenance, error) {
	tree := make(map[string]AgentProvenance)
	for _, event := range events {
		if event.Agent == nil {
			continue
		}
		if event.Agent.ID == "" {
			return nil, fmt.Errorf("journal: agent event has empty id")
		}
		tree[event.Agent.ID] = *event.Agent
	}
	return tree, nil
}

// ActiveAgentTree returns agents that have not reached a terminal lifecycle.
func ActiveAgentTree(events []Event) (map[string]AgentProvenance, error) {
	tree, err := AgentTree(events)
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
	latest := make(map[string]AgentProvenance)
	for _, event := range events {
		if event.Agent == nil || event.Agent.ID == "" {
			continue
		}
		latest[event.Agent.ID] = *event.Agent
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
