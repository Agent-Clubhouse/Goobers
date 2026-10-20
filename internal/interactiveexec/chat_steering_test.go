//go:build !windows

package interactiveexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
)

type interruptDriver struct {
	completionPath string
	chattest.FakeDriver
	starts, sends, interrupts int
	replies                   int
	pendingRequests           bool
	closed                    bool
	stop                      context.CancelFunc
}

func (d *interruptDriver) Start(ctx context.Context, options chat.StartOptions) (chat.Session, error) {
	d.starts++
	session, err := d.FakeDriver.Start(ctx, options)
	return &interruptSession{Session: session, driver: d}, err
}

type interruptSession struct {
	chat.Session
	driver *interruptDriver
}

func (s *interruptSession) Send(ctx context.Context, content []chat.ContentBlock, options chat.SendOptions) (chat.Delivery, error) {
	d := s.driver
	if d.closed {
		return "", chat.ErrClosed
	}
	delivery, err := s.Session.Send(ctx, content, options)
	d.sends++
	if d.sends == 2 && d.completionPath != "" {
		if _, err := os.Stat(d.completionPath); !os.IsNotExist(err) {
			return "", fmt.Errorf("old completion still present: %w", err)
		}
	}
	if d.sends == 1 && d.pendingRequests {
		d.Session.Emit("permission_request", map[string]any{"requestId": "old-permission"})
		d.Session.Emit("question", map[string]any{"requestId": "old-question"})
	}
	if d.stop != nil {
		d.stop()
	}
	if d.sends == 2 && !d.pendingRequests {
		d.Session.Emit("completion.accepted", map[string]any{"payload": json.RawMessage(`{"status":"success","summary":"replacement"}`)})
		d.Session.Emit("turn_ended", map[string]any{"reason": "end_turn", "settled": true})
	}
	return delivery, err
}
func (s *interruptSession) Interrupt(context.Context) error {
	s.driver.interrupts++
	s.driver.Session.Emit("completion.accepted", map[string]any{"payload": json.RawMessage(`{"status":"success","summary":"old turn"}`)})
	s.driver.Session.Emit("turn_ended", map[string]any{"reason": "cancelled", "settled": true})
	return nil
}
func (s *interruptSession) Close() error { s.driver.closed = true; return s.Session.Close() }
func (s *interruptSession) RespondPermission(context.Context, string, chat.PermissionResponse) error {
	s.driver.replies++
	return nil
}
func (s *interruptSession) AnswerQuestion(context.Context, string, chat.QuestionAnswers) error {
	s.driver.replies++
	return nil
}

