package journal

import (
	"testing"
	"time"
)

func TestActiveAgentTreeKeepsWaitingCoordinatorAndChildren(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	events := []Event{
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "coordinator", RunID: "run",
			Stage: "work", Attempt: 1, Lifecycle: AgentWaiting, StartedAt: now, UpdatedAt: now,
		}},
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker", ParentID: "coordinator",
			RunID: "run", Stage: "work", Attempt: 1, Lifecycle: AgentStarted, StartedAt: now, UpdatedAt: now,
		}},
	}
	tree, err := ActiveAgentTree(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree["worker"].ParentID != "coordinator" {
		t.Fatalf("tree = %#v", tree)
	}
}

func TestRollupAgentUsageUsesLatestRetryOnce(t *testing.T) {
	one, two := int64(10), int64(20)
	events := []Event{
		{Agent: &AgentProvenance{ID: "worker", Attempt: 1, Usage: AgentUsage{InputTokens: &one}}},
		{Agent: &AgentProvenance{ID: "worker", Attempt: 2, Usage: AgentUsage{InputTokens: &two}}},
	}
	usage := RollupAgentUsage(events)
	if usage.InputTokens == nil || *usage.InputTokens != 20 {
		t.Fatalf("usage = %#v, want latest retry only", usage.InputTokens)
	}
}

func TestActiveAgentTreeForStageFiltersRunsStagesAndEventTypes(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	agent := func(id, run, stage string, lifecycle AgentLifecycle) Event {
		return Event{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: id, RunID: run, Stage: stage,
			Attempt: 1, Lifecycle: lifecycle, StartedAt: now, UpdatedAt: now,
		}}
	}
	events := []Event{
		agent("wanted", "run-1", "work", AgentStarted),
		agent("other-run", "run-2", "work", AgentStarted),
		agent("other-stage", "run-1", "review", AgentStarted),
		{Type: EventAgentMessage, PeerMessage: &PeerMessageMetadata{ID: "m", SenderID: "wanted", RecipientID: "other", Purpose: "finding"}},
	}
	tree, err := ActiveAgentTreeForStage(events, "run-1", "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 || tree["wanted"].ID != "wanted" {
		t.Fatalf("tree = %#v", tree)
	}
}

func TestAgentTreeRejectsMalformedNonLifecyclePayload(t *testing.T) {
	_, err := AgentTree([]Event{{Type: EventAgentLifecycle}})
	if err == nil {
		t.Fatal("expected malformed lifecycle event to fail")
	}
}

func TestRollupAgentUsagePreservesObservedLatestAttemptAndExcludesCoordinator(t *testing.T) {
	one, two := int64(10), int64(20)
	events := []Event{
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			ID: "coordinator", RunID: "run", Stage: "work", Attempt: 1, Coordinator: true,
			Usage: AgentUsage{InputTokens: &one},
		}},
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			ID: "worker", RunID: "run", Stage: "work", Attempt: 2,
			Usage: AgentUsage{InputTokens: &two},
		}},
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			ID: "worker", RunID: "run", Stage: "work", Attempt: 1,
			Usage: AgentUsage{InputTokens: &one},
		}},
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			ID: "worker", RunID: "run", Stage: "work", Attempt: 2,
		}},
	}
	usage := RollupAgentUsageForStage(events, "run", "work")
	if usage.InputTokens == nil || *usage.InputTokens != 20 {
		t.Fatalf("usage = %#v, want latest worker attempt", usage.InputTokens)
	}
}

func TestValidateAgentEventRejectsUnsupportedAndIncompleteEvents(t *testing.T) {
	if err := ValidateAgentEvent(Event{Type: EventStageStarted}); err == nil {
		t.Fatal("expected unsupported event type to fail")
	}
	if err := ValidateAgentEvent(Event{Type: EventAgentMessage}); err == nil {
		t.Fatal("expected incomplete peer metadata to fail")
	}
}
