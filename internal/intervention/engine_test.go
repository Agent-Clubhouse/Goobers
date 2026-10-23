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
func (e *recordingEngine) HITLState(context.Context, string, ...string) (engine.HITLState, error) {
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

// N-1 histories have no fenced terminal capability and must be refused before
// any Update is sent.
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

func TestTerminalInterventionsRequireCapabilityAndDurableAcknowledgement(t *testing.T) {
	cases := map[string]func(*Service, httpapi.InterventionRequest) (httpapi.InterventionResult, error){
		"approve": func(s *Service, r httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
			return s.Approve(t.Context(), r)
		},
		"override": func(s *Service, r httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
			return s.Override(t.Context(), r)
		},
		"rerun": func(s *Service, r httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
			return s.RerunStage(t.Context(), r)
		},
		"deny": func(s *Service, r httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
			return s.AcceptDenyEscalation(t.Context(), t.Context(), r)
		},
	}
	for action, call := range cases {
		t.Run(action, func(t *testing.T) {
			decisions := &recordingEngine{hitl: engine.HITLState{LiveTerminalInterventions: true, Enabled: true, TerminalGeneration: 3}}
			service := engineRunService(t, decisions)
			request := httpapi.InterventionRequest{RunID: "engine-run", Stage: "review", Actor: "alice", IdempotencyKey: "key-1", Rationale: "reviewed", InstructionAddendum: "retry"}
			if _, err := call(service, request); err != nil {
				t.Fatal(err)
			}
			if len(decisions.intents) != 1 {
				t.Fatalf("intents=%+v", decisions.intents)
			}
			intent := decisions.intents[0]
			if intent.ExpectedTerminalGeneration != 3 || intent.RequestID != "key-1" || intent.Actor != "alice" || intent.Decision != "" {
				t.Fatalf("intent lost raw identity: %+v", intent)
			}
			if action == "rerun" {
				if intent.Kind != engine.HITLRerunStage || intent.Stage != "review" {
					t.Fatalf("rerun=%+v", intent)
				}
			} else if intent.Resolution != action || intent.Gate != "review" {
				t.Fatalf("resolution=%+v", intent)
			}
			decisions.refusal = errors.New("journal delivery is uncertain")
			result, err := call(service, request)
			var failure *httpapi.InterventionError
			if !errors.As(err, &failure) || failure.Status != http.StatusBadGateway || result.JournalSeq != 0 {
				t.Fatalf("uncertain delivery acknowledged: %+v %v", result, err)
			}
		})
	}
}

func TestTerminalHTTPRetryKeepsOriginalGenerationAndNeverDecidesALaterGate(t *testing.T) {
	decisions := &recordingEngine{hitl: engine.HITLState{LiveTerminalInterventions: true, TerminalGeneration: 1}}
	service := engineRunService(t, decisions)
	request := httpapi.InterventionRequest{RunID: "engine-run", Stage: "review", Actor: "alice", IdempotencyKey: "original-approval", Decision: "pass"}
	if _, err := service.Approve(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	original := decisions.intents[0]
	decisions.hitl.TerminalGeneration = 7
	decisions.hitl.RequestKnown = true
	decisions.hitl.RequestGeneration = 1
	decisions.gate = engine.GateDecisionState{Pending: true, Gate: "review", GateOccurrence: 99, TerminalGeneration: 7}
	if _, err := service.Approve(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(decisions.gates) != 0 || len(decisions.intents) != 2 || engine.HITLIntentUpdateID(decisions.intents[1]) != engine.HITLIntentUpdateID(original) {
		t.Fatalf("retry rebound: gates=%+v intents=%+v", decisions.gates, decisions.intents)
	}
	decisions.hitl.RequestExpired = true
	decisions.hitl.RequestGeneration = 0
	_, err := service.Approve(t.Context(), request)
	var refusal *httpapi.InterventionError
	if !errors.As(err, &refusal) || refusal.Code != engine.HITLErrGeneration || len(decisions.intents) != 2 || len(decisions.gates) != 0 {
		t.Fatalf("evicted key sent a new Update: err=%v intents=%+v gates=%+v", err, decisions.intents, decisions.gates)
	}
}
