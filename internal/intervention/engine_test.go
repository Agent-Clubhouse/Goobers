package intervention

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// recordingEngine is the engine router: it records every delivery and
// answers with the workflow's committed state.
type recordingEngine struct {
	gate    engine.GateDecisionState
	hitl    engine.HITLState
	gates   []engine.GateDecisionRequest
	intents []engine.HITLIntent
	refusal error
}

func (e *recordingEngine) GateState(context.Context, string) (engine.GateDecisionState, error) {
	return e.gate, nil
}
func (e *recordingEngine) HITLState(context.Context, string) (engine.HITLState, error) {
	return e.hitl, nil
}
func (e *recordingEngine) DecideGate(_ context.Context, r engine.GateDecisionRequest) (engine.GateDecisionAck, error) {
	e.gates = append(e.gates, r)
	return engine.GateDecisionAck{}, e.refusal
}
func (e *recordingEngine) Intend(_ context.Context, i engine.HITLIntent) (engine.HITLAck, error) {
	e.intents = append(e.intents, i)
	return engine.HITLAck{}, e.refusal
}

// engineRunService retains one engine-driven run, as the live journal
// writer leaves it, and routes interventions to decisions when set.
func engineRunService(t *testing.T, decisions EngineDecisions) *Service {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{RunID: "engine-run", Workflow: "release", WorkflowVersion: 1,
		Gaggle: "example", Driver: journal.DriverEngine, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "approve-release"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	definitions := &testDefinitions{}
	definitions.Replace(Definitions{Runners: map[string]*runner.Runner{"example": nil}})
	return New(Config{Definitions: definitions.Snapshot, Runners: &testRunnerRegistry{owners: map[string]*runner.Runner{}}, LocateRun: locateTestRun(layout),
		Claims: noClaims{}, EngineDrivenRefusal: testEngineDrivenRefusal, EngineDecisions: decisions})
}

func TestApproveRoutesToEngineForEngineRun(t *testing.T) {
	decisions := &recordingEngine{gate: engine.GateDecisionState{Pending: true, Gate: "approve-release", GateOccurrence: 2, TerminalGeneration: 0}}
	service := engineRunService(t, decisions)
	result, err := service.Approve(t.Context(), httpapi.InterventionRequest{RunID: "engine-run", Stage: "approve-release", Actor: "alice", IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions.gates) != 1 || len(decisions.intents) != 0 {
		t.Fatalf("approve was not routed to the gate decision update: %+v %+v", decisions.gates, decisions.intents)
	}
	got := decisions.gates[0]
	want := engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: "engine-run", RequestID: "key-1",
		Actor: "alice", Gate: "approve-release", GateOccurrence: 2, Decision: "pass"}
	if got != want {
		t.Fatalf("decision request = %+v, want %+v", got, want)
	}
	if result.JournalSeq == 0 {
		t.Fatal("engine intervention reported no journal position")
	}
}

// Terminal interventions would reopen an engine run after run.finished, which
// its live journal refuses; they are refused by name, and nothing is sent.
func TestTerminalInterventionsOnEngineRunsAreRefused(t *testing.T) {
	cases := map[string]func(*Service) error{
		"approve past a terminal": func(s *Service) error {
			_, err := s.Approve(t.Context(), httpapi.InterventionRequest{RunID: "engine-run", Stage: "review", Actor: "alice", IdempotencyKey: "k", Decision: "pass"})
			return err
		},
		"override": func(s *Service) error {
			_, err := s.Override(t.Context(), httpapi.InterventionRequest{RunID: "engine-run", Stage: "review", Actor: "alice", IdempotencyKey: "k", Rationale: "safe"})
			return err
		},
		"rerun": func(s *Service) error {
			_, err := s.RerunStage(t.Context(), httpapi.InterventionRequest{RunID: "engine-run", Stage: "build", Actor: "alice", IdempotencyKey: "k", InstructionAddendum: "retry"})
			return err
		},
		"deny": func(s *Service) error {
			_, err := s.AcceptDenyEscalation(t.Context(), t.Context(), httpapi.InterventionRequest{RunID: "engine-run", Stage: "review", Actor: "alice", IdempotencyKey: "k", Rationale: "unsafe"})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			decisions := &recordingEngine{}
			err := call(engineRunService(t, decisions))
			var intervention *httpapi.InterventionError
			if !errors.As(err, &intervention) || intervention.Status != http.StatusConflict || intervention.Code != EngineTerminalInterventionUnsupported {
				t.Fatalf("terminal intervention on an engine run = %v", err)
			}
			if len(decisions.gates) != 0 || len(decisions.intents) != 0 {
				t.Fatalf("a refused intervention reached the engine: %+v %+v", decisions.gates, decisions.intents)
			}
		})
	}
}

func TestEngineRefusalKeepsItsCodeAndUnconfiguredEngineStillRefuses(t *testing.T) {
	decisions := &recordingEngine{gate: engine.GateDecisionState{Pending: true, Gate: "approve-release", GateOccurrence: 2},
		refusal: temporal.NewApplicationError("stale occurrence", engine.GateDecisionErrOccurrence)}
	_, err := engineRunService(t, decisions).Approve(t.Context(), httpapi.InterventionRequest{RunID: "engine-run", Stage: "approve-release", Actor: "alice", IdempotencyKey: "k"})
	var intervention *httpapi.InterventionError
	if !errors.As(err, &intervention) || intervention.Status != http.StatusConflict || intervention.Code != engine.GateDecisionErrOccurrence {
		t.Fatalf("engine refusal = %v", err)
	}
	_, err = engineRunService(t, nil).Approve(t.Context(), httpapi.InterventionRequest{RunID: "engine-run", Stage: "approve-release", Actor: "alice", IdempotencyKey: "k"})
	if !errors.As(err, &intervention) || intervention.Code != "run_engine_driven" {
		t.Fatalf("engine run without a router = %v", err)
	}
}
