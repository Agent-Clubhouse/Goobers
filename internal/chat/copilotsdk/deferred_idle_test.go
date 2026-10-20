package copilotsdk

import (
	"context"
	"errors"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/chat"
)

func TestDeferredPreviousIdlePreservesNewTurnPermission(t *testing.T) {
	s0, err := testDriver(&fakeNative{}).Start(t.Context(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s0.Close() }()
	s := s0.(*session)
	if _, err := s.Send(t.Context(), chat.Text("first"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	decision := make(chan rpc.PermissionDecision, 1)
	requestID := ""
	s.native = acceptQueuedAtIdle{nativeSession: s.native, onSend: func() {
		s.onEvent(sdk.SessionEvent{Data: &rpc.SessionIdleData{}})
		s.onEvent(sdk.SessionEvent{Data: &rpc.AssistantTurnStartData{}})
		go func() { d, _ := s.permission(&rpc.PermissionRequestWrite{}, sdk.PermissionInvocation{}); decision <- d }()
		requestID = next(t, s, "permission_request").Data["requestId"].(string)
	}}
	if _, err := s.Send(context.Background(), chat.Text("queued"), chat.SendOptions{Delivery: chat.Queued}); err != nil {
		t.Fatal(err)
	}
	if err := s.RespondPermission(t.Context(), requestID, chat.PermissionResponse{OptionID: "allow_once"}); err != nil {
		t.Fatalf("previous idle invalidated next turn's permission after enqueue acknowledgement: %v", err)
	}
	select {
	case d := <-decision:
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); !ok {
			t.Fatalf("next turn approval replaced by %T", d)
		}
	case <-time.After(time.Second):
		t.Fatal("next turn approval blocked")
	}
}

func TestDeferredPreviousIdleCannotSettleNewTurnAbort(t *testing.T) {
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
		s.onEvent(sdk.SessionEvent{Data: &rpc.AssistantTurnStartData{}})
		s.onEvent(sdk.SessionEvent{Data: &rpc.AbortData{}})
	}}
	if _, err := s.Send(t.Context(), chat.Text("queued"), chat.SendOptions{Delivery: chat.Queued}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(t.Context(), chat.Text("third"), chat.SendOptions{}); !errors.Is(err, chat.ErrBusy) {
		t.Fatalf("previous idle settled the next turn's abort before its root idle: %v", err)
	}
}
