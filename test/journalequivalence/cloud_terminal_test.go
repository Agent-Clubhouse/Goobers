//go:build cloudintegration

package journalequivalence

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginedecision"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workflow"
)

// Compare the complete keyed operator intervention and resumed segment. The
// local and cloud sides both call Service, with the cloud Router bound to real
// Temporal. The retained status snapshot is read-only during intervention; the
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
	waitCorpusTerminal(t, ctx, router, input.RunID)
	reader, err := backend.OpenRun(ctx, journal.RunRef{ID: scenario.runID})
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalEqual(t, localBefore, canonicalReader(t, initial, reader))
	service := cloudCorpusInterventions(t, reader, router)
	request := httpapi.InterventionRequest{RunID: scenario.runID, Stage: "review", IdempotencyKey: "fixture", Actor: "operator:fixture"}
	if scenario.name == "gate-fail-rerun" {
		request.InstructionAddendum = "accept the deterministic fixture"
		_, err = service.RerunStage(ctx, request)
	} else {
		request.Rationale = "operator reviewed deterministic fixture"
		_, err = service.Override(ctx, request)
	}
	if err != nil {
		t.Fatalf("operator service refused terminal intervention: %v", err)
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

// Service resolves retained identity/status through its file-reader boundary.
// Snapshot those reads from PG, without substituting the snapshot for the PG
// journal owner: the real service must route all writes through EngineDecisions.
func cloudCorpusInterventions(t *testing.T, reader journal.RunReader, router enginedecision.Router) *intervention.Service {
	t.Helper()
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if !identity.EngineDriven() {
		t.Fatal("PG run is not engine-driven")
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != journal.EventRunStarted {
		t.Fatal("PG run lacks initial event")
	}
	root := t.TempDir()
	snapshot, err := journal.Create(root, identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	// Create has already written run.started. Preserve the remaining durable
	// order, including the terminal that the API replica currently observes.
	for _, event := range events[1:] {
		if err := snapshot.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, identity.RunID)
	t.Cleanup(func() {
		retained, err := journal.OpenRead(dir)
		if err != nil {
			t.Error(err)
			return
		}
		after, err := retained.Events()
		if err != nil {
			t.Error(err)
			return
		}
		if len(after) != len(events) {
			t.Errorf("operator mutated retained snapshot: %d events, want %d", len(after), len(events))
		}
	})
	return intervention.New(intervention.Config{
		Definitions: func() intervention.Definitions {
			return intervention.Definitions{Runners: map[string]*runner.Runner{identity.Gaggle: nil}}
		},
		LocateRun: func(_ []string, runID string, _ bool) (string, string, error) {
			if runID != identity.RunID {
				return "", "", fmt.Errorf("unknown retained run %q", runID)
			}
			return dir, identity.Gaggle, nil
		},
		EngineDecisions: router,
	})
}