func TestInterruptContinuePreservesNativeSession(t *testing.T) {
	executor, request := bridgeExecutor(t)
	driver := &interruptDriver{FakeDriver: chattest.FakeDriver{Caps: chat.Capabilities{Interrupt: true}}, pendingRequests: true}
	executor.chatRegistry = func(string, harness.RunRequest) *chat.Registry {
		registry := chat.NewRegistry()
		if err := registry.Register("copilot", driver); err != nil {
			t.Fatal(err)
		}
		return registry
	}
	request.Harness = "copilot"
	steering := make(chan interactive.Steering, 1)
	controls := make(chan interactive.DriverControl, 2)
	request.Controls = controls
	staleReplies := 0
	actual := ""
	steering <- interactive.Steering{CommandID: "stop-send", Text: "replacement", Delivery: "prompt", SteeringMode: "interrupt-and-continue", AckDelivery: func(delivery string, err error) {
		if err != nil {
			t.Error(err)
		}
		actual = delivery
		for kind, id := range map[string]string{"permission.respond": "old-permission", "question.answer": "old-question"} {
			controls <- interactive.DriverControl{Command: interactive.Command{Type: kind, RequestID: id}, Ack: func(_ string, replyErr error) {
				if !errors.Is(replyErr, chat.ErrUnknownRequest) {
					t.Errorf("stale reply was not rejected: %v", replyErr)
				}
				staleReplies++
				if staleReplies == 2 {
					driver.Session.Emit("completion.accepted", map[string]any{"payload": json.RawMessage(`{"status":"success","summary":"replacement"}`)})
					driver.Session.Emit("turn_ended", map[string]any{"reason": "end_turn", "settled": true})
				}
			}}
		}
	}}
	request.Steering = steering
	workspace := t.TempDir()
	driver.completionPath = filepath.Join(workspace, "result.json")
	if err := os.WriteFile(driver.completionPath, []byte(`{"status":"success","summary":"stale artifact"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var path string
	var lineage []chat.Event
	out, restart, err := executor.driveChat(context.Background(), context.Background(), request, harness.RunRequest{Workspace: workspace, CompletionPath: "result.json", Envelope: apiv1.InvocationEnvelope{Goal: "first"}}, nil, func(_ string, data map[string]any) error {
		if value, ok := data["steeringPath"].(string); ok {
			path = value
		}
		if value, ok := data["chat"]; ok {
			bytes, _ := json.Marshal(value)
			var event chat.Event
			if err := json.Unmarshal(bytes, &event); err != nil {
				return err
			}
			lineage = append(lineage, event)
		}
		return nil
	})
	if err != nil || restart != nil || driver.starts != 1 || driver.sends != 2 || driver.interrupts != 1 || !driver.closed || actual != "prompt" || path != "native_interrupt" {
		t.Fatalf("session was not preserved: err=%v restart=%v driver=%+v actual=%s path=%s", err, restart, driver, actual, path)
	}
	if string(out.Payload) != `{"status":"success","summary":"replacement"}` {
		t.Fatalf("old turn completion survived: %s", out.Payload)
	}
	if staleReplies != 2 || driver.replies != 0 {
		t.Fatalf("stale requests reached driver: rejected=%d forwarded=%d", staleReplies, driver.replies)
	}
	if len(lineage) == 0 {
		t.Fatal("no native lineage events observed")
	}
	for _, event := range lineage {
		if event.SessionID != request.SessionID || event.TurnID != request.TurnID || event.Fence != request.Fence {
			t.Fatalf("lineage changed: %+v", event)
		}
	}
}

func TestHumanStopStillClosesChatSession(t *testing.T) {
	executor, request := bridgeExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver := &interruptDriver{FakeDriver: chattest.FakeDriver{Caps: chat.Capabilities{Interrupt: true}}, stop: cancel}
	executor.chatRegistry = func(string, harness.RunRequest) *chat.Registry {
		registry := chat.NewRegistry()
		if err := registry.Register("copilot", driver); err != nil {
			t.Fatal(err)
		}
		return registry
	}
	request.Harness = "copilot"
	_, restart, err := executor.driveChat(ctx, ctx, request, harness.RunRequest{Workspace: t.TempDir(), Envelope: apiv1.InvocationEnvelope{Goal: "first"}}, nil, func(string, map[string]any) error { return nil })
	if !errors.Is(err, context.Canceled) || restart != nil || !driver.closed || driver.interrupts != 0 || driver.sends != 1 {
		t.Fatalf("stop failed to close: %v %v %+v", err, restart, driver)
	}
}

func TestBridgeAttachedInterruptUsesRestartPath(t *testing.T) {
	executor, request := bridgeExecutor(t)
	driver := &interruptDriver{FakeDriver: chattest.FakeDriver{Caps: chat.Capabilities{Interrupt: true}}}
	executor.chatRegistry = func(string, harness.RunRequest) *chat.Registry {
		registry := chat.NewRegistry()
		if err := registry.Register("copilot", driver); err != nil {
			t.Fatal(err)
		}
		return registry
	}
	request.Harness = "copilot"
	steering := make(chan interactive.Steering, 1)
	acknowledged := false
	steering <- interactive.Steering{CommandID: "stop-send", Text: "replacement", Delivery: "prompt", SteeringMode: "interrupt-and-continue", AckDelivery: func(string, error) { acknowledged = true }}
	request.Steering = steering
	var note map[string]any
	_, restart, err := executor.driveChat(t.Context(), t.Context(), request, harness.RunRequest{
		Workspace:  t.TempDir(),
		Envelope:   apiv1.InvocationEnvelope{Goal: "first"},
		MCPServers: []apiv1.MCPServer{{Name: "goobers-interactive"}},
	}, nil, func(_ string, data map[string]any) error {
		if data["steeringPath"] != nil {
			note = data
		}
		return nil
	})
	if err != nil || restart == nil || !driver.closed || driver.interrupts != 0 || driver.sends != 1 || acknowledged {
		t.Fatalf("bridge bypassed restart/custody boundary: err=%v restart=%v driver=%+v acknowledged=%v", err, restart, driver, acknowledged)
	}
	if note["steeringPath"] != "restart" || note["reason"] != "bridge-attached: restart for custody" {
		t.Fatalf("missing custody restart reason: %#v", note)
	}
}

type failedInterruptSession struct {
	chat.Session
	supported, closed bool
	interrupt         func(context.Context) error
	events            chan chat.Event
}

func (s *failedInterruptSession) Capabilities() chat.Capabilities {
	return chat.Capabilities{Interrupt: s.supported}
}
func (s *failedInterruptSession) Interrupt(ctx context.Context) error { return s.interrupt(ctx) }
func (s *failedInterruptSession) Events() <-chan chat.Event           { return s.events }
func (s *failedInterruptSession) Close() error                        { s.closed = true; return nil }
func TestInterruptFallbackJournalsRestart(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		supported    bool
		interrupt    func(context.Context) error
	}{
		{"unsupported", "unsupported", false, nil},
		{"error", "failed", true, func(context.Context) error { return errors.New("private driver error") }},
		{"abort timeout", "timeout", true, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		{"idle timeout", "timeout", true, func(context.Context) error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &failedInterruptSession{supported: tc.supported, interrupt: tc.interrupt, events: make(chan chat.Event)}
			steer := interactive.Steering{CommandID: "stop-send", Text: "replacement", Delivery: "prompt", SteeringMode: "interrupt-and-continue"}
			var note map[string]any
			delivery, err := interruptAndContinue(context.Background(), session, steer, harness.RunRequest{}, func(chat.Event) error { return nil }, func(kind string, data map[string]any) error {
				if kind == "execution.note" {
					note = data
				}
				return nil
			}, 10*time.Millisecond)
			if err != nil || delivery.restart == nil || delivery.restart.CommandID != steer.CommandID || !session.closed {
				t.Fatalf("missing restart: %+v %v closed=%v", delivery, err, session.closed)
			}
			if len(note) != 3 || note["steeringPath"] != "restart" || note["reason"] != tc.reason || note["commandId"] != steer.CommandID {
				t.Fatalf("unbounded or missing restart note: %#v", note)
			}
		})
	}
}
