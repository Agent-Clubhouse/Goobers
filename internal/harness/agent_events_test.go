package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestAdapterAgentEventsProjectLifecycleAndUsage(t *testing.T) {
	req := RunRequest{Attempt: 3, Envelope: testEnvelope(t.TempDir(), "agent:model")}
	events := adapterAgentEvents(req, "copilot", journal.AgentCompleted, map[string]float64{
		"gen_ai.usage.input_tokens": 12,
	})
	if len(events) != 1 || events[0].Agent == nil {
		t.Fatalf("events = %#v", events)
	}
	agent := events[0].Agent
	if agent.Schema == "" || agent.Attempt != 3 || agent.Plugin != "copilot" ||
		agent.Lifecycle != journal.AgentCompleted || agent.Usage.InputTokens == nil ||
		*agent.Usage.InputTokens != 12 {
		t.Fatalf("agent = %#v", agent)
	}
	if err := journal.ValidateAgentEvent(events[0]); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterAgentEmitterDistinguishesRequestedAndResolvedSettings(t *testing.T) {
	req := RunRequest{Envelope: testEnvelope(t.TempDir())}
	emitter, err := beginAdapterAgentTelemetry(req, "copilot", "requested-model", "resolved-model", "high", "medium")
	if err != nil {
		t.Fatal(err)
	}
	var out Outcome
	var runErr error
	emitter.finish(&out, &runErr)
	if runErr != nil {
		t.Fatal(runErr)
	}
	for _, event := range out.AgentEvents {
		if event.Agent == nil ||
			event.Agent.RequestedModel != "requested-model" ||
			event.Agent.ResolvedModel != "resolved-model" ||
			event.Agent.RequestedReasoningEffort != "high" ||
			event.Agent.ResolvedReasoningEffort != "medium" {
			t.Fatalf("settings were not preserved distinctly: %#v", event.Agent)
		}
	}
}

func TestProjectAgentEventsAcceptsOnlyNormalizedRecords(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	payload := `{"type":"assistant.message","content":"not provenance"}
{"type":"agent.lifecycle","agent":{"id":"worker","runId":"spoofed","stage":"spoofed","attempt":99,"lifecycle":"completed","startedAt":"` + now + `","updatedAt":"` + now + `","requestedModel":"requested","resolvedModel":"resolved","requestedReasoningEffort":"high","resolvedReasoningEffort":"medium"}}
{"type":"agent.lifecycle","agent":{"id":"invalid","lifecycle":"completed"}}`
	events := projectAgentEvents([]byte(payload), RunRequest{
		Attempt:  2,
		Envelope: apiv1.InvocationEnvelope{RunID: "run-1", TaskID: "stage-1", Attempt: 2},
	})
	if len(events) != 1 || events[0].Agent == nil {
		t.Fatalf("events = %#v, want one lifecycle event", events)
	}
	agent := events[0].Agent
	if agent.RunID != "run-1" || agent.Stage != "stage-1" || agent.Attempt != 2 ||
		agent.Schema != "goobers.dev/journal/agent/v1" ||
		agent.RequestedModel != "requested" || agent.ResolvedModel != "resolved" ||
		agent.RequestedReasoningEffort != "high" || agent.ResolvedReasoningEffort != "medium" {
		t.Fatalf("agent = %#v, want invocation defaults", agent)
	}
	if got := agentEventsFidelity(events); got != journal.AgentFidelityFull {
		t.Fatalf("fidelity = %q, want full", got)
	}
}

func TestProjectAgentEventsDropsRawMessageContent(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	events := projectAgentEvents([]byte(`{"type":"agent.message","peerMessage":{"id":"m1","senderId":"a","recipientId":"b","occurredAt":"`+
		now+`","purpose":"dependency","content":"must-not-survive"},"content":"also-drop"}`), RunRequest{})
	if len(events) != 1 || events[0].PeerMessage == nil {
		t.Fatalf("events = %#v, want message metadata", events)
	}
	raw, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "must-not-survive") || strings.Contains(string(raw), "also-drop") {
		t.Fatalf("raw peer content survived normalization: %s", raw)
	}
	normalized, ok := normalizedAgentRecord([]byte(`{"type":"agent.message","peerMessage":{"id":"m1","senderId":"a","recipientId":"b","occurredAt":"` +
		now + `","purpose":"dependency","content":"must-not-survive"},"content":"also-drop"}`))
	if !ok || strings.Contains(string(normalized), "must-not-survive") || strings.Contains(string(normalized), "also-drop") {
		t.Fatalf("raw peer content survived transcript normalization: %s", normalized)
	}
}

func assertAdapterLifecycle(t *testing.T, out Outcome, plugin string) {
	t.Helper()
	if len(out.AgentEvents) < 2 {
		t.Fatalf("%s agent events = %#v, want started and terminal lifecycle", plugin, out.AgentEvents)
	}
	started := out.AgentEvents[0].Agent
	finished := out.AgentEvents[len(out.AgentEvents)-1].Agent
	if started == nil || finished == nil || started.Lifecycle != journal.AgentStarted ||
		finished.Lifecycle != journal.AgentCompleted || !started.StartedAt.Equal(finished.StartedAt) {
		t.Fatalf("%s lifecycle = %#v", plugin, out.AgentEvents)
	}
	if out.AgentTelemetryFidelity != journal.AgentFidelityPartial || out.AgentTelemetryDetail == "" {
		t.Fatalf("%s fidelity = %q detail %q", plugin, out.AgentTelemetryFidelity, out.AgentTelemetryDetail)
	}
}
