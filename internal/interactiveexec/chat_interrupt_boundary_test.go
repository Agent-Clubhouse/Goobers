//go:build !windows

package interactiveexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
)

type boundarySession struct {
	afterSend  func()
	generation atomic.Uint64
	sequence   atomic.Uint64
	failedInterruptSession
	active atomic.Uint64
	sends  atomic.Int32
}

func (s *boundarySession) TurnGeneration() uint64 { return s.generation.Load() }
func (s *boundarySession) EventSequence() uint64  { return s.sequence.Load() }
func (s *boundarySession) ActiveTurnID() string {
	if turn := s.active.Load(); turn != 0 {
		return strconv.FormatUint(turn, 10)
	}
	return ""
}
func (s *boundarySession) Send(context.Context, []chat.ContentBlock, chat.SendOptions) (chat.Delivery, error) {
	s.sends.Add(1)
	if s.active.Load() != 0 {
		return "", chat.ErrBusy
	}
	if s.afterSend != nil {
		s.afterSend()
	}
	return chat.Prompt, nil
}

func TestInterruptIgnoresPreviousBufferedIdle(t *testing.T) {
	s := &boundarySession{failedInterruptSession: failedInterruptSession{supported: true, events: make(chan chat.Event, 3), interrupt: func(context.Context) error { return nil }}}
	s.active.Store(2)
	s.generation.Store(2)
	s.sequence.Store(2)
	s.events <- chat.Event{Seq: 1, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "1", "settled": true}}
	s.events <- chat.Event{Seq: 2, Type: "turn_started", Data: map[string]any{"nativeTurnId": "2"}}
	workspace := t.TempDir()
	completion := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(completion, []byte("prior completion"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	observed := make(chan struct{})
	go func() {
		_, err := interruptAndContinue(t.Context(), s, interactive.Steering{Text: "replacement"}, harness.RunRequest{Workspace: workspace, CompletionPath: "result.json"}, func(e chat.Event) error {
			if e.Type == "turn_started" {
				close(observed)
			}
			return nil
		}, func(string, map[string]any) error { return nil }, time.Second)
		done <- err
	}()
	select {
	case <-observed:
	case err := <-done:
		t.Fatalf("previous idle released interruption before current start: %v", err)
	case <-time.After(time.Second):
		t.Fatal("current start was not observed")
	}
	select {
	case err := <-done:
		t.Fatalf("previous idle released interruption: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if s.sends.Load() != 0 {
		t.Fatal("replacement sent before current idle")
	}
	if _, err := os.Stat(completion); err != nil {
		t.Fatalf("completion cleared before current idle: %v", err)
	}
	s.active.Store(0)
	s.sequence.Store(3)
	s.events <- chat.Event{Seq: 3, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "2", "settled": true}}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.sends.Load() != 1 {
		t.Fatal("replacement not sent after current idle")
	}
	if _, err := os.Stat(completion); !os.IsNotExist(err) {
		t.Fatalf("completion not cleared after current idle: %v", err)
	}
}

func TestInterruptMatchingIdleCannotReleaseLaterActiveTurn(t *testing.T) {
	s := &boundarySession{failedInterruptSession: failedInterruptSession{supported: true, events: make(chan chat.Event, 1)}}
	s.active.Store(2)
	s.generation.Store(2)
	s.interrupt = func(context.Context) error {
		s.active.Store(3)
		s.generation.Store(3)
		s.events <- chat.Event{Type: "turn_ended", Data: map[string]any{"nativeTurnId": "2", "settled": true}}
		return nil
	}
	var reason string
	result, err := interruptAndContinue(t.Context(), s, interactive.Steering{Text: "replacement"}, harness.RunRequest{}, func(chat.Event) error { return nil }, func(_ string, data map[string]any) error { reason, _ = data["reason"].(string); return nil }, 20*time.Millisecond)
	if err != nil || result.restart == nil || !s.closed || reason != "failed" || s.sends.Load() != 0 {
		t.Fatalf("later turn bypassed custody: result=%+v err=%v reason=%s sends=%d", result, err, reason, s.sends.Load())
	}
}

func TestInterruptRestartsWhenAnotherTurnStartsAndSettlesDuringAbort(t *testing.T) {
	s := &boundarySession{failedInterruptSession: failedInterruptSession{supported: true, events: make(chan chat.Event, 3)}}
	s.active.Store(1)
	s.generation.Store(1)
	s.interrupt = func(context.Context) error {
		s.events <- chat.Event{Seq: 1, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "1", "settled": true}}
		s.generation.Store(2)
		s.events <- chat.Event{Seq: 2, Type: "turn_started", Data: map[string]any{"nativeTurnId": "2"}}
		s.active.Store(0)
		s.events <- chat.Event{Seq: 3, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "2", "settled": true}}
		s.sequence.Store(3)
		return nil
	}
	result, err := interruptAndContinue(t.Context(), s, interactive.Steering{Text: "replacement"}, harness.RunRequest{}, func(chat.Event) error { return nil }, func(string, map[string]any) error { return nil }, time.Second)
	if err != nil || result.restart == nil || !s.closed || s.sends.Load() != 0 {
		t.Fatalf("rollover admitted replacement: %+v %v sends=%d", result, err, s.sends.Load())
	}
}

