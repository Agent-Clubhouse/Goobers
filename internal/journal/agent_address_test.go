package journal

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestAgentAddressRoundTrip(t *testing.T) {
	address := AgentAddress{
		RunID:   "0af7651916cd43dd8448eb211c80319c",
		Stage:   "implement/child",
		Attempt: 2,
		AgentID: encodeJournalAgentToken("copilot:implement/root worker", 42),
	}
	raw := address.String()
	parsed, err := ParseAgentAddress(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != address {
		t.Fatalf("ParseAgentAddress() = %#v, want %#v", parsed, address)
	}
}

func TestResolveAgentAddress(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	const runID = "0af7651916cd43dd8448eb211c80319c"

	t.Run("top-level live", func(t *testing.T) {
		events := []Event{
			{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1},
			agentLifecycleEvent(now, "copilot:implement", "", runID, "implement", 1, AgentWaiting, AgentUsage{}),
		}
		events[1].Seq = 2
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("copilot:implement", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressLive || resolved.Agent == nil || resolved.Agent.ID != "copilot:implement" {
			t.Fatalf("resolved = %+v", resolved)
		}
	})

	t.Run("nested live", func(t *testing.T) {
		parent := agentLifecycleEvent(now, "copilot:implement", "", runID, "implement", 1, AgentWaiting, AgentUsage{})
		parent.Seq = 2
		child := agentLifecycleEvent(now.Add(time.Second), "worker-1", "copilot:implement", runID, "implement", 1, AgentStarted, AgentUsage{})
		child.Seq = 3
		events := []Event{{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1}, parent, child}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("worker-1", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressLive || resolved.Agent == nil || resolved.Agent.ParentID != "copilot:implement" {
			t.Fatalf("resolved = %+v", resolved)
		}
	})

	t.Run("stale superseded visit", func(t *testing.T) {
		waiting := agentLifecycleEvent(now, "copilot:implement", "", runID, "implement", 1, AgentWaiting, AgentUsage{})
		waiting.Seq = 2
		events := []Event{
			{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1},
			waiting,
			{Type: EventStageFinished, Seq: 3, Stage: "implement", Attempt: 1, Status: "success"},
			{Type: EventStageStarted, Seq: 4, Stage: "implement", Attempt: 2},
		}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("copilot:implement", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressStale {
			t.Fatalf("resolved status = %s, want stale", resolved.Status)
		}
	})

	t.Run("fabricated older visit address is historical", func(t *testing.T) {
		waiting := agentLifecycleEvent(now, "copilot:implement", "", runID, "implement", 1, AgentWaiting, AgentUsage{})
		waiting.Seq = 2
		events := []Event{
			{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1},
			waiting,
			{Type: EventStageFinished, Seq: 3, Stage: "implement", Attempt: 1, Status: "success"},
			{Type: EventStageStarted, Seq: 4, Stage: "implement", Attempt: 2},
		}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("fabricated-agent", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressHistorical {
			t.Fatalf("resolved status = %s, want historical", resolved.Status)
		}
	})

	t.Run("terminated agent", func(t *testing.T) {
		agent := agentLifecycleEvent(now, "worker-1", "", runID, "implement", 1, AgentCompleted, AgentUsage{})
		agent.Seq = 2
		events := []Event{
			{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1},
			agent,
		}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("worker-1", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressTerminated || resolved.Agent == nil || resolved.Agent.Lifecycle != AgentCompleted {
			t.Fatalf("resolved = %+v", resolved)
		}
	})

	t.Run("latest visit terminated after stage finished", func(t *testing.T) {
		waiting := agentLifecycleEvent(now, "worker-1", "", runID, "implement", 1, AgentWaiting, AgentUsage{})
		waiting.Seq = 2
		events := []Event{
			{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1},
			waiting,
			{Type: EventStageFinished, Seq: 3, Stage: "implement", Attempt: 1, Status: "success"},
		}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("worker-1", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressTerminated {
			t.Fatalf("resolved status = %s, want terminated", resolved.Status)
		}
	})

	t.Run("latest visit terminated after run finished", func(t *testing.T) {
		waiting := agentLifecycleEvent(now, "copilot:implement", "", runID, "implement", 1, AgentWaiting, AgentUsage{})
		waiting.Seq = 2
		events := []Event{
			{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1},
			waiting,
			{Type: EventRunFinished, Seq: 3, Status: string(PhaseCompleted)},
		}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("copilot:implement", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressTerminated {
			t.Fatalf("resolved status = %s, want terminated", resolved.Status)
		}
	})

	t.Run("foreign run", func(t *testing.T) {
		events := []Event{{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1}}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: "1bf7651916cd43dd8448eb211c80319c", Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("copilot:implement", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressHistorical {
			t.Fatalf("resolved status = %s, want historical", resolved.Status)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		resolved, err := ResolveAgentAddress(nil, runID, "not-an-agent-address")
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressMalformed {
			t.Fatalf("resolved status = %s, want malformed", resolved.Status)
		}
	})

	t.Run("collided live address is unreachable", func(t *testing.T) {
		top := agentLifecycleEvent(now, "copilot:implement", "", runID, "implement", 1, AgentWaiting, AgentUsage{})
		top.Seq = 2
		nested := agentLifecycleEvent(now.Add(time.Second), "copilot:implement", "parent", runID, "implement", 1, AgentStarted, AgentUsage{})
		nested.Seq = 3
		events := []Event{
			{Type: EventStageStarted, Seq: 1, Stage: "implement", Attempt: 1},
			top,
			nested,
		}
		resolved, err := ResolveAgentAddress(events, runID, AgentAddress{
			RunID: runID, Stage: "implement", Attempt: 1, AgentID: encodeJournalAgentToken("copilot:implement", 1),
		}.String())
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != AgentAddressUnreachable {
			t.Fatalf("resolved status = %s, want unreachable", resolved.Status)
		}
	})
}

func TestAddressableAgentsExcludeUnaddressableAndAreRunScoped(t *testing.T) {
	root := t.TempDir()
	runOneID := testIdentity()
	runOneID.RunID = "0af7651916cd43dd8448eb211c80319c"
	runTwoID := testIdentity()
	runTwoID.RunID = "1bf7651916cd43dd8448eb211c80319c"

	runOne, err := Create(root, runOneID, nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runOne.Close() })
	runTwo, err := Create(root, runTwoID, nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runTwo.Close() })

	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	for _, event := range []Event{
		{Type: EventStageStarted, Stage: "implement", Attempt: 1},
		agentLifecycleEvent(now, "copilot:implement", "", runOneID.RunID, "implement", 1, AgentWaiting, AgentUsage{}),
		agentLifecycleEvent(now.Add(time.Second), "worker-1", "copilot:implement", runOneID.RunID, "implement", 1, AgentStarted, AgentUsage{}),
		agentLifecycleEvent(now.Add(2*time.Second), "worker-terminated", "copilot:implement", runOneID.RunID, "implement", 1, AgentCompleted, AgentUsage{}),
		{Type: EventStageStarted, Stage: "review", Attempt: 1},
		agentLifecycleEvent(now.Add(3*time.Second), "stale-reviewer", "", runOneID.RunID, "review", 1, AgentWaiting, AgentUsage{}),
		{Type: EventStageFinished, Stage: "review", Attempt: 1, Status: "success"},
	} {
		if err := runOne.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []Event{
		{Type: EventStageStarted, Stage: "implement", Attempt: 1},
		agentLifecycleEvent(now, "other-run-agent", "", runTwoID.RunID, "implement", 1, AgentWaiting, AgentUsage{}),
	} {
		if err := runTwo.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	reader, err := OpenRead(filepath.Join(root, runOneID.RunID))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := reader.AddressableAgents()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(agents))
	for _, agent := range agents {
		token, err := parseJournalAgentToken(agent.Address.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, token.rawAgent)
		if agent.Address.RunID != runOneID.RunID {
			t.Fatalf("enumerated foreign run address: %+v", agent.Address)
		}
	}
	if !slices.Equal(got, []string{"copilot:implement", "worker-1"}) {
		t.Fatalf("addressable agents = %v", got)
	}
}

func TestAddressableAgentsExcludeCollidedLiveAddresses(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	identity.RunID = "0af7651916cd43dd8448eb211c80319c"
	run, err := Create(root, identity, nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	for _, event := range []Event{
		{Type: EventStageStarted, Stage: "implement", Attempt: 1},
		agentLifecycleEvent(now, "copilot:implement", "", identity.RunID, "implement", 1, AgentWaiting, AgentUsage{}),
		agentLifecycleEvent(now.Add(time.Second), "copilot:implement", "parent", identity.RunID, "implement", 1, AgentStarted, AgentUsage{}),
		agentLifecycleEvent(now.Add(2*time.Second), "worker-1", "copilot:implement", identity.RunID, "implement", 1, AgentStarted, AgentUsage{}),
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	reader, err := OpenRead(filepath.Join(root, identity.RunID))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := reader.AddressableAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("addressable agents = %+v, want only the unique live address", agents)
	}
	token, err := parseJournalAgentToken(agents[0].Address.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if token.rawAgent != "worker-1" {
		t.Fatalf("enumerated raw agent = %q, want worker-1", token.rawAgent)
	}
}

func TestCollidedLatestVisitAddressesStayUnreachableAfterOneTargetEnds(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	identity.RunID = "0af7651916cd43dd8448eb211c80319c"
	run, err := Create(root, identity, nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	if err := run.Append(Event{Type: EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	startedSeq := run.Seq()
	for _, event := range []Event{
		agentLifecycleEvent(now, "copilot:implement", "", identity.RunID, "implement", 1, AgentWaiting, AgentUsage{}),
		agentLifecycleEvent(now.Add(time.Second), "copilot:implement", "parent", identity.RunID, "implement", 1, AgentStarted, AgentUsage{}),
		agentLifecycleEvent(now.Add(2*time.Second), "copilot:implement", "parent", identity.RunID, "implement", 1, AgentCompleted, AgentUsage{}),
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	reader, err := OpenRead(filepath.Join(root, identity.RunID))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := reader.AddressableAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("addressable agents = %+v, want collided latest-visit address excluded after one target ends", agents)
	}

	address := AgentAddress{
		RunID:   identity.RunID,
		Stage:   "implement",
		Attempt: 1,
		AgentID: encodeJournalAgentToken("copilot:implement", startedSeq),
	}
	resolved, err := reader.ResolveAgentAddress(address.String())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != AgentAddressUnreachable {
		t.Fatalf("resolved = %+v, want unreachable", resolved)
	}
}

func TestResolveAgentAddressMarksEarlierVisitsStale(t *testing.T) {
	for _, tt := range []struct {
		name        string
		secondAgent string
	}{
		{name: "same agent id", secondAgent: "worker-1"},
		{name: "different agent id", secondAgent: "worker-2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			identity := testIdentity()
			identity.RunID = "0af7651916cd43dd8448eb211c80319c"
			run, err := Create(root, identity, nil, WithClock(fixedClock()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := run.Close(); err != nil {
					t.Errorf("close run: %v", err)
				}
			}()
			now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
			start := Event{Type: EventStageStarted, Stage: "work", Attempt: 1}
			if err := run.Append(start); err != nil {
				t.Fatal(err)
			}
			firstSeq := run.Seq()
			if err := run.Append(agentLifecycleEvent(now, "worker-1", "", identity.RunID, "work", 1, AgentWaiting, AgentUsage{})); err != nil {
				t.Fatal(err)
			}
			old := AgentAddress{RunID: identity.RunID, Stage: "work", Attempt: 1, AgentID: encodeJournalAgentToken("worker-1", firstSeq)}
			if err := run.Append(Event{Type: EventStageFinished, Stage: "work", Attempt: 1, Status: "success"}); err != nil {
				t.Fatal(err)
			}
			if err := run.Append(Event{Type: EventStageStarted, Stage: "work", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			if err := run.Append(agentLifecycleEvent(now.Add(time.Second), tt.secondAgent, "", identity.RunID, "work", 1, AgentWaiting, AgentUsage{})); err != nil {
				t.Fatal(err)
			}

			reader, err := OpenRead(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.ResolveAgentAddress(old.String())
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Status != AgentAddressStale {
				t.Fatalf("resolved = %+v, want stale", resolved)
			}
			addresses, err := reader.AddressableAgents()
			if err != nil {
				t.Fatal(err)
			}
			for _, agent := range addresses {
				if agent.Address.String() == old.String() {
					t.Fatalf("stale address leaked into enumeration: %+v", addresses)
				}
			}
		})
	}
}
