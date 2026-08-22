package journal

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// AgentLifecycle is the deliberately small, engine-neutral lifecycle taxonomy.
type AgentLifecycle string

// Agent lifecycle states emitted by harness adapters.
const (
	AgentStarted   AgentLifecycle = "started"
	AgentWaiting   AgentLifecycle = "waiting"
	AgentResumed   AgentLifecycle = "resumed"
	AgentCompleted AgentLifecycle = "completed"
	AgentFailed    AgentLifecycle = "failed"
	AgentCancelled AgentLifecycle = "cancelled"
)

// Agent telemetry fidelity levels describe adapter coverage.
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
	Schema                   string         `json:"schema"`
	ID                       string         `json:"id"`
	ParentID                 string         `json:"parentId,omitempty"`
	RunID                    string         `json:"runId"`
	Stage                    string         `json:"stage"`
	Attempt                  int            `json:"attempt"`
	Plugin                   string         `json:"plugin,omitempty"`
	Objective                string         `json:"objective,omitempty"`
	Coordinator              bool           `json:"coordinator,omitempty"`
	Worker                   bool           `json:"worker,omitempty"`
	Leaf                     bool           `json:"leaf,omitempty"`
	RequestedModel           string         `json:"requestedModel,omitempty"`
	ResolvedModel            string         `json:"resolvedModel,omitempty"`
	RequestedReasoningEffort string         `json:"requestedReasoningEffort,omitempty"`
	ResolvedReasoningEffort  string         `json:"resolvedReasoningEffort,omitempty"`
	Lifecycle                AgentLifecycle `json:"lifecycle"`
	StartedAt                time.Time      `json:"startedAt"`
	UpdatedAt                time.Time      `json:"updatedAt"`
	Budget                   AgentUsage     `json:"budget,omitempty"`
	Usage                    AgentUsage     `json:"usage,omitempty"`
	// UsageAggregated marks coordinator usage that already includes its
	// descendants and must not be added to child totals.
	UsageAggregated bool     `json:"usageAggregated,omitempty"`
	Results         []Ref    `json:"results,omitempty"`
	DependsOn       []string `json:"dependsOn,omitempty"`
	Fidelity        string   `json:"fidelity,omitempty"`
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
			event.PeerMessage.Purpose == "" || event.PeerMessage.OccurredAt.IsZero() {
			return fmt.Errorf("journal: invalid peer-message metadata")
		}
		return nil
	default:
		return fmt.Errorf("journal: unsupported nested-agent event %q", event.Type)
	}
}

