package startintent

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

func TestSourceDeadlinePinnedToFirstAcceptance(t *testing.T) {
	for _, kind := range []string{"signal", "webhook", "backlog", "refill"} {
		t.Run(kind, func(t *testing.T) {
			source, entry := sourceFixture(t)
			now := time.Now().UTC()
			observed := now
			accept := func() {
				t.Helper()
				if kind == "backlog" || kind == "refill" {
					err := source.AcceptWorkers(t.Context(), entry, localscheduler.WorkerObservation{Kind: kind, At: observed, Eligible: 2, Count: 2, MaxPending: 2})
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				var delivery *webhookhttp.Delivery
				if kind == "webhook" {
					delivery = &webhookhttp.Delivery{ID: "same", Event: "issues", PayloadDigest: "original"}
				}
				ids, err := source.AcceptSignal(t.Context(), []localscheduler.WorkflowEntry{entry}, "same", "wake", "wake", delivery, now)
				if err != nil || len(ids) != 1 {
					t.Fatal(ids, err)
				}
			}
			accept()
			before, err := source.Queue.Pending(t.Context(), 10)
			if err != nil || len(before) == 0 {
				t.Fatal(before, err)
			}
			for _, record := range before {
				envelope, err := Parse(record.Payload)
				if err != nil || !envelope.Deadline.Equal(record.AcceptedAt.Add(7*24*time.Hour)) {
					t.Fatalf("deadline=%v acceptance=%v err=%v", envelope.Deadline, record.AcceptedAt, err)
				}
			}
			source.Acquire = func(context.Context, Target) (func(), error) {
				t.Fatal("retry renewed captured authority or deadline")
				return nil, nil
			}
			now = now.Add(6 * 24 * time.Hour)
			entry.ConfigGeneration = "replacement"
			accept()
			after, err := source.Queue.Pending(t.Context(), 10)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("retry changed receipt", before, after, err)
			}
			// Exercise the real admission owner: expired source starts cannot rebuild
			// a definition or launch; each original receipt remains inspectable.
			service := &Service{Queue: source.Queue, Now: func() time.Time { return observed.Add(7 * 24 * time.Hour) },
				RunDirectory: func(context.Context, string) (string, error) { return "", nil },
				Build: func(context.Context, Target) (Prepared, error) {
					t.Fatal("expired source rebuilt execution")
					return Prepared{}, nil
				},
				Scheduler: func() *localscheduler.Scheduler { t.Fatal("expired source reached scheduler"); return nil },
			}
			for _, record := range before {
				if err := service.Dispatch(t.Context(), t.Context(), record); err != nil {
					t.Fatal(err)
				}
				terminal, err := source.Queue.Get(t.Context(), record.ID, record.Actor)
				if err != nil || terminal.State != triggerqueue.Rejected || terminal.Reason != "accepted trigger expired before execution admission" || string(terminal.Payload) != string(record.Payload) || !terminal.AcceptedAt.Equal(record.AcceptedAt) {
					t.Fatal(terminal, err)
				}
			}
		})
	}
}
