package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestOrdinaryRecoveryRetriesOnlyAbsentPriorProcessStarts(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "published"}[published], func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			queuePath := filepath.Join(layout.Root, "accepted.db")
			dispatch := newDaemonTriggerService()
			s := acceptedService(t, queuePath, dispatch)
			starter := &acceptedRunIDStarter{ids: make(chan string, 2)}
			entry := localscheduler.WorkflowEntry{Workflow: "impl", Gaggle: "own", Starter: starter}
			log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log)
			dispatch.AttachScheduler(scheduler)
			dispatch.AttachDispatchContext(t.Context())
			target := startintent.Target{Workflow: "impl", Gaggle: "own", ConfigGeneration: "generation-1", WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goober"))}
			install := func(s *durableTriggerService) {
				s.ordinary = &startintent.Service{Queue: s.queue, Now: time.Now,
					Capture: func(context.Context, startintent.Request) (startintent.Target, func(), error) {
						return target, func() {}, nil
					},
					Build: func(context.Context, startintent.Target) (startintent.Prepared, error) {
						return startintent.Prepared{Entry: entry, Release: func() {}}, nil
					},
					Scheduler: func() *localscheduler.Scheduler { return scheduler },
					RunDirectory: func(ctx context.Context, id string) (string, error) {
						return acceptedTriggerJournalDir(ctx, layout, id)
					},
				}
			}
			// A legacy receipt must still replay exactly after typed admission is enabled.
			legacyRequest := httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "legacy", Actor: "operator"}
			legacy, err := s.Trigger(t.Context(), legacyRequest)
			if err != nil {
				t.Fatal(err)
			}
			install(s)
			if repeated, err := s.Trigger(t.Context(), legacyRequest); err != nil || !repeated.Duplicate || repeated.AcceptanceID != legacy.AcceptanceID {
				t.Fatal(repeated, err)
			}
			if err := s.queue.BeginDispatch(t.Context(), legacy.AcceptanceID); err != nil {
				t.Fatal(err)
			}
			if err := s.queue.Finish(t.Context(), legacy.AcceptanceID, triggerqueue.Rejected, "", "fixture complete", time.Now()); err != nil {
				t.Fatal(err)
			}
			accepted, err := s.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "typed", Actor: "operator"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.queue.BeginDispatch(t.Context(), accepted.AcceptanceID); err != nil {
				t.Fatal(err)
			}
			runID := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
			if published {
				run, err := journal.Create(layout.ForGaggle("own").RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "impl", Gaggle: "own", WorkflowVersion: 1, ConfigGeneration: target.ConfigGeneration, WorkflowDigest: target.WorkflowDigest, GooberDigest: target.GooberDigest, Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "impl"}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.queue.Close(); err != nil {
				t.Fatal(err)
			}
			s = acceptedService(t, queuePath, dispatch)
			install(s)
			for range 4 {
				if err := s.Drain(t.Context()); err != nil {
					t.Fatal(err)
				}
				scheduler.Wait()
			}
			want := triggerqueue.Dispatched
			if !published {
				want = triggerqueue.Dispatching
				select {
				case got := <-starter.ids:
					if got != runID {
						t.Fatal(got)
					}
				default:
					t.Fatal("absent prior start not retried")
				}
			}
			if len(starter.ids) != 0 || len(s.bootUncertain) != 0 {
				t.Fatal("replayed current or published execution")
			}
			record, err := s.queue.Get(t.Context(), accepted.AcceptanceID, "operator")
			if err != nil || record.State != want || record.RunID != runID {
				t.Fatal(record, err)
			}
		})
	}
}
