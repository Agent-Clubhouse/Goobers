package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNestedAgentPayloadIsRedactedAtJournalBoundary(t *testing.T) {
	const secret = "nested-secret-value"
	registry, scrubber := DefaultScrubber()
	registry.Register([]byte(secret))
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrubber), WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	events := []Event{
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			Schema: agentSchemaV1, ID: "worker-" + secret, ParentID: "parent-" + secret,
			RunID: testIdentity().RunID, Stage: "work-" + secret, Attempt: 1,
			Plugin: "plugin-" + secret, Objective: "assignment " + secret,
			RequestedModel: "requested-" + secret, ResolvedModel: "resolved-" + secret,
			RequestedReasoningEffort: "requested-" + secret, ResolvedReasoningEffort: "resolved-" + secret,
			Lifecycle: AgentCompleted, StartedAt: now, UpdatedAt: now,
			Results:   []Ref{{Path: "artifacts/" + secret, Digest: "sha256:" + secret, MediaType: "application/" + secret}},
			DependsOn: []string{"dependency-" + secret}, Fidelity: AgentFidelityFull,
		}},
		{Type: EventAgentMessage, PeerMessage: &PeerMessageMetadata{
			ID: "message-" + secret, SenderID: "sender-" + secret, RecipientID: "recipient-" + secret,
			OccurredAt: now, Purpose: "dependency-" + secret,
			Artifact:    &Ref{Path: "artifacts/" + secret, Digest: "sha256:" + secret, MediaType: "text/" + secret},
			ContentHash: "sha256:" + secret,
		}},
	}
	for _, event := range events {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(root, testIdentity().RunID, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("journal retained secret in nested-agent payload: %s", raw)
	}
	if count := strings.Count(string(raw), Redacted); count < 15 {
		t.Fatalf("redaction count = %d, want comprehensive field coverage: %s", count, raw)
	}

	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("typed journal read restored secret: %s", encoded)
	}
}

func TestActiveAgentTreeKeepsWaitingCoordinatorAndChildren(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	events := []Event{
		agentLifecycleEvent(now, "coordinator", "", "run", "work", 1, AgentWaiting, AgentUsage{}),
		agentLifecycleEvent(now, "worker", "coordinator", "run", "work", 1, AgentStarted, AgentUsage{}),
	}
	events[0].Agent.Coordinator = true
	events[1].Agent.Worker = true
	tree, err := activeAgentTree(events, "run", "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree["worker"].ParentID != "coordinator" {
		t.Fatalf("tree = %#v", tree)
	}
}

