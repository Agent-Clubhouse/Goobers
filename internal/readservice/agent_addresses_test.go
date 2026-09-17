package readservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func agentAddressDefinition() workflow.Definition {
	return workflow.Definition{
		Name:    "implementation",
		Version: 7,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers",
			Start:  "implement",
			Tasks: []apiv1.Task{
				{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement", Goober: "coder", Next: "verify"},
				{Name: "verify", Type: apiv1.TaskDeterministic, Goal: "verify"},
			},
		},
	}
}

func agentAddressService(t *testing.T) (*Local, instance.Layout, workflow.Definition) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return service, layout, agentAddressDefinition()
}

func createAgentAddressRun(t *testing.T, layout instance.Layout, def workflow.Definition, runID string, startedAt time.Time) (*journal.Run, *fixtureClock) {
	t.Helper()
	definition, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := workflow.ComputeDigest(def)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fixtureClock{now: startedAt}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        def.Name,
		WorkflowVersion: def.Version,
		WorkflowDigest:  digest,
		Gaggle:          def.Spec.Gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       startedAt,
	}, map[string][]byte{
		journal.PinnedWorkflowDefinitionInputName: definition,
	}, journal.WithClock(func() time.Time { return clock.now }))
	if err != nil {
		t.Fatal(err)
	}
	return run, clock
}

func appendAgenticStageStart(t *testing.T, run *journal.Run, clock *fixtureClock, attempt int) uint64 {
	t.Helper()
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageStarted,
		Stage:   "implement",
		Attempt: attempt,
	}); err != nil {
		t.Fatal(err)
	}
	return run.Seq()
}

