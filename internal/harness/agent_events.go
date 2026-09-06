package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"sync"
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
	// mu guards everything below. The liveness sampler (#4179) emits from its
	// own goroutine while the adapter's main path is still running, so the
	// event slice and the nested-agent flag are shared state now, not
	// single-threaded bookkeeping.
	mu              sync.Mutex
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
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.emitLocked(events...)
}

func (e *adapterAgentEmitter) emitLocked(events ...journal.Event) error {
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
	e.mu.Lock()
	defer e.mu.Unlock()
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
	if event.Agent != nil && e.hasNestedAgents {
		event.Agent.Coordinator = true
		event.Agent.Worker = false
	}
	if err := e.emitLocked(event); err != nil {
		*runErr = errors.Join(*runErr, err)
	}
	out.AgentEvents = append(out.AgentEvents, e.events...)
	out.AgentTelemetryFidelity = journal.AgentFidelityPartial
	out.AgentTelemetryDetail = partialAgentTelemetryDetail
}

// activityObserver turns subprocess output samples into periodic agent
// lifecycle marks, so a stalled agentic stage is legible from `goobers trace`
// without opening the transcript artifact (#4179).
//
// The mapping is the whole idea, and it is deliberately the smallest one that
// answers the question the journal could not:
//
//	output arrived this interval → resumed  ("working")
//	no output this interval      → waiting  ("blocked on something")
//
// Both already exist in journal.AgentLifecycle's taxonomy; nothing is added to
// AgentProvenance, so no schema moves and no drift guard is involved. The
// evidence an operator needs is the SEQUENCE: a run of `waiting` marks with
// their timestamps says "silent since T" at a glance, which is exactly what
// separated a looping agent from the blocked `go mod download` in
// f5faeec4ee947f88af7a09204db51bcb — and what nothing in that run's journal
// said.
//
// Emission failures are dropped on purpose. This is observational telemetry
// about a session that is still running; failing the stage because a liveness
// mark could not be written would turn a diagnostic into an outage.
func (e *adapterAgentEmitter) activityObserver() ActivityObserver {
	if e == nil {
		return nil
	}
	return func(sample ActivitySample) {
		lifecycle := journal.AgentWaiting
		if sample.Moved {
			lifecycle = journal.AgentResumed
		}
		_ = e.emit(e.lifecycleEvent(lifecycle, nil, e.resolvedModel, e.resolvedEffort))
	}
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

func agentUsage(metrics map[string]float64) journal.AgentUsage {
	var usage journal.AgentUsage
	if value, ok := metrics[telemetry.AttrGenAIUsageInputTokens]; ok {
		v := int64(value)
		usage.InputTokens = &v
	}
	if value, ok := metrics[telemetry.AttrGenAIUsageOutputTokens]; ok {
		v := int64(value)
		usage.OutputTokens = &v
	}
	if value, ok := metrics[telemetry.AttrUsageCostUSD]; ok {
		usage.CostUSD = &value
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