func TestActiveAgentTreeUsesLatestStageAttempt(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	tree, err := activeAgentTree([]Event{
		agentLifecycleEvent(now, "old-worker", "", "run", "work", 1, AgentStarted, AgentUsage{}),
		agentLifecycleEvent(now.Add(time.Second), "new-worker", "", "run", "work", 2, AgentStarted, AgentUsage{}),
	}, "run", "work", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 || tree["new-worker"].Attempt != 2 {
		t.Fatalf("tree = %#v, want only latest attempt", tree)
	}
}

func TestReaderActiveAgentTreeObservesInFlightAppend(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	if err := run.Append(agentLifecycleEvent(
		now, "worker", "", testIdentity().RunID, "work", 1, AgentStarted, AgentUsage{},
	)); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := reader.ActiveAgentTree("work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 || tree["worker"].Lifecycle != AgentStarted {
		t.Fatalf("live tree = %#v", tree)
	}
}

func TestRollupAgentUsageUsesLatestRetryOnce(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	one, two := int64(10), int64(20)
	events := []Event{
		agentLifecycleEvent(now, "worker", "", "run", "work", 1, AgentCompleted, AgentUsage{InputTokens: &one}),
		agentLifecycleEvent(now.Add(time.Second), "worker", "", "run", "work", 2, AgentCompleted, AgentUsage{InputTokens: &two}),
	}
	usage := RollupAgentUsage(events)
	if usage.InputTokens == nil || *usage.InputTokens != 20 {
		t.Fatalf("usage = %#v, want latest retry only", usage.InputTokens)
	}
}

func TestRollupAgentUsageDropsAgentsFromOlderStageAttempt(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	oldCoordinator, currentWorker := int64(10), int64(20)
	coordinator := agentLifecycleEvent(now, "coordinator", "", "run", "work", 1, AgentCompleted, AgentUsage{InputTokens: &oldCoordinator})
	coordinator.Agent.Coordinator = true
	worker := agentLifecycleEvent(now.Add(time.Second), "worker", "", "run", "work", 2, AgentCompleted, AgentUsage{InputTokens: &currentWorker})
	usage := rollupAgentUsage([]Event{coordinator, worker}, "run", "work")
	if usage.InputTokens == nil || *usage.InputTokens != 20 {
		t.Fatalf("usage = %#v, want only latest stage attempt", usage.InputTokens)
	}
}

func TestActiveAgentTreeForStageFiltersRunsStagesAndEventTypes(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	events := []Event{
		agentLifecycleEvent(now, "wanted", "", "run-1", "work", 1, AgentStarted, AgentUsage{}),
		agentLifecycleEvent(now, "other-run", "", "run-2", "work", 1, AgentStarted, AgentUsage{}),
		agentLifecycleEvent(now, "other-stage", "", "run-1", "review", 1, AgentStarted, AgentUsage{}),
		{Type: EventAgentMessage, PeerMessage: &PeerMessageMetadata{
			ID: "m", SenderID: "wanted", RecipientID: "other", Purpose: "finding", OccurredAt: now,
		}},
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

func TestRollupAgentUsageKeepsIndependentCoordinatorButExcludesProvenAggregate(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	coordinatorTokens, workerTokens, peerTokens := int64(10), int64(20), int64(5)
	independent := agentLifecycleEvent(now, "coordinator", "", "run", "work", 1, AgentCompleted, AgentUsage{InputTokens: &coordinatorTokens})
	independent.Agent.Coordinator = true
	peer := agentLifecycleEvent(now, "peer", "", "run", "work", 1, AgentCompleted, AgentUsage{InputTokens: &peerTokens})
	usage := rollupAgentUsage([]Event{independent, peer}, "run", "work")
	if usage.InputTokens == nil || *usage.InputTokens != 15 {
		t.Fatalf("independent coordinator usage = %#v, want 15", usage.InputTokens)
	}

	child := agentLifecycleEvent(now, "worker", "coordinator", "run", "work", 1, AgentCompleted, AgentUsage{InputTokens: &workerTokens})
	usage = rollupAgentUsage([]Event{independent, child}, "run", "work")
	if usage.InputTokens == nil || *usage.InputTokens != 20 {
		t.Fatalf("aggregate coordinator usage = %#v, want child-only 20", usage.InputTokens)
	}
}

func TestRollupAgentUsageForRunSumsDeduplicatedStages(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	oldAttempt, implement, review := int64(100), int64(20), int64(7)
	events := []Event{
		agentLifecycleEvent(now, "worker", "", "run", "implement", 1, AgentCompleted, AgentUsage{InputTokens: &oldAttempt}),
		agentLifecycleEvent(now.Add(time.Second), "worker", "", "run", "implement", 2, AgentCompleted, AgentUsage{InputTokens: &implement}),
		agentLifecycleEvent(now, "reviewer", "", "run", "review", 1, AgentCompleted, AgentUsage{InputTokens: &review}),
		agentLifecycleEvent(now, "other", "", "other-run", "implement", 1, AgentCompleted, AgentUsage{InputTokens: &oldAttempt}),
	}
	var usage AgentUsage
	addAgentUsage(&usage, rollupAgentUsage(events, "run", "implement"))
	addAgentUsage(&usage, rollupAgentUsage(events, "run", "review"))
	if usage.InputTokens == nil || *usage.InputTokens != 27 {
		t.Fatalf("run usage = %#v, want 27", usage.InputTokens)
	}
}

func TestValidateAgentEventRejectsUnsupportedAndIncompleteEvents(t *testing.T) {
	if err := ValidateAgentEvent(Event{Type: EventStageStarted}); err == nil {
		t.Fatal("expected unsupported event type to fail")
	}
	if err := ValidateAgentEvent(Event{Type: EventAgentMessage}); err == nil {
		t.Fatal("expected incomplete peer metadata to fail")
	}
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	event := agentLifecycleEvent(now, "worker", "", "run", "work", 1, AgentCompleted, AgentUsage{})
	event.Agent.Fidelity = "invented"
	if err := ValidateAgentEvent(event); err == nil {
		t.Fatal("expected invalid fidelity to fail")
	}
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })
	if err := run.Append(Event{Type: EventAgentLifecycle}); err == nil {
		t.Fatal("journal boundary accepted malformed nested-agent event")
	}
}

const agentSchemaV1 = "goobers.dev/journal/agent/v1"

func agentLifecycleEvent(at time.Time, id, parentID, runID, stage string, attempt int, lifecycle AgentLifecycle, usage AgentUsage) Event {
	return Event{Type: EventAgentLifecycle, Agent: &AgentProvenance{
		Schema: agentSchemaV1, ID: id, ParentID: parentID, RunID: runID, Stage: stage,
		Attempt: attempt, Lifecycle: lifecycle, StartedAt: at, UpdatedAt: at,
		Usage: usage, Fidelity: AgentFidelityFull,
	}}
}