func TestInterruptRejectsUnidentifiedBoundaryDuringAbort(t *testing.T) {
	s := &boundarySession{failedInterruptSession: failedInterruptSession{supported: true, events: make(chan chat.Event, 1)}}
	s.active.Store(1)
	s.generation.Store(1)
	s.interrupt = func(context.Context) error {
		s.active.Store(0)
		s.sequence.Store(1)
		s.events <- chat.Event{Seq: 1, Type: "turn_ended", Data: map[string]any{"settled": true}}
		return nil
	}
	result, err := interruptAndContinue(t.Context(), s, interactive.Steering{Text: "replacement"}, harness.RunRequest{}, func(chat.Event) error { return nil }, func(string, map[string]any) error { return nil }, time.Second)
	if err != nil || result.restart == nil || !s.closed || s.sends.Load() != 0 {
		t.Fatalf("unidentified boundary admitted replacement: %+v %v", result, err)
	}
}

func TestReplacementDiscardsTerminalEventsBeforeInterruptBoundary(t *testing.T) {
	s := &boundarySession{failedInterruptSession: failedInterruptSession{supported: true, events: make(chan chat.Event, 5)}}
	s.active.Store(1)
	s.generation.Store(1)
	s.interrupt = func(context.Context) error {
		s.active.Store(0)
		s.sequence.Store(10)
		s.events <- chat.Event{Seq: 10, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "1", "settled": true}}
		return nil
	}
	s.afterSend = func() {
		s.events <- chat.Event{Seq: 9, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "1", "settled": true, "reason": "end_turn"}}
		s.events <- chat.Event{Seq: 11, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "1", "settled": true, "reason": "end_turn"}}
		s.events <- chat.Event{Seq: 12, Type: "completion.accepted", Data: map[string]any{"payload": json.RawMessage(`{"status":"success"}`)}}
		s.events <- chat.Event{Seq: 13, Type: "turn_ended", Data: map[string]any{"nativeTurnId": "2", "settled": true, "reason": "end_turn"}}
	}
	steering := make(chan interactive.Steering, 1)
	steering <- interactive.Steering{Text: "replacement", SteeringMode: "interrupt-and-continue"}
	request := interactive.ExecutionRequest{Steering: steering}
	out, restart, err := driveChatEvents(t.Context(), t.Context(), request, harness.RunRequest{}, s, newDriverControls(s, time.Second), func(chat.Event) error { return nil }, func(string, map[string]any) error { return nil })
	if err != nil || restart != nil || string(out.Payload) != `{"status":"success"}` {
		t.Fatalf("old terminal ended replacement: %s %v %v", out.Payload, restart, err)
	}
}
