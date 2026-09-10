package readservice

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestRunAgentProgressOrderingFidelityAndDegradation(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	const runID = "test-agent-progress-run"

	j, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "3771"},
		StartedAt:       time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	now := time.Now()
	coord := &journal.AgentProvenance{
		Schema:      "goobers.dev/journal/agent/v1",
		ID:          "coordinator-1",
		RunID:       runID,
		Stage:       "implement",
		Attempt:     1,
		Coordinator: true,
		Lifecycle:   journal.AgentStarted,
		StartedAt:   now,
		UpdatedAt:   now,
		Fidelity:    journal.AgentFidelityFull,
	}
	if err := j.Append(journal.Event{Type: journal.EventAgentLifecycle, Agent: coord}); err != nil {
		t.Fatalf("Append coord lifecycle: %v", err)
	}

	worker := &journal.AgentProvenance{
		Schema:    "goobers.dev/journal/agent/v1",
		ID:        "worker-1",
		ParentID:  "coordinator-1",
		RunID:     runID,
		Stage:     "implement",
		Attempt:   1,
		Worker:    true,
		Lifecycle: journal.AgentStarted,
		StartedAt: now,
		UpdatedAt: now,
		Fidelity:  journal.AgentFidelityFull,
	}
	if err := j.Append(journal.Event{Type: journal.EventAgentLifecycle, Agent: worker}); err != nil {
		t.Fatalf("Append worker lifecycle: %v", err)
	}

	// Add worker progress events out of order to verify sequence sorting
	p2 := journal.AgentProgress{
		Schema:     "goobers.dev/journal/agent-progress/v1",
		AgentID:    "worker-1",
		RunID:      runID,
		Stage:      "implement",
		Attempt:    1,
		Sequence:   2,
		Kind:       journal.AgentProgressDecision,
		Source:     journal.AgentProgressSourceModel,
		OccurredAt: time.Now(),
		Decision:   "Selected parser fix strategy",
		Evidence:   []journal.AgentProgressEvidence{{Type: "artifact", ID: "diff-1"}},
	}
	p1 := journal.AgentProgress{
		Schema:     "goobers.dev/journal/agent-progress/v1",
		AgentID:    "worker-1",
		RunID:      runID,
		Stage:      "implement",
		Attempt:    1,
		Sequence:   1,
		Kind:       journal.AgentProgressPlan,
		Source:     journal.AgentProgressSourceNative,
		OccurredAt: time.Now(),
		Plan:       []string{"Inspect code", "Patch bug"},
	}

	if err := j.Append(journal.Event{Type: journal.EventAgentProgress, Progress: &p2}); err != nil {
		t.Fatalf("Append p2: %v", err)
	}
	if err := j.Append(journal.Event{Type: journal.EventAgentProgress, Progress: &p1}); err != nil {
		t.Fatalf("Append p1: %v", err)
	}

	_ = j.Close()

	reads, err := NewOfflineRuns(layout)
	if err != nil {
		t.Fatalf("NewOfflineRuns: %v", err)
	}

	progress, err := reads.RunAgentProgress(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunAgentProgress: %v", err)
	}

	if len(progress) != 1 {
		t.Fatalf("expected 1 root agent (coordinator-1), got %d", len(progress))
	}
	rootAgent := progress[0]
	if rootAgent.AgentID != "coordinator-1" {
		t.Fatalf("root agent ID = %q, want coordinator-1", rootAgent.AgentID)
	}
	if !rootAgent.Degraded || rootAgent.Fidelity != journal.AgentFidelityNone {
		t.Fatalf("coordinator without progress events should be marked degraded fidelity=none")
	}
	if rootAgent.Current == nil || rootAgent.Current.Source != "lifecycle" || rootAgent.Current.Lifecycle != journal.AgentStarted {
		t.Fatalf("coordinator current status = %#v, want lifecycle started", rootAgent.Current)
	}

	if len(rootAgent.Children) != 1 {
		t.Fatalf("expected 1 child worker agent, got %d", len(rootAgent.Children))
	}
	child := rootAgent.Children[0]
	if child.AgentID != "worker-1" {
		t.Fatalf("child agent ID = %q, want worker-1", child.AgentID)
	}
	if len(child.History) != 2 {
		t.Fatalf("expected 2 progress records in history, got %d", len(child.History))
	}
	if child.History[0].Sequence >= child.History[1].Sequence {
		t.Fatalf("progress history not sorted by sequence: seq0=%d, seq1=%d", child.History[0].Sequence, child.History[1].Sequence)
	}
	if child.Latest == nil || child.Latest.Sequence != child.History[len(child.History)-1].Sequence {
		t.Fatalf("latest progress = %#v, want final durable history record", child.Latest)
	}
	if child.Current == nil || child.Current.Source != "progress" || child.Current.Sequence != child.Latest.Sequence {
		t.Fatalf("child current status = %#v, want progress seq=2", child.Current)
	}
}