func appendAgentLifecycle(t *testing.T, run *journal.Run, clock *fixtureClock, runID string, attempt int, id string, lifecycle journal.AgentLifecycle) {
	t.Helper()
	clock.advance(time.Second)
	at := clock.now
	if err := run.Append(journal.Event{
		Type: journal.EventAgentLifecycle,
		Agent: &journal.AgentProvenance{
			Schema:    "goobers.dev/journal/agent/v1",
			ID:        id,
			RunID:     runID,
			Stage:     "implement",
			Attempt:   attempt,
			ParentID:  "coder",
			Worker:    true,
			Lifecycle: lifecycle,
			StartedAt: at,
			UpdatedAt: at,
			Fidelity:  journal.AgentFidelityFull,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func finishAgenticStage(t *testing.T, run *journal.Run, clock *fixtureClock, attempt int) {
	t.Helper()
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: attempt,
		Status:  string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatal(err)
	}
}

func appendMalformedForeignLifecycle(t *testing.T, runDir, foreignRunID string, seq uint64, at time.Time) {
	t.Helper()
	event := journal.Event{
		Schema: journal.EventSchema,
		Seq:    seq,
		Time:   at,
		Type:   journal.EventAgentLifecycle,
		Agent: &journal.AgentProvenance{
			Schema:    "goobers.dev/journal/agent/v1",
			ID:        "foreign-worker",
			RunID:     foreignRunID,
			Stage:     "implement",
			Attempt:   1,
			Lifecycle: "broken",
			StartedAt: at,
			UpdatedAt: at,
		},
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(runDir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestParseAgentAddressRoundTripsEscapedSegments(t *testing.T) {
	address := AgentAddress{
		Schema:  AgentAddressSchema,
		RunID:   "run-1",
		Stage:   "implement",
		Attempt: 2,
		Agent:   encodeAddressAgent("worker/a:b", 42),
	}
	raw := address.String()
	if !strings.HasPrefix(raw, journal.AgentAddressSchema+"/") {
		t.Fatalf("raw address = %q, want journal schema prefix", raw)
	}
	journalAddress, err := journal.ParseAgentAddress(raw)
	if err != nil {
		t.Fatalf("journal.ParseAgentAddress: %v", err)
	}
	if journalAddress.AgentID != address.Agent {
		t.Fatalf("journal agent id = %q, want %q", journalAddress.AgentID, address.Agent)
	}
	parsed, err := ParseAgentAddress(raw)
	if err != nil {
		t.Fatalf("ParseAgentAddress: %v", err)
	}
	if !reflect.DeepEqual(parsed, address) {
		t.Fatalf("parsed = %+v, want %+v", parsed, address)
	}
}

func TestResolveAgentAddressReportsMalformedAndHistorical(t *testing.T) {
	service, layout, def := agentAddressService(t)
	run, clock := createAgentAddressRun(t, layout, def, "run-live", time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	appendAgenticStageStart(t, run, clock, 1)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := service.ResolveAgentAddress(context.Background(), "run-live", "not-an-address")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionMalformed || got.Address != nil || got.Detail == "" {
		t.Fatalf("malformed resolution = %+v", got)
	}

	other := agentAddressFor("run-other", "implement", 1, "coder", 1)
	got, err = service.ResolveAgentAddress(context.Background(), "run-live", other.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionHistorical || got.Address == nil || got.Address.RunID != "run-other" {
		t.Fatalf("historical resolution = %+v", got)
	}
}

func TestResolveAgentAddressResolvesLiveTopLevelAndNestedAgents(t *testing.T) {
	service, layout, def := agentAddressService(t)
	runID := "run-live"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	startedSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentWaiting)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	top := agentAddressFor(runID, "implement", 1, "coder", startedSeq)
	got, err := service.ResolveAgentAddress(context.Background(), runID, top.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionResolved || got.Agent == nil || got.Agent.Kind != AgentAddressTopLevel {
		t.Fatalf("top-level resolution = %+v", got)
	}

	nested := agentAddressFor(runID, "implement", 1, "worker-1", startedSeq)
	got, err = service.ResolveAgentAddress(context.Background(), runID, nested.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionResolved || got.Agent == nil || got.Agent.Kind != AgentAddressNested {
		t.Fatalf("nested resolution = %+v", got)
	}
}

func TestResolveAgentAddressReportsStaleTopLevelAndNestedAttempts(t *testing.T) {
	service, layout, def := agentAddressService(t)
	runID := "run-stale"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	firstSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentCompleted)
	finishAgenticStage(t, run, clock, 1)
	appendAgenticStageStart(t, run, clock, 2)
	appendAgentLifecycle(t, run, clock, runID, 2, "worker-1", journal.AgentWaiting)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	top := agentAddressFor(runID, "implement", 1, "coder", firstSeq)
	got, err := service.ResolveAgentAddress(context.Background(), runID, top.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionStale {
		t.Fatalf("top-level stale resolution = %+v", got)
	}

	nested := agentAddressFor(runID, "implement", 1, "worker-1", firstSeq)
	got, err = service.ResolveAgentAddress(context.Background(), runID, nested.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionStale {
		t.Fatalf("nested stale resolution = %+v", got)
	}
}

func TestResolveAgentAddressReportsFabricatedOlderVisitAsHistorical(t *testing.T) {
	service, layout, def := agentAddressService(t)
	runID := "run-fabricated"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	firstSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentCompleted)
	finishAgenticStage(t, run, clock, 1)
	appendAgenticStageStart(t, run, clock, 2)
	appendAgentLifecycle(t, run, clock, runID, 2, "worker-2", journal.AgentWaiting)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	fabricated := agentAddressFor(runID, "implement", 1, "fabricated-agent", firstSeq)
	got, err := service.ResolveAgentAddress(context.Background(), runID, fabricated.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionHistorical {
		t.Fatalf("fabricated older visit resolution = %+v", got)
	}
}

func TestResolveAgentAddressReportsTerminatedTopLevelAndNestedAgents(t *testing.T) {
	service, layout, def := agentAddressService(t)
	runID := "run-terminated"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	startedSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentWaiting)
	finishAgenticStage(t, run, clock, 1)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	top := agentAddressFor(runID, "implement", 1, "coder", startedSeq)
	got, err := service.ResolveAgentAddress(context.Background(), runID, top.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionTerminated {
		t.Fatalf("top-level terminated resolution = %+v", got)
	}

	nested := agentAddressFor(runID, "implement", 1, "worker-1", startedSeq)
	got, err = service.ResolveAgentAddress(context.Background(), runID, nested.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != AgentResolutionTerminated {
		t.Fatalf("nested terminated resolution = %+v", got)
	}
}

func TestResolveAgentAddressReportsLatestVisitTerminatedWhenRunEnds(t *testing.T) {
	service, layout, def := agentAddressService(t)
	runID := "run-finished"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	startedSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentWaiting)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(apiv1.ResultFailure),
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	for _, address := range []AgentAddress{
		agentAddressFor(runID, "implement", 1, "coder", startedSeq),
		agentAddressFor(runID, "implement", 1, "worker-1", startedSeq),
	} {
		got, err := service.ResolveAgentAddress(context.Background(), runID, address.String())
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != AgentResolutionTerminated {
			t.Fatalf("%s resolution = %+v, want terminated", address.String(), got)
		}
	}
}

func TestAddressableAgentsExcludeUnaddressableCollisions(t *testing.T) {
	service, layout, def := agentAddressService(t)
	runID := "run-collision"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	startedSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "coder", journal.AgentWaiting)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentWaiting)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := service.AddressableAgents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("addressable agents = %+v, want only one uniquely addressed agent", got)
	}
	token, err := parseAddressAgent(got[0].Address.Agent)
	if err != nil || token.rawAgent != "worker-1" {
		t.Fatalf("unique live address = %+v, token=%+v err=%v", got[0], token, err)
	}

	colliding := agentAddressFor(runID, "implement", 1, "coder", startedSeq)
	resolution, err := service.ResolveAgentAddress(context.Background(), runID, colliding.String())
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != AgentResolutionUnaddressable {
		t.Fatalf("colliding resolution = %+v", resolution)
	}
}

func TestAddressableAgentsRejectsMalformedRunID(t *testing.T) {
	service, _, _ := agentAddressService(t)
	_, err := service.AddressableAgents(context.Background(), "..\\escape")
	if err == nil || !strings.Contains(err.Error(), ErrInvalidArgument.Error()) {
		t.Fatalf("err = %v, want invalid-argument run-id rejection", err)
	}
}

func TestAddressableAgentsAreScopedToTheSelectedRun(t *testing.T) {
	service, layout, def := agentAddressService(t)
	started := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	runOne, clockOne := createAgentAddressRun(t, layout, def, "run-one", started)
	appendAgenticStageStart(t, runOne, clockOne, 1)
	appendAgentLifecycle(t, runOne, clockOne, "run-one", 1, "worker-1", journal.AgentWaiting)
	if err := runOne.Close(); err != nil {
		t.Fatal(err)
	}

	runTwo, clockTwo := createAgentAddressRun(t, layout, def, "run-two", started.Add(time.Minute))
	appendAgenticStageStart(t, runTwo, clockTwo, 1)
	appendAgentLifecycle(t, runTwo, clockTwo, "run-two", 1, "worker-2", journal.AgentWaiting)
	if err := runTwo.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := service.AddressableAgents(context.Background(), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("addressable agents = %+v, want top-level and nested agents from one run", got)
	}
	for _, agent := range got {
		if agent.Address.RunID != "run-one" {
			t.Fatalf("address %q escaped the selected run", agent.Address.String())
		}
	}
}

func TestAddressableAgentsIgnoreMalformedForeignRunEvents(t *testing.T) {
	service, layout, def := agentAddressService(t)
	runID := "run-live"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	startedSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentWaiting)
	lastSeq := run.Seq()
	runDir := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	appendMalformedForeignLifecycle(t, runDir, "run-other", lastSeq+1, clock.now.Add(time.Second))

	agents, err := service.AddressableAgents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("addressable agents = %+v, want local top-level and nested addresses only", agents)
	}
	address := agentAddressFor(runID, "implement", 1, "worker-1", startedSeq)
	resolution, err := service.ResolveAgentAddress(context.Background(), runID, address.String())
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != AgentResolutionResolved {
		t.Fatalf("resolution with foreign malformed event = %+v", resolution)
	}
}

func TestResolveAgentAddressMarksEarlierVisitsStale(t *testing.T) {
	for _, tt := range []struct {
		name        string
		secondAgent string
	}{
		{name: "same nested id", secondAgent: "worker-1"},
		{name: "different nested id", secondAgent: "worker-2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service, layout, def := agentAddressService(t)
			runID := strings.ReplaceAll(tt.name, " ", "-")
			run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
			firstSeq := appendAgenticStageStart(t, run, clock, 1)
			appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentWaiting)

			oldTop := agentAddressFor(runID, "implement", 1, "coder", firstSeq)
			oldNested := agentAddressFor(runID, "implement", 1, "worker-1", firstSeq)

			finishAgenticStage(t, run, clock, 1)
			secondSeq := appendAgenticStageStart(t, run, clock, 1)
			appendAgentLifecycle(t, run, clock, runID, 1, tt.secondAgent, journal.AgentWaiting)
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}

			for _, address := range []AgentAddress{oldTop, oldNested} {
				resolution, err := service.ResolveAgentAddress(context.Background(), runID, address.String())
				if err != nil {
					t.Fatal(err)
				}
				if resolution.Kind != AgentResolutionStale {
					t.Fatalf("%s resolution = %+v, want stale", address.String(), resolution)
				}
			}

			live, err := service.AddressableAgents(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			currentTop := agentAddressFor(runID, "implement", 1, "coder", secondSeq).String()
			currentNested := agentAddressFor(runID, "implement", 1, tt.secondAgent, secondSeq).String()
			for _, agent := range live {
				if agent.Address.String() == oldTop.String() || agent.Address.String() == oldNested.String() {
					t.Fatalf("stale address leaked into enumeration: %+v", live)
				}
			}
			if !containsAddress(live, currentTop) || !containsAddress(live, currentNested) {
				t.Fatalf("live addresses = %+v, want %q and %q", live, currentTop, currentNested)
			}
		})
	}
}

func TestReaderExposesAgentAddressOperations(t *testing.T) {
	service, layout, def := agentAddressService(t)
	var reader Reader = service
	runID := "run-reader"
	run, clock := createAgentAddressRun(t, layout, def, runID, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	startedSeq := appendAgenticStageStart(t, run, clock, 1)
	appendAgentLifecycle(t, run, clock, runID, 1, "worker-1", journal.AgentWaiting)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	agents, err := reader.AddressableAgents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("addressable agents = %+v, want top-level and nested agents", agents)
	}

	address := agentAddressFor(runID, "implement", 1, "worker-1", startedSeq)
	resolution, err := reader.ResolveAgentAddress(context.Background(), runID, address.String())
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != AgentResolutionResolved || resolution.Agent == nil || resolution.Agent.Kind != AgentAddressNested {
		t.Fatalf("interface resolution = %+v", resolution)
	}
}

func containsAddress(agents []AddressableAgent, want string) bool {
	for _, agent := range agents {
		if agent.Address.String() == want {
			return true
		}
	}
	return false
}
