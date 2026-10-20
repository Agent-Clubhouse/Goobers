//go:build !windows

package interactiveexec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
)

type fixtureQueuedDriver struct {
	chattest.FakeDriver
	closed bool
}

func (d *fixtureQueuedDriver) Start(ctx context.Context, o chat.StartOptions) (chat.Session, error) {
	s, e := d.FakeDriver.Start(ctx, o)
	return &fixtureQueuedSession{Session: s, d: d}, e
}

type fixtureQueuedSession struct {
	chat.Session
	d *fixtureQueuedDriver
}

func (s *fixtureQueuedSession) Send(ctx context.Context, c []chat.ContentBlock, o chat.SendOptions) (chat.Delivery, error) {
	actual, err := s.Session.Send(ctx, c, o)
	if o.Delivery == chat.Queued {
		// Claude emits a result/turn_ended for the first request, then another
		// lifecycle for the acknowledged queued request (driver_test.go pins it).
		s.d.Session.Emit("completion.accepted", map[string]any{"payload": json.RawMessage(`{"status":"success","summary":"first request only"}`)})
		s.d.Session.Emit("turn_ended", map[string]any{"reason": "end_turn"})
		s.d.Session.Emit("turn_started", map[string]any{})
		s.d.Session.Emit("completion.accepted", map[string]any{"payload": json.RawMessage(`{"status":"success","summary":"queued request completed"}`)})
		s.d.Session.Emit("turn_ended", map[string]any{"reason": "end_turn"})
	}
	return actual, err
}
func (s *fixtureQueuedSession) Close() error { s.d.closed = true; return s.Session.Close() }
func TestHostDrainsAcknowledgedQueuedTurn(t *testing.T) {
	e, req := bridgeExecutor(t)
	d := &fixtureQueuedDriver{FakeDriver: chattest.FakeDriver{Caps: chat.Capabilities{Steer: true, Queue: "native"}}}
	e.chatRegistry = func(string, harness.RunRequest) *chat.Registry {
		r := chat.NewRegistry()
		_ = r.Register("claude", d)
		return r
	}
	req.Harness = "claude"
	q := make(chan interactive.Steering, 1)
	acked := false
	q <- interactive.Steering{CommandID: "queued-command", Text: "second request", Delivery: "queued", AckDelivery: func(actual string, err error) {
		if actual != "queued" || err != nil {
			t.Fatalf("ack %s %v", actual, err)
		}
		acked = true
	}}
	req.Steering = q
	out, _, err := e.driveChat(t.Context(), t.Context(), req, harness.RunRequest{Workspace: t.TempDir(), Envelope: apiv1.InvocationEnvelope{Goal: "first request"}}, nil, func(string, map[string]any) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !acked || !strings.Contains(string(out.Payload), "queued request completed") {
		t.Fatalf("host closed=%t after first result despite acknowledged queue=%t; completion=%s", d.closed, acked, out.Payload)
	}
}
