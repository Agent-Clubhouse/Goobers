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

func TestInterruptedTurnCannotApproveOldPermission(t *testing.T) {
	driver := testDriver(&fakeNative{})
	session0, err := driver.Start(context.Background(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session0.Close() }()
	s := session0.(*session)
	if _, err := s.Send(context.Background(), chat.Text("edit requested file"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	decision := make(chan rpc.PermissionDecision, 1)
	go func() { d, _ := s.permission(&rpc.PermissionRequestWrite{}, sdk.PermissionInvocation{}); decision <- d }()
	request := next(t, s, "permission_request")
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	next(t, s, "turn_ended")
	_ = s.RespondPermission(context.Background(), request.Data["requestId"].(string), chat.PermissionResponse{OptionID: "allow_once"})
	select {
	case d := <-decision:
		if _, approved := d.(*rpc.PermissionDecisionApproveOnce); approved {
			t.Fatal("completed abort still allows approval of the cancelled turn's write request")
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt left the cancelled permission handler blocked")
	}
}

func TestIdleMustRepresentRootTurnCompletion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event sdk.SessionEvent
	}{
		{"autopilot continuation", sdk.SessionEvent{Data: &rpc.SessionIdleData{Mode: new(rpc.SessionModeAutopilot)}}},
		{"subagent idle", sdk.SessionEvent{AgentID: new("child-agent"), Data: &rpc.SessionIdleData{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session0, err := testDriver(&fakeNative{}).Start(context.Background(), options())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session0.Close() }()
			s := session0.(*session)
			if _, err := s.Send(context.Background(), chat.Text("initial"), chat.SendOptions{}); err != nil {
				t.Fatal(err)
			}
			s.onEvent(tc.event)
			s.mu.Lock()
			busy := s.busy
			s.mu.Unlock()
			if !busy {
				t.Fatal("non-terminal idle cleared root busy state and emitted turn_ended")
			}
		})
	}
}
func TestNativeAbortIdleStaysCancelled(t *testing.T) {
	session0, err := testDriver(&fakeNative{}).Start(context.Background(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session0.Close() }()
	s := session0.(*session)
	if _, err := s.Send(context.Background(), chat.Text("initial"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.onEvent(sdk.SessionEvent{Data: &rpc.SessionIdleData{Aborted: new(true)}})
	if e := next(t, s, "turn_ended"); e.Data["reason"] != "cancelled" {
		t.Fatalf("native aborted idle reported %v", e.Data)
	}
}

func TestTerminalBoundaryCancelsPendingQuestion(t *testing.T) {
	for _, terminal := range []rpc.SessionEventData{&rpc.SessionIdleData{}, &rpc.AbortData{}} {
		s0, err := testDriver(&fakeNative{}).Start(context.Background(), options())
		if err != nil {
			t.Fatal(err)
		}
		s := s0.(*session)
		if _, err := s.Send(t.Context(), chat.Text("work"), chat.SendOptions{}); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.question(sdk.UserInputRequest{Question: "Pick one", Choices: []string{"yes"}}, sdk.UserInputInvocation{})
			done <- err
		}()
		request := next(t, s, "question")
		s.onEvent(sdk.SessionEvent{Data: terminal})
		if err := s.AnswerQuestion(t.Context(), request.Data["requestId"].(string), chat.QuestionAnswers{"answer": {"yes"}}); !errors.Is(err, chat.ErrUnknownRequest) {
			t.Fatalf("ended turn accepted stale answer: %v", err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("ended turn answered a pending question")
			}
		case <-time.After(time.Second):
			t.Fatal("ended turn retained a pending question")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
