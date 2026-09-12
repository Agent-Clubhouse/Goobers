package journal

import (
	"path/filepath"
	"testing"
	"time"
)

// Unstamped journals retain their historical ordering. Neither stage-visit
// boundaries nor arrival sequence identify which visit authored a late event.
func TestLegacyAgentVisitsRetainLogicalAttemptOrdering(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })
	at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	oldUsage, newUsage := int64(200), int64(30)
	events := []Event{
		{Type: EventStageStarted, Stage: "work", Attempt: 1},
		agentLifecycleEvent(at, "copilot:work", "", testIdentity().RunID, "work", 1, AgentFailed, AgentUsage{}),
		{Type: EventStageFinished, Stage: "work", Attempt: 1, Status: "failure"},
		{Type: EventStageStarted, Stage: "work", Attempt: 2, AttemptClass: "policy"},
		agentLifecycleEvent(at.Add(time.Minute), "copilot:work", "", testIdentity().RunID, "work", 2, AgentCompleted, AgentUsage{InputTokens: &oldUsage}),
		{Type: EventStageFinished, Stage: "work", Attempt: 2, Status: "success"},
		{Type: EventStageStarted, Stage: "review", Attempt: 1},
		{Type: EventStageFinished, Stage: "review", Attempt: 1, Status: "success", Outputs: map[string]any{"decision": "needs-changes"}},
		{Type: EventStageStarted, Stage: "work", Attempt: 1},
		agentLifecycleEvent(at.Add(2*time.Minute), "copilot:work", "", testIdentity().RunID, "work", 1, AgentWaiting, AgentUsage{InputTokens: &newUsage}),
	}
	for _, event := range events {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := AgentTree(persisted)
	if err != nil {
		t.Fatal(err)
	}
	active, err := reader.ActiveAgentTree("work", 1)
	if err != nil {
		t.Fatal(err)
	}
	usage := RollupRunAgentUsage(persisted, testIdentity().RunID)
	if tree["copilot:work"].Attempt != 2 || active["copilot:work"].Lifecycle != AgentWaiting || usage.InputTokens == nil || *usage.InputTokens != oldUsage {
		t.Fatalf("unexpected baseline: tree=%v active=%v usage=%v", tree, active, usage)
	}
	t.Logf("current visit attempt=1/waiting; aggregate=%d/%s; active=%s; cost uses old visit=%d", tree["copilot:work"].Attempt, tree["copilot:work"].Lifecycle, active["copilot:work"].Lifecycle, *usage.InputTokens)

	// A prior attempt's process can report after a later visit starts. Seq
	// records arrival; it cannot identify which work/1 visit authored this.
	late := agentLifecycleEvent(at, "copilot:work", "", testIdentity().RunID, "work", 1, AgentFailed, AgentUsage{})
	late.Agent.UpdatedAt = at.Add(3 * time.Minute)
	if err := run.Append(late); err != nil {
		t.Fatal(err)
	}
	active, err = reader.ActiveAgentTree("work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("unexpected late-delivery baseline: %v", active)
	}
	t.Log("late event from old work/1 removes active current work/1 agent")
}
