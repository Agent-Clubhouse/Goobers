package engine

import (
	"fmt"
	"testing"
)

func TestTerminalAcknowledgementQueryWindowIsBoundedAndEvictionIsExplicit(t *testing.T) {
	session := &hitlSession{liveTerminal: true, acks: map[string]HITLAck{}, generation: 1000}
	for i := 0; i < 1000; i++ {
		session.rememberTerminalAck(fmt.Sprintf("request-%d", i), HITLAck{TerminalGeneration: uint64(i + 1)})
	}
	state, err := session.state()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AcknowledgedGenerations) != HITLAcknowledgementWindow || len(session.recentAcks) != HITLAcknowledgementWindow || cap(session.recentAcks) > HITLAcknowledgementWindow*2 {
		t.Fatalf("window=%d index=%d capacity=%d", len(state.AcknowledgedGenerations), len(session.recentAcks), cap(session.recentAcks))
	}
	for _, tc := range []struct {
		id             string
		known, expired bool
		generation     uint64
	}{
		{"request-0", true, true, 0}, {"request-935", true, true, 0}, {"request-936", true, false, 937}, {"request-999", true, false, 1000}, {"new-request", false, false, 0},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got, err := session.requestState(tc.id)
			if err != nil || got.RequestKnown != tc.known || got.RequestExpired != tc.expired || got.RequestGeneration != tc.generation {
				t.Fatalf("lookup=%+v err=%v", got, err)
			}
			if len(got.AcknowledgedGenerations) > HITLAcknowledgementWindow {
				t.Fatal("request lookup exposed unbounded ledger")
			}
		})
	}
	// Neither a query caller's mutation nor a repeated acknowledgement grows or
	// moves the bounded index, or reintroduces an evicted request generation.
	state.AcknowledgedGenerations["request-0"] = 1
	session.rememberTerminalAck("request-0", session.acks["request-0"])
	got, _ := session.requestState("request-0")
	if !got.RequestExpired || got.RequestGeneration != 0 {
		t.Fatalf("evicted request reintroduced: %+v", got)
	}
}
