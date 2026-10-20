package interactive

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestDriverControlReceiptsSurviveReplay(t *testing.T) {
	for _, kind := range []string{"permission.respond", "question.answer", "session.set_effort"} {
		for _, rejected := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/rejected", false: "/delivered"}[rejected], func(t *testing.T) {
				e := newExecutor()
				opts := options(t, e)
				opts.AvailableExecutionModes = []string{ExecutionModeCodexLocal}
				opts.DefaultExecutionMode = ExecutionModeCodexLocal
				s := openTest(t, opts)
				session := createTest(t, s, "alice")
				start := commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "Work"})
				run := receiveRun(t, e)
				cmd := Command{Type: kind, TurnID: start.TurnID}
				if kind != "session.set_effort" {
					cmd.RequestID, cmd.OptionID = "request-7", "reject_once"
					cmd.Answers = map[string][]string{"answer": {"yes"}}
				}
				accepted := commandTest(t, s, "alice", session.ID, cmd)
				control := <-run.req.Controls
				if accepted.RequestID != cmd.RequestID {
					t.Fatalf("accepted request identity = %q", accepted.RequestID)
				}
				var deliveryErr error
				if rejected {
					deliveryErr = errors.New("unsupported request")
				}
				control.Ack("", deliveryErr)
				// Replay uses the exact original admission sequence and fence.
				replayed, err := s.Command(context.Background(), "alice", session.ID, control.Command)
				if err != nil {
					t.Fatal(err)
				}
				want := "delivered"
				if rejected {
					want = "rejected"
				}
				if replayed.Status != want || replayed.RequestID != cmd.RequestID {
					t.Fatalf("receipt = %+v", replayed)
				}
				raw, _ := json.Marshal(replayed)
				var serialized map[string]any
				_ = json.Unmarshal(raw, &serialized)
				actual, exists := serialized["actual"]
				if kind == "session.set_effort" && !rejected {
					if !exists || actual != "" {
						t.Fatalf("default effort confirmation missing: %s", raw)
					}
				} else if exists {
					t.Fatalf("unexpected actual: %s", raw)
				}
				events, err := s.Events("alice", session.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, event := range events {
					if event.CommandID != accepted.CommandID {
						continue
					}
					count++
					if event.Data["requestId"] != cmd.RequestID {
						t.Fatalf("event lost request identity: %+v", event)
					}
					if event.Type == "command.delivered" && kind == "session.set_effort" {
						if value, ok := event.Data["actual"]; !ok || value != "" {
							t.Fatalf("event lost default effort: %+v", event)
						}
					}
				}
				if count != 2 {
					t.Fatalf("expected admission and outcome, got %d", count)
				}
			})
		}
	}
}
