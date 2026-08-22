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
