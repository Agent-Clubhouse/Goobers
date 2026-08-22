package harness

import (
	"bufio"
	"bytes"
	"encoding/json"

	"github.com/goobers/goobers/internal/journal"
)

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
