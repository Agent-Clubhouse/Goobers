package copilotsdk

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/chat"
)

type rejectQueuedAtIdle struct {
	nativeSession
	onSend func()
}

func (n rejectQueuedAtIdle) Send(context.Context, sdk.MessageOptions) (string, error) {
	n.onSend()
	return "", errors.New("queue rejected")
}
func TestRejectedQueueAfterIdleDoesNotStrandSession(t *testing.T) {
	s0, err := testDriver(&fakeNative{}).Start(t.Context(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s0.Close() }()
	s := s0.(*session)
	if _, err := s.Send(t.Context(), chat.Text("first"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	native := s.native
	s.native = rejectQueuedAtIdle{nativeSession: native, onSend: func() {
		s.onEvent(sdk.SessionEvent{Data: &rpc.SessionIdleData{}})
	}}
	if _, err := s.Send(t.Context(), chat.Text("rejected"), chat.SendOptions{Delivery: chat.Queued}); err == nil {
		t.Fatal("fixture must reject queue")
	}
	s.native = native
	if _, err := s.Send(t.Context(), chat.Text("next"), chat.SendOptions{}); err != nil {
		t.Fatalf("settled current turn plus rejected queue stranded session: %v", err)
	}
}

type acceptQueuedAtIdle struct {
	nativeSession
	onSend func()
}

func (n acceptQueuedAtIdle) Send(context.Context, sdk.MessageOptions) (string, error) {
	n.onSend()
	return "queued", nil
}

func TestAcceptedQueueAfterIdleRemainsPending(t *testing.T) {
	s0, err := testDriver(&fakeNative{}).Start(t.Context(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s0.Close() }()
	s := s0.(*session)
	if _, err := s.Send(t.Context(), chat.Text("first"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.native = acceptQueuedAtIdle{nativeSession: s.native, onSend: func() {
		s.onEvent(sdk.SessionEvent{Data: &rpc.SessionIdleData{}})
	}}
	if actual, err := s.Send(t.Context(), chat.Text("queued"), chat.SendOptions{Delivery: chat.Queued}); err != nil || actual != chat.Queued {
		t.Fatal(actual, err)
	}
	if e := next(t, s, "turn_ended"); e.Data["settled"] != false {
		t.Fatalf("accepted queue settled prematurely: %+v", e.Data)
	}
	s.onEvent(sdk.SessionEvent{Data: &rpc.SessionIdleData{}})
	if e := next(t, s, "turn_ended"); e.Data["settled"] != true {
		t.Fatalf("accepted queue did not settle: %+v", e.Data)
	}
}