func TestRunAgentProgressPreservesNestedGrandchildren(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	const runID = "test-agent-progress-grandchildren"

	j, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "3771"},
		StartedAt:       time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	now := time.Now().UTC()
	for _, event := range []journal.Event{
		{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "coordinator-1", RunID: runID, Stage: "implement",
			Attempt: 1, Coordinator: true, Lifecycle: journal.AgentStarted, StartedAt: now, UpdatedAt: now,
			Fidelity: journal.AgentFidelityFull,
		}},
		{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", ParentID: "coordinator-1", RunID: runID, Stage: "implement",
			Attempt: 1, Worker: true, Lifecycle: journal.AgentStarted, StartedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute),
			Fidelity: journal.AgentFidelityFull,
		}},
		{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "leaf-1", ParentID: "worker-1", RunID: runID, Stage: "implement",
			Attempt: 1, Leaf: true, Lifecycle: journal.AgentStarted, StartedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(2 * time.Minute),
			Fidelity: journal.AgentFidelityFull,
		}},
		{Type: journal.EventAgentProgress, Progress: &journal.AgentProgress{
			Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "leaf-1", RunID: runID,
			Stage: "implement", Attempt: 1, Kind: journal.AgentProgressSummary,
			Source: journal.AgentProgressSourceModel, OccurredAt: now.Add(3 * time.Minute),
			Summary: "Leaf completed its analysis.",
		}},
	} {
		if err := j.Append(event); err != nil {
			t.Fatalf("Append %s: %v", event.Type, err)
		}
	}
	_ = j.Close()

	reads, err := NewOfflineRuns(layout)
	if err != nil {
		t.Fatalf("NewOfflineRuns: %v", err)
	}
	progress, err := reads.RunAgentProgress(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunAgentProgress: %v", err)
	}
	if len(progress) != 1 {
		t.Fatalf("root summaries = %d, want 1", len(progress))
	}
	if len(progress[0].Children) != 1 {
		t.Fatalf("coordinator children = %d, want 1", len(progress[0].Children))
	}
	if len(progress[0].Children[0].Children) != 1 {
		t.Fatalf("worker grandchildren = %d, want 1", len(progress[0].Children[0].Children))
	}
	leaf := progress[0].Children[0].Children[0]
	if leaf.AgentID != "leaf-1" || leaf.Latest == nil || leaf.Latest.Summary != "Leaf completed its analysis." {
		t.Fatalf("leaf summary = %#v", leaf)
	}
}

