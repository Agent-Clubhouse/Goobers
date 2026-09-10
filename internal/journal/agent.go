package journal

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
	Model            string   `json:"model,omitempty"`
	InputTokens      *int64   `json:"inputTokens,omitempty"`
	OutputTokens     *int64   `json:"outputTokens,omitempty"`
	CacheReadTokens  *int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64   `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  *int64   `json:"reasoningTokens,omitempty"`
	NanoAIU          *int64   `json:"nanoAiu,omitempty"`
	CostUSD          *float64 `json:"costUsd,omitempty"`
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

// AgentProgressKind names the normalized operator-readable status record. These
// are intentionally bounded, explicit, and one-way: we record the current
// intent or observed fact, never hidden private reasoning.
type AgentProgressKind string

const (
	AgentProgressPlan       AgentProgressKind = "plan"
	AgentProgressProgress   AgentProgressKind = "progress"
	AgentProgressDecision   AgentProgressKind = "decision"
	AgentProgressBlocker    AgentProgressKind = "blocker"
	AgentProgressQuestion   AgentProgressKind = "question"
	AgentProgressNextAction AgentProgressKind = "next_action"
	AgentProgressSummary    AgentProgressKind = "summary"
)

// AgentProgressSource classifies how an operator-readable status was emitted.
type AgentProgressSource string

const (
	AgentProgressSourceNative   AgentProgressSource = "native"
	AgentProgressSourceModel    AgentProgressSource = "model"
	AgentProgressSourceEvidence AgentProgressSource = "evidence"
)

// AgentProgress is a bounded, resumable, operator-readable status update for a
// single agent invocation. It intentionally omits hidden chain-of-thought and
// only describes observable facts or explicit summaries the model chose to emit.
type AgentProgress struct {
	Schema     string                  `json:"schema"`
	AgentID    string                  `json:"agentId"`
	RunID      string                  `json:"runId"`
	Stage      string                  `json:"stage"`
	Attempt    int                     `json:"attempt"`
	Sequence   uint64                  `json:"sequence"`
	Kind       AgentProgressKind       `json:"kind"`
	Source     AgentProgressSource     `json:"source,omitempty"`
	OccurredAt time.Time               `json:"occurredAt"`
	UpdatedAt  time.Time               `json:"updatedAt,omitempty"`
	Fidelity   string                  `json:"fidelity,omitempty"`
	Summary    string                  `json:"summary,omitempty"`
	Plan       []string                `json:"plan,omitempty"`
	Progress   []string                `json:"progress,omitempty"`
	Decision   string                  `json:"decision,omitempty"`
	Blocker    string                  `json:"blocker,omitempty"`
	Question   string                  `json:"question,omitempty"`
	NextAction string                  `json:"nextAction,omitempty"`
	Evidence   []AgentProgressEvidence `json:"evidence,omitempty"`
}

// AgentProgressEvidence attaches a named observable fact or artifact to a status
// update without persisting a raw private chain-of-thought transcript.
type AgentProgressEvidence struct {
	Type  string `json:"type,omitempty"`
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
	Ref   *Ref   `json:"ref,omitempty"`
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
	case EventAgentProgress:
		if event.Progress == nil {
			return fmt.Errorf("journal: agent progress event has no progress payload")
		}
		return validateAgentProgress(*event.Progress)
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

func validateAgentProgress(progress AgentProgress) error {
	if progress.Schema != "goobers.dev/journal/agent-progress/v1" || progress.AgentID == "" ||
		progress.RunID == "" || progress.Stage == "" || progress.Attempt < 1 {
		return fmt.Errorf("journal: invalid nested-agent progress identity %q", progress.AgentID)
	}
	if progress.OccurredAt.IsZero() {
		return fmt.Errorf("journal: invalid nested-agent progress timestamp for %q", progress.AgentID)
	}
	if err := validateAgentProgressSize(progress); err != nil {
		return err
	}
	if err := validateAgentProgressEnums(progress); err != nil {
		return err
	}
	return validateAgentProgressText(progress)
}

func validateAgentProgressSize(progress AgentProgress) error {
	if len(progress.Summary) > 4096 || len(progress.Decision) > 2048 || len(progress.Blocker) > 2048 ||
		len(progress.Question) > 2048 || len(progress.NextAction) > 2048 || len(progress.Plan) > 100 ||
		len(progress.Progress) > 100 || len(progress.Evidence) > 100 {
		return fmt.Errorf("journal: nested-agent progress payload exceeds size limits for %q", progress.AgentID)
	}
	return nil
}

func validateAgentProgressEnums(progress AgentProgress) error {
	switch progress.Kind {
	case AgentProgressPlan, AgentProgressProgress, AgentProgressDecision, AgentProgressBlocker,
		AgentProgressQuestion, AgentProgressNextAction, AgentProgressSummary:
	default:
		return fmt.Errorf("journal: invalid nested-agent progress kind %q", progress.Kind)
	}
	switch progress.Source {
	case "", AgentProgressSourceNative, AgentProgressSourceModel, AgentProgressSourceEvidence:
	default:
		return fmt.Errorf("journal: invalid nested-agent progress source %q", progress.Source)
	}
	switch progress.Fidelity {
	case "", AgentFidelityFull, AgentFidelityPartial, AgentFidelityNone:
	default:
		return fmt.Errorf("journal: invalid nested-agent progress fidelity %q", progress.Fidelity)
	}
	return nil
}

func validateAgentProgressText(progress AgentProgress) error {
	if containsPrivateReasoning(progress.Summary) {
		return fmt.Errorf("journal: nested-agent progress summary must not contain hidden reasoning")
	}
	if containsPrivateReasoning(progress.Decision) || containsPrivateReasoning(progress.Blocker) ||
		containsPrivateReasoning(progress.Question) || containsPrivateReasoning(progress.NextAction) {
		return fmt.Errorf("journal: nested-agent progress field must not contain hidden reasoning")
	}
	if hasPrivateReasoning(progress.Plan) {
		return fmt.Errorf("journal: nested-agent progress plan must not contain hidden reasoning")
	}
	if hasPrivateReasoning(progress.Progress) {
		return fmt.Errorf("journal: nested-agent progress record must not contain hidden reasoning")
	}
	if hasPrivateReasoningEvidence(progress.Evidence) {
		return fmt.Errorf("journal: nested-agent progress evidence must not contain hidden reasoning")
	}
	return nil
}

func hasPrivateReasoning(values []string) bool {
	for _, value := range values {
		if containsPrivateReasoning(value) {
			return true
		}
	}
	return false
}

func hasPrivateReasoningEvidence(evidence []AgentProgressEvidence) bool {
	for _, ref := range evidence {
		if containsPrivateReasoning(ref.Type) || containsPrivateReasoning(ref.ID) || containsPrivateReasoning(ref.Label) {
			return true
		}
	}
	return false
}

func containsPrivateReasoning(v string) bool {
	l := strings.ToLower(v)
	return strings.Contains(l, "chain-of-thought") || strings.Contains(l, "chain of thought") ||
		strings.Contains(l, "private reasoning") || strings.Contains(l, "hidden reasoning") ||
		strings.Contains(l, "scratchpad") || strings.Contains(l, "inner monologue")
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
	for _, event := range latestPodAgentEvents(events) {
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
	for _, event := range latestPodAgentEvents(events) {
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

// RollupRunAgentUsage sums finalized usage across every agentic stage in one
// run. RollupAgentUsage intentionally follows the first observed stage for
// callers rendering one invocation tree; provider cost receipts need the wider
// workflow total, including implementation, review, and remediation stages.
//
// Stages are folded independently so a repass in one stage does not discard a
// different stage whose latest attempt number is lower.
func RollupRunAgentUsage(events []Event, runID string) AgentUsage {
	stages := make(map[string]struct{})
	for _, event := range events {
		if event.Type != EventAgentLifecycle || event.Agent == nil ||
			event.Agent.Stage == "" || (runID != "" && event.Agent.RunID != runID) {
			continue
		}
		stages[event.Agent.Stage] = struct{}{}
	}
	names := make([]string, 0, len(stages))
	for stage := range stages {
		names = append(names, stage)
	}
	sort.Strings(names)

	var result AgentUsage
	for _, stage := range names {
		addAgentUsage(&result, rollupAgentUsage(events, runID, stage))
	}
	return result
}

func rollupAgentUsage(events []Event, runID, stage string) AgentUsage {
	events = latestPodAgentEvents(events)
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
	if dst.Model == "" {
		dst.Model = src.Model
	}
	if dst.InputTokens == nil {
		dst.InputTokens = src.InputTokens
	}
	if dst.OutputTokens == nil {
		dst.OutputTokens = src.OutputTokens
	}
	if dst.CacheReadTokens == nil {
		dst.CacheReadTokens = src.CacheReadTokens
	}
	if dst.CacheWriteTokens == nil {
		dst.CacheWriteTokens = src.CacheWriteTokens
	}
	if dst.ReasoningTokens == nil {
		dst.ReasoningTokens = src.ReasoningTokens
	}
	if dst.NanoAIU == nil {
		dst.NanoAIU = src.NanoAIU
	}
	if dst.CostUSD == nil {
		dst.CostUSD = src.CostUSD
	}
}

func addAgentUsage(dst *AgentUsage, src AgentUsage) {
	if dst.Model == "" {
		dst.Model = src.Model
	}
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
	addAgentUsageInt64(&dst.CacheReadTokens, src.CacheReadTokens)
	addAgentUsageInt64(&dst.CacheWriteTokens, src.CacheWriteTokens)
	addAgentUsageInt64(&dst.ReasoningTokens, src.ReasoningTokens)
	addAgentUsageInt64(&dst.NanoAIU, src.NanoAIU)
	addAgentUsageFloat64(&dst.CostUSD, src.CostUSD)
}

func addAgentUsageInt64(dst **int64, src *int64) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = new(int64)
	}
	**dst += *src
}

func addAgentUsageFloat64(dst **float64, src *float64) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = new(float64)
	}
	**dst += *src
}

func validateAgentUsage(usage AgentUsage) error {
	if usage.InputTokens != nil && *usage.InputTokens < 0 {
		return fmt.Errorf("negative input tokens")
	}
	if usage.OutputTokens != nil && *usage.OutputTokens < 0 {
		return fmt.Errorf("negative output tokens")
	}
	if usage.CacheReadTokens != nil && *usage.CacheReadTokens < 0 {
		return fmt.Errorf("negative cache-read tokens")
	}
	if usage.CacheWriteTokens != nil && *usage.CacheWriteTokens < 0 {
		return fmt.Errorf("negative cache-write tokens")
	}
	if usage.ReasoningTokens != nil && *usage.ReasoningTokens < 0 {
		return fmt.Errorf("negative reasoning tokens")
	}
	if usage.NanoAIU != nil && *usage.NanoAIU < 0 {
		return fmt.Errorf("negative nano-AIU")
	}
	if usage.CostUSD != nil && *usage.CostUSD < 0 {
		return fmt.Errorf("negative cost")
	}
	return nil
}
