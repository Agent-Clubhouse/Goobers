package harness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
)

type transcriptAgentAdapter struct {
	FakeAdapter
	agentIDs []string
}

func (a *transcriptAgentAdapter) Run(_ context.Context, req RunRequest) (Outcome, error) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var events []journal.Event
	for _, id := range a.agentIDs {
		events = append(events, journal.Event{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: id, RunID: req.Envelope.RunID,
			Stage: req.Envelope.TaskID, Attempt: 1, Worker: true, Lifecycle: journal.AgentCompleted,
			StartedAt: now, UpdatedAt: now, Fidelity: journal.AgentFidelityFull,
		}})
	}
	payload, _ := json.Marshal(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	return Outcome{
		Payload: payload, Transcript: []byte("agent session"),
		AgentEvents: events, AgentTelemetryFidelity: journal.AgentFidelityFull,
	}, nil
}

func runTranscriptAgentStage(t *testing.T, agentIDs ...string) *fakeRecorder {
	t.Helper()
	rec := &fakeRecorder{}
	exec, err := NewExecutor(
		&transcriptAgentAdapter{agentIDs: agentIDs},
		testInjector(t, "", "", noopRegistrar{}),
		rec, rec, rec,
		journal.NewPatternScrubber(),
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir())); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return rec
}

func spanProvenanceAnnotations(rec *fakeRecorder) []journal.Event {
	var found []journal.Event
	for _, event := range rec.events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner[creditgraph.SpanProvenanceKeyKind] == creditgraph.SpanProvenanceAnnotation {
			found = append(found, event)
		}
	}
	return found
}

// TestExecutorJournalsSpanProvenance pins the additive credit-graph link: a
// recorded transcript span names the subagent whose session it is, so the
// graph attaches its model and tool calls to a recorded agent.
func TestExecutorJournalsSpanProvenance(t *testing.T) {
	rec := runTranscriptAgentStage(t, "root")
	annotations := spanProvenanceAnnotations(rec)
	if len(annotations) != 1 {
		t.Fatalf("annotations = %+v, want exactly one span-provenance link", annotations)
	}
	annotation := annotations[0]
	if annotation.Stage != "implement" {
		t.Fatalf("annotation stage = %q, want %q", annotation.Stage, "implement")
	}
	if annotation.Runner[creditgraph.SpanProvenanceKeyAgentID] != "root" {
		t.Fatalf("annotation agent = %#v, want the recorded root agent", annotation.Runner)
	}
	digest, _ := annotation.Runner[creditgraph.SpanProvenanceKeyDigest].(string)
	if len(rec.spans) == 0 || digest != journal.Digest(rec.spans[len(rec.spans)-1].data) {
		t.Fatalf("annotation digest = %q, want the recorded span's digest", digest)
	}
}

// TestExecutorLeavesAmbiguousSpanProvenanceUnknown pins that an ambiguous
// owner is left unlinked rather than attributed to a guessed agent.
func TestExecutorLeavesAmbiguousSpanProvenanceUnknown(t *testing.T) {
	rec := runTranscriptAgentStage(t, "root-a", "root-b")
	if annotations := spanProvenanceAnnotations(rec); len(annotations) != 0 {
		t.Fatalf("annotations = %+v, want none when the span's owner is ambiguous", annotations)
	}
}

func TestTranscriptRootAgentIDIgnoresOtherStagesAndChildren(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	agent := func(id, parent, stage string) journal.Event {
		return journal.Event{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			ID: id, ParentID: parent, Stage: stage, Attempt: 1,
			Lifecycle: journal.AgentCompleted, StartedAt: now, UpdatedAt: now,
		}}
	}
	events := []journal.Event{
		agent("root", "", "implement"),
		agent("child", "root", "implement"),
		agent("other", "", "review"),
	}
	if got := transcriptRootAgentID(events, "implement"); got != "root" {
		t.Fatalf("transcriptRootAgentID = %q, want %q", got, "root")
	}
	if got := transcriptRootAgentID(events, "unknown-stage"); got != "" {
		t.Fatalf("transcriptRootAgentID = %q, want no link for a stage with no root", got)
	}
}
