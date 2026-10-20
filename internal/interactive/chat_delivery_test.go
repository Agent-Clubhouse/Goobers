package interactive

import (
	"context"
	"testing"
)

func TestChatDeliveryIntentIsValidatedAndForwarded(t *testing.T) {
	for _, delivery := range []string{"steer", "queued", "prompt"} {
		t.Run(delivery, func(t *testing.T) {
			e := newExecutor()
			opts := options(t, e)
			opts.AvailableExecutionModes = []string{ExecutionModeCodexLocal}
			opts.DefaultExecutionMode = ExecutionModeCodexLocal
			s := openTest(t, opts)
			session := createTest(t, s, "alice")
			turn := commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
			run := receiveRun(t, e)
			mode := ""
			if delivery == "prompt" {
				mode = "interrupt-and-continue"
			}
			commandTest(t, s, "alice", session.ID, Command{Type: "steer", Text: "next", TurnID: turn.TurnID, Delivery: delivery, SteeringMode: mode})
			message := <-run.req.Steering
			if message.Delivery != delivery || message.SteeringMode != mode {
				t.Fatalf("delivery intent lost: %#v", message)
			}
		})
	}
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	turn := commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	run := receiveRun(t, e)
	for _, cmd := range []Command{
		{Delivery: "queued"},
		{Delivery: "prompt", SteeringMode: "interrupt-and-continue"},
		{Delivery: "invalid"},
		{Delivery: "prompt"},
		{Delivery: "steer", SteeringMode: "interrupt-and-continue"},
	} {
		current, _ := s.Session("alice", session.ID)
		cmd.Type, cmd.CommandID, cmd.Text, cmd.TurnID = "steer", id("cmd_"), "next", turn.TurnID
		cmd.ExpectedFence, cmd.ExpectedSequence = current.Fence, current.Sequence
		if _, err := s.Command(context.Background(), "alice", session.ID, cmd); err == nil {
			t.Fatalf("unsupported or invalid demo delivery accepted: %#v", cmd)
		}
	}
	select {
	case <-run.req.Steering:
		t.Fatal("rejected command reached the executor")
	default:
	}
}

func TestActualSteeringReceiptOverridesRequestedIntent(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	receipt := commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	run := receiveRun(t, e)
	steer := commandTest(t, s, "alice", session.ID, Command{Type: "steer", Text: "continue", TurnID: receipt.TurnID, Delivery: "steer"})
	message := <-run.req.Steering
	message.AckDelivery("prompt", nil)
	s.mu.Lock()
	got := s.state.Sessions[session.ID].Commands[steer.CommandID].Receipt
	s.mu.Unlock()
	if got.Status != "delivered" || got.Delivery != "prompt" || got.SteeringMode != "interrupt-and-continue" {
		t.Fatalf("receipt reports intent instead of actual delivery: %+v", got)
	}
}
