package copilotsdk

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/goobers/goobers/internal/chat"
)

func TestAbortDoesNotSettleBeforeRootIdle(t *testing.T) {
	s0, err := testDriver(&fakeNative{}).Start(context.Background(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s0.Close() }()
	s := s0.(*session)
	if _, err := s.Send(t.Context(), chat.Text("first"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// Abort describes why a turn was aborted; only root session.idle guarantees
	// there are no background agents or attached shell commands in flight.
	s.onEvent(sdk.SessionEvent{Data: &rpc.AbortData{}})
	if _, err := s.Send(t.Context(), chat.Text("second"), chat.SendOptions{}); !errors.Is(err, chat.ErrBusy) {
		t.Fatalf("abort admitted a new prompt before root idle: %v", err)
	}
}

func TestQueuedDeliveryWaitsForFollowingRootIdle(t *testing.T) {
	s0, err := testDriver(&fakeNative{}).Start(context.Background(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s0.Close() }()
	s := s0.(*session)
	if _, err := s.Send(t.Context(), chat.Text("first"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if actual, err := s.Send(t.Context(), chat.Text("second"), chat.SendOptions{Delivery: chat.Queued}); err != nil || actual != chat.Queued {
		t.Fatal(actual, err)
	}
	s.onEvent(sdk.SessionEvent{Data: &rpc.SessionIdleData{}})
	if e := next(t, s, "turn_ended"); e.Data["settled"] != false {
		t.Fatalf("queued work truncated: %+v", e.Data)
	}
	if _, err := s.Send(t.Context(), chat.Text("third"), chat.SendOptions{}); !errors.Is(err, chat.ErrBusy) {
		t.Fatalf("queue gap admitted new prompt: %v", err)
	}
	s.onEvent(sdk.SessionEvent{Data: &rpc.AssistantTurnStartData{}})
	s.onEvent(sdk.SessionEvent{Data: &rpc.SessionIdleData{}})
	if e := next(t, s, "turn_ended"); e.Data["settled"] != true {
		t.Fatalf("queued work not settled: %+v", e.Data)
	}
}
