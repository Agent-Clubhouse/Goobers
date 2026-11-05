package engine

import (
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"
)

func TestOperatorRequestLedgerRefusesCrossProtocolAfterValidatorSnapshot(t *testing.T) {
	for _, first := range []string{HITLProtocol, GateDecisionProtocol} {
		t.Run(first, func(t *testing.T) {
			in := RunInput{RunID: "gate-run", HITL: &HITLPolicy{Enabled: true, Actors: []string{"operator"}}}
			m := gateDecisionMachine(t)
			hitl := newHITLSession(in, m, nil)
			hitl.liveTerminal = true
			hitl.phase = hitlPhaseAwaiting
			hitl.generation = 2
			gates := newGateDecisionSession(in, m, func() uint64 { return hitl.generation })
			shareOperatorRequestLedger(hitl, gates)
			g, _ := m.Gate("approval")
			if err := gates.activate(g, 7); err != nil {
				t.Fatal(err)
			}
			request := gateDecisionRequest()
			intent := HITLIntent{Protocol: HITLProtocol, Version: HITLProtocolVersion, RunID: in.RunID, RequestID: request.RequestID, Actor: "operator", ExpectedTerminalGeneration: 2, Kind: HITLResolveEscalation, Gate: "approval", Resolution: "approve", Decision: "pass"}
			if err := hitl.validate(intent); err != nil {
				t.Fatal(err)
			}
			if err := gates.validate(request); err != nil {
				t.Fatal(err)
			}
			// Both validators observed an unclaimed key. One handler now claims it
			// before yielding, as happens before journal delivery / gate consumption.
			// The validator sees that pending claim; the delayed handler sees its ack.
			var validateErr, handleErr error
			want := GateDecisionErrKeyReused
			if first == HITLProtocol {
				hitl.fingerprints[intent.RequestID] = operatorRequestFingerprint(true, HITLProtocol, hitlFingerprint(intent))
				validateErr = gates.validate(request)
				hitl.rememberTerminalAck(intent.RequestID, HITLAck{TerminalGeneration: 2})
				_, handleErr = gates.handle(nil, request)
			} else {
				gates.fingerprints[request.RequestID] = operatorRequestFingerprint(true, GateDecisionProtocol, gateDecisionFingerprint(request))
				validateErr = hitl.validate(intent)
				gates.acks[request.RequestID] = GateDecisionAck{TerminalGeneration: 2}
				_, handleErr = hitl.handle(nil, intent)
				want = HITLErrKeyReused
			}
			for name, err := range map[string]error{"validator": validateErr, "handler after stale validator": handleErr} {
				var refusal *temporal.ApplicationError
				if !errors.As(err, &refusal) || refusal.Type() != want {
					t.Fatalf("%s error=%v want=%s", name, err, want)
				}
			}
			if gates.active.decision != nil || hitl.pending != nil {
				t.Fatal("cross-protocol refusal published a decision")
			}
		})
	}
}