func TestRunAgentProgressPreservesAttemptScopedCurrentStatus(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	const runID = "test-agent-progress-attempts"

	j, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "3771"},
		StartedAt:       time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	now := time.Now().UTC()
	for _, event := range []journal.Event{
		{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", RunID: runID, Stage: "implement",
			Attempt: 1, Worker: true, Lifecycle: journal.AgentStarted, StartedAt: now, UpdatedAt: now,
			Fidelity: journal.AgentFidelityFull,
		}},
		{Type: journal.EventAgentProgress, Progress: &journal.AgentProgress{
			Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "worker-1", RunID: runID,
			Stage: "implement", Attempt: 1, Kind: journal.AgentProgressSummary,
			Source: journal.AgentProgressSourceModel, OccurredAt: now, Summary: "Attempt one progress",
		}},
		{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", RunID: runID, Stage: "implement",
			Attempt: 2, Worker: true, Lifecycle: journal.AgentWaiting, StartedAt: now.Add(time.Minute),
			UpdatedAt: now.Add(time.Minute), Fidelity: journal.AgentFidelityNone,
		}},
		{Type: journal.EventAgentProgress, Progress: &journal.AgentProgress{
			Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "worker-1", RunID: runID,
			Stage: "implement", Attempt: 1, Kind: journal.AgentProgressDecision,
			Source: journal.AgentProgressSourceModel, OccurredAt: now.Add(2 * time.Minute),
			Decision: "Late attempt-one decision",
		}},
	} {
		if err := j.Append(event); err != nil {
			t.Fatalf("Append %s: %v", event.Type, err)
		}
	}
	_ = j.Close()

	reads, err := NewOfflineRuns(layout)
	if err != nil {
		t.Fatalf("NewOfflineRuns: %v", err)
	}
	progress, err := reads.RunAgentProgress(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunAgentProgress: %v", err)
	}
	if len(progress) != 2 {
		t.Fatalf("expected both attempts to remain distinct, got %d summaries", len(progress))
	}
	if progress[0].Attempt != 1 || progress[0].Current == nil || progress[0].Current.Source != "progress" || progress[0].Current.Sequence != progress[0].Latest.Sequence {
		t.Fatalf("attempt 1 current status = %#v", progress[0].Current)
	}
	if progress[1].Attempt != 2 || progress[1].Current == nil || progress[1].Current.Source != "lifecycle" || progress[1].Current.Lifecycle != journal.AgentWaiting {
		t.Fatalf("attempt 2 current status = %#v", progress[1].Current)
	}
}

func TestRunAgentProgressDropsLateProgressFromOlderPod(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	const runID = "test-agent-progress-stale-pod"

	j, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "3771"},
		StartedAt:       time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	now := time.Now().UTC()
	events := []journal.Event{
		{Type: journal.EventAgentLifecycle, Runner: map[string]any{"emitKey": "pod/1/agent.lifecycle"}, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", RunID: runID, Stage: "implement",
			Attempt: 1, Worker: true, Lifecycle: journal.AgentStarted, StartedAt: now, UpdatedAt: now,
			Fidelity: journal.AgentFidelityFull,
		}},
		{Type: journal.EventAgentProgress, Runner: map[string]any{"emitKey": "pod/1/agent.progress"}, Progress: &journal.AgentProgress{
			Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "worker-1", RunID: runID,
			Stage: "implement", Attempt: 1, Kind: journal.AgentProgressSummary,
			Source: journal.AgentProgressSourceModel, OccurredAt: now.Add(time.Minute),
			Summary: "Older pod summary",
		}},
		{Type: journal.EventAgentLifecycle, Runner: map[string]any{"emitKey": "pod/2/agent.lifecycle"}, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", RunID: runID, Stage: "implement",
			Attempt: 1, Worker: true, Lifecycle: journal.AgentResumed, StartedAt: now.Add(2 * time.Minute),
			UpdatedAt: now.Add(2 * time.Minute), Fidelity: journal.AgentFidelityNone,
		}},
		{Type: journal.EventAgentProgress, Runner: map[string]any{"emitKey": "pod/1/late.progress"}, Progress: &journal.AgentProgress{
			Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "worker-1", RunID: runID,
			Stage: "implement", Attempt: 1, Kind: journal.AgentProgressDecision,
			Source: journal.AgentProgressSourceModel, OccurredAt: now.Add(3 * time.Minute),
			Decision: "Late decision from older pod",
		}},
	}
	for _, event := range events {
		if err := j.Append(event); err != nil {
			t.Fatalf("Append %s: %v", event.Type, err)
		}
	}
	_ = j.Close()

	reads, err := NewOfflineRuns(layout)
	if err != nil {
		t.Fatalf("NewOfflineRuns: %v", err)
	}
	progress, err := reads.RunAgentProgress(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunAgentProgress: %v", err)
	}
	if len(progress) != 1 {
		t.Fatalf("root summaries = %d, want 1", len(progress))
	}
	summary := progress[0]
	if len(summary.History) != 1 || summary.History[0].Summary != "Older pod summary" {
		t.Fatalf("history = %#v, want pre-reconnect progress preserved", summary.History)
	}
	if summary.Current == nil || summary.Current.Source != "lifecycle" || summary.Current.Lifecycle != journal.AgentResumed {
		t.Fatalf("current status = %#v, want lifecycle resumed from latest pod", summary.Current)
	}
	if summary.Latest == nil || summary.Latest.Summary != "Older pod summary" {
		t.Fatalf("latest progress = %#v, want earlier valid progress record retained", summary.Latest)
	}
	if !summary.Degraded || summary.DegradedText == "" {
		t.Fatalf("degraded summary = %#v, want lifecycle-only degraded fallback", summary)
	}
}

