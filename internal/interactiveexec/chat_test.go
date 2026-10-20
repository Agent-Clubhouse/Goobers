//go:build !windows

package interactiveexec

import (
	"context"
	"encoding/json"
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

type steeringDriver struct {
	chattest.FakeDriver
	sends  int
	closed bool
}

func (d *steeringDriver) Start(ctx context.Context, o chat.StartOptions) (chat.Session, error) {
	s, err := d.FakeDriver.Start(ctx, o)
	return &steeringSession{Session: s, driver: d}, err
}

type steeringSession struct {
	chat.Session
	driver *steeringDriver
}

func (s *steeringSession) Send(ctx context.Context, c []chat.ContentBlock, o chat.SendOptions) (chat.Delivery, error) {
	if s.driver.closed {
		panic("closed before steering")
	}
	delivery, err := s.Session.Send(ctx, c, o)
	s.driver.sends++
	if s.driver.sends == 2 {
		s.driver.Session.Emit("completion.accepted", map[string]any{"payload": json.RawMessage(`{"status":"success","summary":"native steering"}`)})
		s.driver.Session.Emit("turn_ended", map[string]any{"reason": "end_turn", "settled": true})
	}
	return delivery, err
}
func (s *steeringSession) Close() error { s.driver.closed = true; return s.Session.Close() }
func TestRegistryNativeSteeringKeepsSessionAndActualReceipt(t *testing.T) {
	for _, delivery := range []string{"steer", "queued"} {
		t.Run(delivery, func(t *testing.T) {
			e, req := bridgeExecutor(t)
			driver := &steeringDriver{FakeDriver: chattest.FakeDriver{Caps: chat.Capabilities{Steer: true, Queue: "native"}}}
			e.chatRegistry = func(string, harness.RunRequest) *chat.Registry {
				r := chat.NewRegistry()
				_ = r.Register("copilot", driver)
				return r
			}
			req.Harness = "copilot"
			steering := make(chan interactive.Steering, 1)
			actual := ""
			steering <- interactive.Steering{CommandID: "s1", Text: "next", Delivery: delivery, AckDelivery: func(d string, err error) {
				if err != nil {
					t.Error(err)
				}
				actual = d
			}}
			req.Steering = steering
			workspace := t.TempDir()
			out, again, err := e.driveChat(context.Background(), context.Background(), req, harness.RunRequest{Workspace: workspace, Envelope: apiv1.InvocationEnvelope{Goal: "first"}}, nil, func(string, map[string]any) error { return nil })
			if err != nil || again != nil || len(out.Payload) == 0 || driver.sends != 2 || actual != delivery || !driver.closed {
				t.Fatal(err, again, driver.sends, actual)
			}
		})
	}
}

// Opt-in integration test: real Copilot reads/tests the public sample and writes
// a completion artifact; no remote provider calls or publication are allowed.
func TestLiveCopilotInteractiveSample(t *testing.T) {
	if os.Getenv("GOOBERS_LIVE_CHAT") != "1" {
		t.Skip("live model opt-in required")
	}
	e, req := bridgeExecutor(t)
	e.config.Model = ""
	e.config.Sandbox = nil // Live validation must use the actual platform sandbox.
	req.Model = ""
	req.Harness = "copilot"
	req.Tools = nil
	req.Text = "Read invoice.mjs and run node --test. Explain the test result accurately. Do not change project files. Write the required .goobers/result.json completion artifact."
	workspace := t.TempDir()
	source, err := filepath.Abs("../../samples/interactive-investigation")
	if err != nil {
		t.Fatal(err)
	}
	if err := copySource(source, workspace); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tools := 0
	summary, err := e.native(ctx, req, workspace, "No external provider observations.", func(kind string, data map[string]any) error {
		if kind == "tool.completed" {
			tools++
		}
		return nil
	})
	t.Logf("completion: %s; error: %v", summary, err)
	// The public sample intentionally has a failing test; an honest failure
	// completion is a successful transport/custody integration check.
	if err != nil && err.Error() != "native harness returned failure" {
		t.Fatal(err)
	}
	if summary == "" || tools == 0 {
		t.Fatal("missing real tool evidence", summary, tools)
	}
	t.Log(summary)
}
