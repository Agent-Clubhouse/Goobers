package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestGateDecisionValidatorBindsOccurrenceAndGeneration(t *testing.T) {
	m := gateDecisionMachine(t)
	in := RunInput{
		RunID: "gate-run",
		HITL:  &HITLPolicy{Enabled: true, Actors: []string{"operator"}},
	}
	generation := uint64(2)
	session := newGateDecisionSession(in, m, func() uint64 { return generation })
	g, _ := m.Gate("approval")
	if err := session.activate(g, 7); err != nil {
		t.Fatal(err)
	}
	valid := gateDecisionRequest()
	if err := session.validate(valid); err != nil {
		t.Fatalf("valid decision refused: %v", err)
	}

	tests := []struct {
		name string
		edit func(*GateDecisionRequest)
		code string
	}{
		{"protocol", func(r *GateDecisionRequest) { r.Version++ }, GateDecisionErrProtocol},
		{"run", func(r *GateDecisionRequest) { r.RunID = "other" }, GateDecisionErrRunMismatch},
		{"request id", func(r *GateDecisionRequest) { r.RequestID = "" }, GateDecisionErrInvalid},
		{"actor", func(r *GateDecisionRequest) { r.Actor = "" }, GateDecisionErrInvalid},
		{"unauthorized", func(r *GateDecisionRequest) { r.Actor = "intruder" }, GateDecisionErrUnauthorized},
		{"gate", func(r *GateDecisionRequest) { r.Gate = "other" }, GateDecisionErrOccurrence},
		{"occurrence", func(r *GateDecisionRequest) { r.GateOccurrence = 8 }, GateDecisionErrOccurrence},
		{"generation", func(r *GateDecisionRequest) { r.ExpectedTerminalGeneration = 1 }, GateDecisionErrGeneration},
		{"decision", func(r *GateDecisionRequest) { r.Decision = "unknown" }, GateDecisionErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.edit(&request)
			err := session.validate(request)
			code, _, ok := gateDecisionRefusalCode(err)
			if !ok || code != test.code {
				t.Fatalf("error=%v code=%q, want %q", err, code, test.code)
			}
			if session.active == nil || session.active.decision != nil {
				t.Fatal("rejected decision mutated the active occurrence")
			}
		})
	}
}

func TestGateDecisionValidatorRequiresEnabledPendingHumanGate(t *testing.T) {
	m := gateDecisionMachine(t)
	request := gateDecisionRequest()
	for _, test := range []struct {
		name    string
		session *gateDecisionSession
		code    string
	}{
		{
			name: "disabled",
			session: newGateDecisionSession(
				RunInput{RunID: "gate-run"}, m, nil,
			),
			code: GateDecisionErrNotEnabled,
		},
		{
			name: "not pending",
			session: newGateDecisionSession(
				RunInput{RunID: "gate-run", HITL: &HITLPolicy{Enabled: true}},
				m, func() uint64 { return 2 },
			),
			code: GateDecisionErrNotPending,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.session.validate(request)
			code, _, ok := gateDecisionRefusalCode(err)
			if !ok || code != test.code {
				t.Fatalf("error=%v code=%q, want %q", err, code, test.code)
			}
		})
	}

	session := newGateDecisionSession(
		RunInput{RunID: "gate-run", HITL: &HITLPolicy{Enabled: true}},
		m, nil,
	)
	automated, _ := m.Gate("automated")
	if err := session.activate(automated, 9); err == nil {
		t.Fatal("activated a non-human gate")
	}
	human, _ := m.Gate("approval")
	if err := session.activate(human, 0); err == nil {
		t.Fatal("activated an uncommitted gate occurrence")
	}
}

