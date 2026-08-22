package harness

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestProjectAgentEventsAcceptsOnlyNormalizedRecords(t *testing.T) {
	events := projectAgentEvents([]byte(`{"type":"assistant.message","content":"not provenance"}
{"type":"agent.lifecycle","agent":{"id":"worker","lifecycle":"completed"}}`), RunRequest{
		Envelope: apiv1.InvocationEnvelope{RunID: "run-1", TaskID: "stage-1"},
	})
	if len(events) != 1 || events[0].Agent == nil {
		t.Fatalf("events = %#v, want one lifecycle event", events)
	}
	agent := events[0].Agent
	if agent.RunID != "run-1" || agent.Stage != "stage-1" || agent.Attempt != 1 ||
		agent.Schema != "goobers.dev/journal/agent/v1" {
		t.Fatalf("agent = %#v, want invocation defaults", agent)
	}
	if got := agentEventsFidelity(events); got != journal.AgentFidelityFull {
		t.Fatalf("fidelity = %q, want full", got)
	}
}
