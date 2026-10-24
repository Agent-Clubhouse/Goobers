//go:build cloudintegration

package journalequivalence

import (
	"context"
	"io"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginedecision"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workflow"
)

// Compare the complete keyed operator intervention and resumed segment. The
// local service and cloud router both use the production delivery paths; the
// PG journal runs as goobers_controller, checked by cloudScenario.
func compareCloudTerminalIntervention(t *testing.T, scenario corpusScenario) {
	local := runScenario(t, scenario)
	if canonicalDigest(t, local) != localBaselineDigests[scenario.name] {
		t.Fatal("local fixture drifted")
	}
	initial := scenario
	initial.expect = nil
	initial.run = func(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, _ string, runID string) runner.Result {
		result, err := r.Start(ctx, startInput(runID, machine))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	localBefore := runScenario(t, initial)
	backend, _, activities, input := cloudScenario(t, scenario)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	}()
	host := worker.New(server.Client(), "corpus-terminal", worker.Options{})
	engine.RegisterWith(host, activities)
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	defer host.Stop()
	input.HITL.WaitSeconds = 120
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: input.RunID, TaskQueue: "corpus-terminal"}, engine.Run, input)
	if err != nil {
		t.Fatal(err)
	}
	router := enginedecision.Router{Client: server.Client()}
	state := waitCorpusTerminal(t, ctx, router, input.RunID)
	reader, err := backend.OpenRun(ctx, journal.RunRef{ID: scenario.runID})
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalEqual(t, localBefore, canonicalReader(t, initial, reader))
	intent := engine.HITLIntent{Protocol: engine.HITLProtocol, Version: engine.HITLProtocolVersion, RunID: scenario.runID, RequestID: "fixture", Actor: "operator:fixture", ExpectedTerminalGeneration: state.TerminalGeneration}
	if scenario.name == "gate-fail-rerun" {
		intent.Kind, intent.Stage, intent.InstructionAddendum = engine.HITLRerunStage, "review", "accept the deterministic fixture"
	} else {
		// Service.Override routes this exact intent through deliverTerminalIntent.
		intent.Kind, intent.Gate, intent.Resolution = engine.HITLResolveEscalation, "review", "override"
		intent.Rationale = "operator reviewed deterministic fixture"
	}
	ack, err := router.Intend(ctx, intent)
	if err != nil || !ack.Resumed {
		t.Fatalf("terminal intervention: ack=%+v err=%v", ack, err)
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	reader, err = backend.OpenRun(ctx, journal.RunRef{ID: scenario.runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCloudEmitKeys(events, scenario.runID); err != nil {
		t.Fatal(err)
	}
	assertCanonicalEqual(t, local, canonicalReader(t, scenario, reader))
}

func waitCorpusTerminal(t *testing.T, ctx context.Context, router enginedecision.Router, runID string) engine.HITLState {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := router.HITLState(ctx, runID)
		if err == nil && state.Phase == "awaiting-operator" {
			return state
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for terminal hold: %v (last query %v)", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}