func TestGateDecisionDedupByUpdateID(t *testing.T) {
	session := newGateDecisionSession(
		RunInput{RunID: "gate-run", HITL: &HITLPolicy{Enabled: true}},
		gateDecisionMachine(t),
		nil,
	)
	request := gateDecisionRequest()
	fingerprint := gateDecisionFingerprint(request)
	session.fingerprints[request.RequestID] = fingerprint
	session.acks[request.RequestID] = GateDecisionAck{
		Protocol: GateDecisionProtocol, Version: GateDecisionProtocolVersion,
		RunID: request.RunID, RequestID: request.RequestID, Gate: request.Gate,
		GateOccurrence: request.GateOccurrence, Decision: request.Decision,
	}

	ack, err, done := session.replay(request.RequestID, fingerprint)
	if err != nil || !done || !ack.Duplicate {
		t.Fatalf("exact replay=(%+v, %v, %v), want duplicate ack", ack, err, done)
	}
	changed := request
	changed.Decision = "reject"
	_, err, done = session.replay(request.RequestID, gateDecisionFingerprint(changed))
	code, _, ok := gateDecisionRefusalCode(err)
	if !done || !ok || code != GateDecisionErrKeyReused {
		t.Fatalf("changed replay=(%v, %v, %q), want key-reused refusal", err, done, code)
	}
}

func TestGateDecisionUpdateCompletesAfterWalkConsumesDecision(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	request := gateDecisionRequest()
	var ack GateDecisionAck
	var updateErr error
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(
			GateDecisionUpdateName,
			request.RequestID,
			&testsuite.TestUpdateCallback{
				OnReject: func(err error) { updateErr = err },
				OnComplete: func(value interface{}, err error) {
					updateErr = err
					if err != nil || value == nil {
						return
					}
					switch result := value.(type) {
					case converter.EncodedValue:
						updateErr = result.Get(&ack)
					case GateDecisionAck:
						ack = result
					default:
						updateErr = errors.New("unexpected gate-decision update result")
					}
				},
			},
			request,
		)
	}, time.Millisecond)
	env.ExecuteWorkflow(func(ctx workflow.Context) (string, error) {
		m := gateDecisionMachine(t)
		session := newGateDecisionSession(
			RunInput{
				RunID: "gate-run",
				HITL:  &HITLPolicy{Enabled: true, WaitSeconds: 60, Actors: []string{"operator"}},
			},
			m,
			func() uint64 { return 2 },
		)
		if err := session.register(ctx); err != nil {
			return "", err
		}
		g, _ := m.Gate("approval")
		if err := session.activate(g, 7); err != nil {
			return "", err
		}
		decision, err := session.await(ctx)
		return decision.decision, err
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err := env.GetWorkflowResult(&decision); err != nil {
		t.Fatal(err)
	}
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	if decision != "pass" || ack.RequestID != request.RequestID ||
		ack.GateOccurrence != request.GateOccurrence || ack.TerminalGeneration != 2 {
		t.Fatalf("decision=%q ack=%+v", decision, ack)
	}
}

func TestHumanGateWindowExpiryNamesCommittedOccurrence(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		m := gateDecisionMachine(t)
		session := newGateDecisionSession(
			RunInput{
				RunID: "gate-run",
				HITL:  &HITLPolicy{Enabled: true, WaitSeconds: 1},
			},
			m,
			nil,
		)
		g, _ := m.Gate("approval")
		if err := session.activate(g, 17); err != nil {
			return err
		}
		_, err := session.await(ctx)
		return err
	})
	err := env.GetWorkflowError()
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != HumanGateWindowExpiredCode {
		t.Fatalf("expiry error=%v, want %s", err, HumanGateWindowExpiredCode)
	}
	for _, want := range []string{"approval", "17"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expiry error %q does not name %q", err, want)
		}
	}
}

func gateDecisionRequest() GateDecisionRequest {
	return GateDecisionRequest{
		Protocol: GateDecisionProtocol, Version: GateDecisionProtocolVersion,
		RunID: "gate-run", RequestID: "decision-1", Actor: "operator",
		Gate: "approval", GateOccurrence: 7, ExpectedTerminalGeneration: 2,
		Decision: "pass",
	}
}

func gateDecisionMachine(t *testing.T) *wf.Machine {
	t.Helper()
	m, err := wf.Compile(wf.Definition{
		Name: "human-gate-contract", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "web", Start: "approval",
			Gates: []apiv1.Gate{
				{
					Name: "approval", Evaluator: apiv1.EvaluatorHuman,
					Human: &apiv1.HumanGate{Approvers: []string{"operator"}},
					Branches: map[string]string{
						"pass": "automated", "reject": wf.TargetAbort,
					},
				},
				{
					Name: "automated", Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "status-equals"},
					Branches: map[string]string{
						"pass": wf.TerminalComplete, "fail": wf.TargetAbort,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
