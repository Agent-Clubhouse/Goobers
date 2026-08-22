package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

const agentSchema = "goobers.dev/journal/agent/v1"

func adapterAgentEvents(req RunRequest, plugin string, lifecycle journal.AgentLifecycle, metrics map[string]float64) []journal.Event {
	attempt := req.Attempt
	if attempt < 1 {
		attempt = 1
	}
	now := time.Now().UTC()
	agent := &journal.AgentProvenance{
		Schema: agentSchema, ID: plugin + ":" + req.Envelope.TaskID,
		RunID: req.Envelope.RunID, Stage: req.Envelope.TaskID, Attempt: attempt,
		Plugin: plugin, Objective: req.Envelope.Goal, Worker: true,
		RequestedModel: req.Model, ResolvedModel: req.Model,
		Lifecycle: lifecycle, StartedAt: now, UpdatedAt: now,
		Fidelity: journal.AgentFidelityPartial,
	}
	if lifecycle == journal.AgentCompleted || lifecycle == journal.AgentFailed || lifecycle == journal.AgentCancelled {
		agent.Usage = agentUsage(metrics)
	}
	return []journal.Event{{Type: journal.EventAgentLifecycle, Agent: agent}}
}

func agentUsage(metrics map[string]float64) journal.AgentUsage {
	var usage journal.AgentUsage
	if value, ok := metrics["gen_ai.usage.input_tokens"]; ok {
		v := int64(value)
		usage.InputTokens = &v
	}
	if value, ok := metrics["gen_ai.usage.output_tokens"]; ok {
		v := int64(value)
		usage.OutputTokens = &v
	}
	if value, ok := metrics["gen_ai.usage.cost_usd"]; ok {
		usage.CostUSD = &value
	}
	return usage
}

// projectAgentEvents accepts only records that already identify themselves as
// the normalized contract. Transcript prose and provider-specific messages are
// intentionally ignored.
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
			event.Agent.RunID = defaultString(event.Agent.RunID, req.Envelope.RunID)
			event.Agent.Stage = defaultString(event.Agent.Stage, req.Envelope.TaskID)
			if event.Agent.Attempt < 1 {
				event.Agent.Attempt = 1
			}
			if event.Agent.Schema == "" {
				event.Agent.Schema = "goobers.dev/journal/agent/v1"
			}
		}
		events = append(events, event)
	}
	return events
}

func isNormalizedAgentRecord(data []byte) bool {
	var event struct {
		Type journal.EventType `json:"type"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	return event.Type == journal.EventAgentLifecycle || event.Type == journal.EventAgentMessage
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func agentEventsFidelity(events []journal.Event) string {
	if len(events) == 0 {
		return journal.AgentFidelityNone
	}
	return journal.AgentFidelityFull
}