func TestRunAgentProgressPreservesEarlierAttemptsAcrossNewerPods(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	const runID = "test-agent-progress-attempt-pods"

	j, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "3771"},
		StartedAt:       time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	now := time.Now().UTC()
	events := []journal.Event{
		{Type: journal.EventAgentLifecycle, Runner: map[string]any{"emitKey": "pod/1/agent.lifecycle"}, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", RunID: runID, Stage: "implement",
			Attempt: 1, Worker: true, Lifecycle: journal.AgentStarted, StartedAt: now, UpdatedAt: now,
			Fidelity: journal.AgentFidelityFull,
		}},
		{Type: journal.EventAgentProgress, Runner: map[string]any{"emitKey": "pod/1/agent.progress"}, Progress: &journal.AgentProgress{
			Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "worker-1", RunID: runID,
			Stage: "implement", Attempt: 1, Kind: journal.AgentProgressSummary,
			Source: journal.AgentProgressSourceModel, OccurredAt: now.Add(time.Minute),
			Summary: "Attempt one progress",
		}},
		{Type: journal.EventAgentLifecycle, Runner: map[string]any{"emitKey": "pod/2/agent.lifecycle"}, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", RunID: runID, Stage: "implement",
			Attempt: 2, Worker: true, Lifecycle: journal.AgentWaiting, StartedAt: now.Add(2 * time.Minute),
			UpdatedAt: now.Add(2 * time.Minute), Fidelity: journal.AgentFidelityNone,
		}},
		{Type: journal.EventAgentProgress, Runner: map[string]any{"emitKey": "pod/1/late.progress"}, Progress: &journal.AgentProgress{
			Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "worker-1", RunID: runID,
			Stage: "implement", Attempt: 1, Kind: journal.AgentProgressDecision,
			Source: journal.AgentProgressSourceModel, OccurredAt: now.Add(3 * time.Minute),
			Decision: "Late attempt-one decision",
		}},
	}
	for _, event := range events {
		if err := j.Append(event); err != nil {
			t.Fatalf("Append %s: %v", event.Type, err)
		}
	}
	_ = j.Close()

	reads, err := NewOfflineRuns(layout)
	if err != nil {
		t.Fatalf("NewOfflineRuns: %v", err)
	}
	progress, err := reads.RunAgentProgress(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunAgentProgress: %v", err)
	}
	if len(progress) != 2 {
		t.Fatalf("expected both attempts to remain visible, got %d summaries", len(progress))
	}
	if progress[0].Attempt != 1 || len(progress[0].History) != 2 || progress[0].Current == nil ||
		progress[0].Current.Source != "progress" || progress[0].Latest == nil ||
		progress[0].Latest.Decision != "Late attempt-one decision" {
		t.Fatalf("attempt 1 summary = %#v", progress[0])
	}
	if progress[1].Attempt != 2 || progress[1].Current == nil ||
		progress[1].Current.Source != "lifecycle" || progress[1].Current.Lifecycle != journal.AgentWaiting {
		t.Fatalf("attempt 2 current status = %#v", progress[1].Current)
	}
	if !progress[1].Degraded || progress[1].DegradedText == "" {
		t.Fatalf("attempt 2 degraded summary = %#v", progress[1])
	}
}
