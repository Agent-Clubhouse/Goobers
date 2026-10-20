//go:build !windows

package interactiveexec

import (
	"context"
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
	failedInterruptSession
	active atomic.Uint64
	sends  atomic.Int32
}

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
	return chat.Prompt, nil
}

func TestInterruptIgnoresPreviousBufferedIdle(t *testing.T) {
	s := &boundarySession{failedInterruptSession: failedInterruptSession{supported: true, events: make(chan chat.Event, 3), interrupt: func(context.Context) error { return nil }}}
	s.active.Store(2)
	s.events <- chat.Event{Type: "turn_ended", Data: map[string]any{"nativeTurnId": "1", "settled": true}}
	s.events <- chat.Event{Type: "turn_started", Data: map[string]any{"nativeTurnId": "2"}}
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
	s.events <- chat.Event{Type: "turn_ended", Data: map[string]any{"nativeTurnId": "2", "settled": true}}
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
	s.interrupt = func(context.Context) error {
		s.active.Store(3)
		s.events <- chat.Event{Type: "turn_ended", Data: map[string]any{"nativeTurnId": "2", "settled": true}}
		return nil
	}
	var reason string
	result, err := interruptAndContinue(t.Context(), s, interactive.Steering{Text: "replacement"}, harness.RunRequest{}, func(chat.Event) error { return nil }, func(_ string, data map[string]any) error { reason, _ = data["reason"].(string); return nil }, 20*time.Millisecond)
	if err != nil || result.restart == nil || !s.closed || reason != "timeout" || s.sends.Load() != 0 {
		t.Fatalf("later turn bypassed custody: result=%+v err=%v reason=%s sends=%d", result, err, reason, s.sends.Load())
	}
}
