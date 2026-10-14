package startintent

import (
	"context"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type queuedQuota struct {
	blocked bool
	reset   time.Time
}

func (q *queuedQuota) ExhaustedFor(apiv1.Provider, time.Time) (time.Time, bool) {
	return q.reset, q.blocked
}
func (q *queuedQuota) ResetIfDue(apiv1.Provider, time.Time) (localscheduler.ProviderQuotaReset, bool) {
	return localscheduler.ProviderQuotaReset{}, false
}
func (q *queuedQuota) ReservePolls(apiv1.Provider, time.Time, int) localscheduler.ProviderPollBudget {
	return localscheduler.ProviderPollBudget{}
}

type queuedPRCount struct{ count int }

func (q *queuedPRCount) OpenPRCount(string, string) (int, bool) { return q.count, true }

func TestAcceptedStartRetainsIdentityUntilOperatingConstraintClears(t *testing.T) {
	for _, condition := range []string{"hourly-budget", "daily-budget", "provider-quota", "open-pr-cap", "paused"} {
		t.Run(condition, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			service := intentService(t)
			entry := localscheduler.WorkflowEntry{Gaggle: "own", Workflow: "repair", ConfigGeneration: "generation-1", WorkflowDigest: "workflow-1", GooberDigest: "goober-1"}
			started := make(chan string, 3)
			entry.Starter = scheduleStarterFunc(func(_ context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
				started <- req.RunID
				return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
			})
			quota, prs := &queuedQuota{reset: now.Add(time.Hour)}, &queuedPRCount{}
			consumeBudget := false
			reason := ""
			switch condition {
			case "hourly-budget":
				entry.Readiness.MaxRunsPerHour = 1
				consumeBudget = true
				reason = localscheduler.ReasonBudget
			case "daily-budget":
				entry.Readiness.MaxRunsPerDay = 1
				consumeBudget = true
				reason = localscheduler.ReasonDailyBudget
			case "provider-quota":
				quota.blocked = true
				reason = localscheduler.ReasonProviderQuota
			case "open-pr-cap":
				entry.Readiness.MaxOpenPRs = 1
				prs.count = 1
				reason = localscheduler.ReasonOpenPRCap
			case "paused":
				reason = localscheduler.ReasonDisabled
			}
			log, _, err := journal.OpenInstanceLog(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			current := entry
			if condition == "paused" {
				current.DisabledReason = "workflow paused"
			}
			newScheduler := func() *localscheduler.Scheduler {
				s := localscheduler.New([]localscheduler.WorkflowEntry{current}, log, localscheduler.WithClock(func() time.Time { return now }, time.After), localscheduler.WithProviderQuota(quota), localscheduler.WithOpenPRCounter(prs))
				if err := s.ReconcileAll(nil, now); err != nil {
					t.Fatal(err)
				}
				return s
			}
			scheduler := newScheduler()
			t.Cleanup(func() { scheduler.Wait() })
			service.Now = func() time.Time { return now }
			service.Scheduler = func() *localscheduler.Scheduler { return scheduler }
			service.RunDirectory = func(context.Context, string) (string, error) { return "", nil }
			service.Build = func(_ context.Context, target Target) (Prepared, error) {
				if target.ConfigGeneration != entry.ConfigGeneration {
					t.Fatal("accepted pins changed", target)
				}
				return Prepared{Entry: entry, Release: func() {}}, nil
			}
			accept := func(key string) triggerqueue.Record {
				t.Helper()
				r, _, err := service.Accept(t.Context(), key, "operator", Request{Workflow: entry.Workflow, Gaggle: entry.Gaggle})
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			if consumeBudget {
				if err := service.Dispatch(t.Context(), t.Context(), accept("consume")); err != nil {
					t.Fatal(err)
				}
				scheduler.Wait()
				<-started
			}
			original := accept("held")
			for range 2 {
				if err := service.Dispatch(t.Context(), t.Context(), original); err != nil {
					t.Fatal(err)
				}
				held, err := service.Queue.Get(t.Context(), original.ID, "operator")
				if err != nil || held.State != triggerqueue.Accepted || !strings.HasPrefix(held.Reason, reason) || held.RunID != "" || string(held.Payload) != string(original.Payload) || !held.AcceptedAt.Equal(original.AcceptedAt) {
					t.Fatal("wait lost accepted custody", held, err)
				}
			}
			select {
			case id := <-started:
				t.Fatal("launched through refusal", id)
			default:
			}
			switch condition {
			case "hourly-budget":
				now = now.Add(time.Hour + time.Second)
			case "daily-budget":
				now = now.Add(24*time.Hour + time.Second)
			case "provider-quota":
				quota.blocked = false
			case "open-pr-cap":
				prs.count = 0
			case "paused":
				scheduler.Wait()
				current.DisabledReason = ""
				scheduler = newScheduler()
			}
			if err := service.Dispatch(t.Context(), t.Context(), original); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
			select {
			case id := <-started:
				if id != strings.TrimPrefix(original.ID, "trigger-") {
					t.Fatal("changed reserved run identity", id)
				}
			default:
				t.Fatal("accepted run did not resume")
			}
			admitted, err := service.Queue.Get(t.Context(), original.ID, "operator")
			if err != nil || admitted.State != triggerqueue.Dispatching || admitted.Reason != "" || admitted.RunID != strings.TrimPrefix(original.ID, "trigger-") {
				t.Fatal(admitted, err)
			}
		})
	}
}

func TestQueuedPermanentRefusalStillSettlesAcceptedStart(t *testing.T) {
	for _, reason := range []string{localscheduler.ReasonMissingCapability + ": git", localscheduler.ReasonProviderAuth + ": revoked", "unknown refusal"} {
		t.Run(reason, func(t *testing.T) {
			service := intentService(t)
			record, _, err := service.Accept(t.Context(), "key", "operator", Request{Workflow: "repair"})
			if err != nil {
				t.Fatal(err)
			}
			if err = service.Queue.BeginDispatch(t.Context(), record.ID); err != nil {
				t.Fatal(err)
			}
			if err = service.refuseDispatch(t.Context(), record.ID, &localscheduler.TriggerRejectedError{Reason: reason}); err != nil {
				t.Fatal(err)
			}
			terminal, err := service.Queue.Get(t.Context(), record.ID, "operator")
			if err != nil || terminal.State != triggerqueue.Rejected || terminal.Reason != reason {
				t.Fatal(terminal, err)
			}
		})
	}
}
