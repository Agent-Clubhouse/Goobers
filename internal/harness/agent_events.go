package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

const (
	agentSchema                 = "goobers.dev/journal/agent/v1"
	partialAgentTelemetryDetail = "adapter lifecycle and normalized records are projected, but complete child coverage is not guaranteed"
)

type adapterAgentEmitter struct {
	req             RunRequest
	plugin          string
	startedAt       time.Time
	requestedModel  string
	resolvedModel   string
	requestedEffort string
	resolvedEffort  string
	events          []journal.Event
	hasNestedAgents bool
}

func beginAdapterAgentTelemetry(req RunRequest, plugin, requestedModel, resolvedModel, requestedEffort, resolvedEffort string) (*adapterAgentEmitter, error) {
	emitter := &adapterAgentEmitter{
		req:             req,
		plugin:          plugin,
		startedAt:       time.Now().UTC(),
		requestedModel:  requestedModel,
		resolvedModel:   resolvedModel,
		requestedEffort: requestedEffort,
		resolvedEffort:  resolvedEffort,
	}
	event := emitter.lifecycleEvent(journal.AgentStarted, nil, resolvedModel, resolvedEffort)
	if err := emitter.emit(event); err != nil {
		return nil, err
	}
	return emitter, nil
}

func (e *adapterAgentEmitter) emit(events ...journal.Event) error {
	for _, event := range events {
		if event.Type == journal.EventAgentLifecycle && event.Agent != nil &&
			event.Agent.ID != adapterAgentID(e.req, e.plugin) {
			e.hasNestedAgents = true
		}
		e.events = append(e.events, event)
		if e.req.AgentEventSink != nil {
			if err := e.req.AgentEventSink(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *adapterAgentEmitter) finish(out *Outcome, runErr *error) {
	lifecycle := journal.AgentCompleted
	if *runErr != nil {
		lifecycle = journal.AgentFailed
	}
	metrics := out.Metrics
	if e.hasNestedAgents {
		// The adapter's process metrics are a stage aggregate. Structured
		// children carry their own usage, so do not duplicate that aggregate on
		// the synthetic adapter node.
		metrics = nil
	}
	resolvedModel := resolvedAgentModel(*out)
	if resolvedModel == "" {
		resolvedModel = e.resolvedModel
	}
	event := e.lifecycleEvent(lifecycle, metrics, resolvedModel, e.resolvedEffort)
	if event.Agent != nil && !e.hasNestedAgents {
		event.Agent.Usage = agentUsage(metrics, out.ModelUsage...)
		if event.Agent.Usage.Model == "" {
			event.Agent.Usage.Model = resolvedModel
		}
	}
	if event.Agent != nil && e.hasNestedAgents {
		event.Agent.Coordinator = true
		event.Agent.Worker = false
	}
	if err := e.emit(event); err != nil {
		*runErr = errors.Join(*runErr, err)
	}
	out.AgentEvents = append(out.AgentEvents, e.events...)
	out.AgentTelemetryFidelity = journal.AgentFidelityPartial
	out.AgentTelemetryDetail = partialAgentTelemetryDetail
}

func (e *adapterAgentEmitter) lifecycleEvent(lifecycle journal.AgentLifecycle, metrics map[string]float64, resolvedModel, resolvedEffort string) journal.Event {
	event := adapterAgentEventAt(e.req, e.plugin, lifecycle, metrics, e.startedAt)
	event.Agent.RequestedModel = e.requestedModel
	event.Agent.ResolvedModel = resolvedModel
	event.Agent.RequestedReasoningEffort = e.requestedEffort
	event.Agent.ResolvedReasoningEffort = resolvedEffort
	return event
}

func adapterAgentEventAt(req RunRequest, plugin string, lifecycle journal.AgentLifecycle, metrics map[string]float64, startedAt time.Time) journal.Event {
	attempt := req.Attempt
	if attempt < 1 {
		attempt = int(req.Envelope.Attempt)
	}
	if attempt < 1 {
		attempt = 1
	}
	now := time.Now().UTC()
	agent := &journal.AgentProvenance{
		Schema: agentSchema, ID: adapterAgentID(req, plugin),
		RunID: req.Envelope.RunID, Stage: req.Envelope.TaskID, Attempt: attempt,
		Plugin: plugin, Objective: req.Envelope.Goal, Worker: true,
		RequestedModel: req.Model,
		Lifecycle:      lifecycle, StartedAt: startedAt, UpdatedAt: now,
		Fidelity: journal.AgentFidelityPartial,
	}
	if lifecycle == journal.AgentCompleted || lifecycle == journal.AgentFailed || lifecycle == journal.AgentCancelled {
		agent.Usage = agentUsage(metrics)
	}
	return journal.Event{Type: journal.EventAgentLifecycle, Agent: agent}
}

func adapterAgentID(req RunRequest, plugin string) string {
	return plugin + ":" + req.Envelope.TaskID
}

func agentUsage(metrics map[string]float64, models ...telemetry.ModelUsage) journal.AgentUsage {
	var usage journal.AgentUsage
	if value, ok := metrics[telemetry.AttrGenAIUsageInputTokens]; ok {
		v := int64(value)
		usage.InputTokens = &v
	}
	if value, ok := metrics[telemetry.AttrGenAIUsageOutputTokens]; ok {
		v := int64(value)
		usage.OutputTokens = &v
	}
	if value, ok := metrics[telemetry.AttrUsageCacheReadTokens]; ok {
		v := int64(value)
		usage.CacheReadTokens = &v
	}
	if value, ok := metrics[telemetry.AttrUsageCacheWriteTokens]; ok {
		v := int64(value)
		usage.CacheWriteTokens = &v
	}
	if value, ok := metrics[telemetry.AttrUsageReasoningTokens]; ok {
		v := int64(value)
		usage.ReasoningTokens = &v
	}
	if value, ok := metrics[telemetry.AttrUsageNanoAIU]; ok {
		v := int64(value)
		usage.NanoAIU = &v
		cost := telemetry.NanoAIUToUSD(v)
		usage.CostUSD = &cost
	} else if value, ok := metrics[telemetry.AttrUsageCostUSD]; ok {
		usage.CostUSD = &value
	}
	if len(models) == 1 {
		usage.Model = models[0].Model
	}
	var exactCacheRead, exactCacheWrite, exactReasoning, exactNanoAIU int64
	var hasExactCacheRead, hasExactCacheWrite, hasExactReasoning, hasExactNanoAIU bool
	for _, model := range models {
		if model.CacheReadTokens != nil {
			exactCacheRead += *model.CacheReadTokens
			hasExactCacheRead = true
		}
		if model.CacheWriteTokens != nil {
			exactCacheWrite += *model.CacheWriteTokens
			hasExactCacheWrite = true
		}
		if model.ReasoningTokens != nil {
			exactReasoning += *model.ReasoningTokens
			hasExactReasoning = true
		}
		if model.NanoAIU != nil {
			exactNanoAIU += *model.NanoAIU
			hasExactNanoAIU = true
		}
	}
	if hasExactCacheRead {
		usage.CacheReadTokens = &exactCacheRead
	}
	if hasExactCacheWrite {
		usage.CacheWriteTokens = &exactCacheWrite
	}
	if hasExactReasoning {
		usage.ReasoningTokens = &exactReasoning
	}
	if hasExactNanoAIU {
		aggregate, hasAggregate := metrics[telemetry.AttrUsageNanoAIU]
		if !hasAggregate || float64(exactNanoAIU) == aggregate {
			usage.NanoAIU = &exactNanoAIU
			cost := telemetry.NanoAIUToUSD(exactNanoAIU)
			usage.CostUSD = &cost
		}
	}
	return usage
}

// transcriptRootAgentID returns the id of the stage's single root nested agent
// — the one whose session the recorded transcript span is. It returns "" when
// the projected events name no unique root, so the span's provenance stays
// explicitly unknown instead of being attributed to a guessed agent.
func transcriptRootAgentID(events []journal.Event, stage string) string {
	var root string
	for _, event := range events {
		if event.Type != journal.EventAgentLifecycle || event.Agent == nil {
			continue
		}
		agent := event.Agent
		if agent.ID == "" || agent.ParentID != "" || agent.Stage != stage {
			continue
		}
		if root != "" && root != agent.ID {
			return ""
		}
		root = agent.ID
	}
	return root
}

func resolvedAgentModel(out Outcome) string {
	models := make(map[string]struct{})
	for _, usage := range out.ModelUsage {
		if usage.Model != "" {
			models[usage.Model] = struct{}{}
		}
	}
	if len(models) != 1 {
		return ""
	}
	names := make([]string, 0, 1)
	for model := range models {
		names = append(names, model)
	}
	sort.Strings(names)
	return names[0]
}

func requestedHarnessOption(req RunRequest, name string) string {
	value, ok := req.HarnessOptions[name]
	if !ok {
		return ""
	}
	var result string
	if json.Unmarshal(value.Raw, &result) != nil {
		return ""
	}
	return result
}

// projectAgentEvents accepts only records that already identify themselves as
// the normalized contract. Transcript prose and provider-specific messages are
// intentionally ignored. Invocation scope is always overwritten from the
// trusted request so an adapter payload cannot inject another run or attempt.
func projectAgentEvents(data []byte, req RunRequest) []journal.Event {
	var events []journal.Event
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var event journal.Event
		if json.Unmarshal(line, &event) != nil ||
			(event.Type != journal.EventAgentLifecycle && event.Type != journal.EventAgentMessage) {
			continue
		}
		if event.Type == journal.EventAgentLifecycle && event.Agent != nil {
			event.Agent.RunID = req.Envelope.RunID
			event.Agent.Stage = req.Envelope.TaskID
			event.Agent.Attempt = req.Attempt
			if event.Agent.Attempt < 1 {
				event.Agent.Attempt = int(req.Envelope.Attempt)
			}
			if event.Agent.Attempt < 1 {
				event.Agent.Attempt = 1
			}
			event.Agent.Schema = agentSchema
			if event.Agent.Fidelity == "" {
				event.Agent.Fidelity = journal.AgentFidelityFull
			}
		}
		if journal.ValidateAgentEvent(event) != nil {
			continue
		}
		events = append(events, event)
	}
	return events
}

func normalizedAgentRecord(data []byte) ([]byte, bool) {
	var event struct {
		Type        journal.EventType            `json:"type"`
		Agent       *journal.AgentProvenance     `json:"agent,omitempty"`
		PeerMessage *journal.PeerMessageMetadata `json:"peerMessage,omitempty"`
	}
	if json.Unmarshal(data, &event) != nil {
		return nil, false
	}
	if event.Type != journal.EventAgentLifecycle && event.Type != journal.EventAgentMessage {
		return nil, false
	}
	normalized, err := json.Marshal(event)
	return normalized, err == nil
}