// ScrubAgentEvent applies the same byte-level policy used by the journal
// boundary and returns a typed event safe for in-memory projection.
func ScrubAgentEvent(scrubber Scrubber, event Event) (Event, error) {
	if scrubber == nil {
		scrubber = NewPatternScrubber()
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("journal: marshal nested-agent event for scrubbing: %w", err)
	}
	var scrubbed Event
	if err := json.Unmarshal(scrubber.Scrub(raw), &scrubbed); err != nil {
		return Event{}, fmt.Errorf("journal: decode scrubbed nested-agent event: %w", err)
	}
	return scrubbed, nil
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

// ActiveAgentTreeForStage reconstructs the active tree for one in-flight stage
// attempt. Events from other runs and stages are deliberately ignored.
func ActiveAgentTreeForStage(events []Event, runID, stage string, attempt int) (map[string]AgentProvenance, error) {
	return activeAgentTree(events, runID, stage, attempt)
}

func activeAgentTree(events []Event, runID, stage string, attempt int) (map[string]AgentProvenance, error) {
	scoped := make([]Event, 0, len(events))
	for _, event := range events {
		if event.Type != EventAgentLifecycle || event.Agent == nil ||
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

// ActiveAgentTree reads the current durable journal and reconstructs one
// in-flight stage attempt without waiting for the run to finish.
func (r *Reader) ActiveAgentTree(stage string, attempt int) (map[string]AgentProvenance, error) {
	identity, err := r.Identity()
	if err != nil {
		return nil, err
	}
	events, err := r.Events()
	if err != nil {
		return nil, err
	}
	return ActiveAgentTreeForStage(events, identity.RunID, stage, attempt)
}

// RollupAgentUsage sums each finalized invocation once. Coordinator usage is
// excluded only when the coordinator has children, because a coordinator-only
// invocation is itself the measured agent.
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

func rollupAgentUsage(events []Event, runID, stage string) AgentUsage {
	latestAttempt := 0
	for _, event := range events {
		if event.Type == EventAgentLifecycle && event.Agent != nil &&
			(runID == "" || event.Agent.RunID == runID) &&
			(stage == "" || event.Agent.Stage == stage) &&
			event.Agent.Attempt > latestAttempt {
			latestAttempt = event.Agent.Attempt
		}
	}
	latest := make(map[string]AgentProvenance)
	for _, event := range events {
		if event.Type != EventAgentLifecycle || event.Agent == nil ||
			event.Agent.ID == "" ||
			event.Agent.Attempt != latestAttempt ||
			(runID != "" && event.Agent.RunID != runID) ||
			(stage != "" && event.Agent.Stage != stage) {
			continue
		}
		current, ok := latest[event.Agent.ID]
		if !ok || newerAgentEvent(event.Agent, &current) {
			next := *event.Agent
			if ok && current.Attempt == event.Agent.Attempt {
				mergeAgentUsage(&next.Usage, current.Usage)
			}
			latest[event.Agent.ID] = next
		} else if event.Agent.Attempt == current.Attempt {
			mergeAgentUsage(&current.Usage, event.Agent.Usage)
			latest[event.Agent.ID] = current
		}
	}
	hasChildren := make(map[string]bool)
	for _, agent := range latest {
		parent, ok := latest[agent.ParentID]
		if agent.ParentID != "" && ok && parent.Attempt == agent.Attempt {
			hasChildren[agent.ParentID] = true
		}
	}
	// Exclude a coordinator only when an explicit aggregate marker or a
	// same-attempt child edge proves its usage overlaps descendant usage.
	for id, agent := range latest {
		if agent.Coordinator && (agent.UsageAggregated || hasChildren[id]) {
			delete(latest, id)
			continue
		}
		switch agent.Lifecycle {
		case AgentCompleted, AgentFailed, AgentCancelled:
		default:
			delete(latest, id)
		}
	}
	var result AgentUsage
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		addAgentUsage(&result, latest[key].Usage)
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
	if agent.StartedAt.IsZero() || agent.UpdatedAt.IsZero() || agent.UpdatedAt.Before(agent.StartedAt) {
		return fmt.Errorf("journal: invalid nested-agent timestamps for %q", agent.ID)
	}
	switch agent.Fidelity {
	case "", AgentFidelityFull, AgentFidelityPartial, AgentFidelityNone:
	default:
		return fmt.Errorf("journal: invalid nested-agent fidelity %q", agent.Fidelity)
	}
	if err := validateAgentUsage(agent.Budget); err != nil {
		return fmt.Errorf("journal: invalid nested-agent budget for %q: %w", agent.ID, err)
	}
	if err := validateAgentUsage(agent.Usage); err != nil {
		return fmt.Errorf("journal: invalid nested-agent usage for %q: %w", agent.ID, err)
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
	return candidate.UpdatedAt.Equal(current.UpdatedAt)
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

func addAgentUsage(dst *AgentUsage, src AgentUsage) {
	if src.InputTokens != nil {
		if dst.InputTokens == nil {
			dst.InputTokens = new(int64)
		}
		*dst.InputTokens += *src.InputTokens
	}
	if src.OutputTokens != nil {
		if dst.OutputTokens == nil {
			dst.OutputTokens = new(int64)
		}
		*dst.OutputTokens += *src.OutputTokens
	}
	if src.CostUSD != nil {
		if dst.CostUSD == nil {
			dst.CostUSD = new(float64)
		}
		*dst.CostUSD += *src.CostUSD
	}
}

func validateAgentUsage(usage AgentUsage) error {
	if usage.InputTokens != nil && *usage.InputTokens < 0 {
		return fmt.Errorf("negative input tokens")
	}
	if usage.OutputTokens != nil && *usage.OutputTokens < 0 {
		return fmt.Errorf("negative output tokens")
	}
	if usage.CostUSD != nil && *usage.CostUSD < 0 {
		return fmt.Errorf("negative cost")
	}
	return nil
}
