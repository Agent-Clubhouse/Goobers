package startintent

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type workerCount int

func (c workerCount) EligibleCount(context.Context) (int, error) { return int(c), nil }

type workerStarter struct {
	calls    atomic.Int32
	requests chan localscheduler.StartRequest
}

func (s *workerStarter) Start(_ context.Context, request localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.calls.Add(1)
	if s.requests != nil {
		s.requests <- request
	}
	return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
}
func workerScheduler(t *testing.T, source *Sources, entry localscheduler.WorkflowEntry) *localscheduler.Scheduler {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	t.Cleanup(func() { scheduler.Wait(); _ = log.Close() })
	return scheduler
}
func workerPending(t *testing.T, source *Sources, want int) []triggerqueue.Record {
	t.Helper()
	pending, err := source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != want {
		t.Fatalf("pending=%d want=%d: %v", len(pending), want, err)
	}
	return pending
}

func TestWorkerCountQueuesBoundedOrdinalsAndRestartDoesNotRefillPendingCapacity(t *testing.T) {
	source, entry := sourceFixture(t)
	starter := &workerStarter{}
	entry.Starter = starter
	entry.BacklogCounter = workerCount(5)
	entry.Readiness.MaxConcurrentRuns = 3
	scheduler := workerScheduler(t, source, entry)
	now := time.Now().UTC()
	scheduler.Tick(t.Context(), now)
	pending := workerPending(t, source, 3)
	ordinals := map[int]bool{}
	for _, record := range pending {
		envelope, err := Parse(record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if envelope.Source == nil || envelope.Source.WorkerKind != "backlog" || envelope.Source.ObservedCount != 5 || !envelope.Source.ObservedAt.Equal(now) || envelope.Target.ConfigGeneration != entry.ConfigGeneration {
			t.Fatal(envelope)
		}
		ordinals[envelope.Source.WorkerOrdinal] = true
	}
	if len(ordinals) != 3 {
		t.Fatal(ordinals)
	}
	scheduler.Tick(t.Context(), now.Add(time.Minute))
	restarted := workerScheduler(t, source, entry)
	restarted.Tick(t.Context(), now.Add(2*time.Minute))
	workerPending(t, source, 3)
	if starter.calls.Load() != 0 {
		t.Fatal("worker executed before durable dispatch")
	}
}

func TestWorkerRefillAccountsForQueuedManualAndExactLiveOwner(t *testing.T) {
	source, entry := sourceFixture(t)
	entry.Starter = &workerStarter{}
	entry.Readiness = apiv1.ReadinessConditions{MaxConcurrentRuns: 4, DesiredConcurrentRuns: 3}
	entry.RefillDemandCounter = workerCount(10)
	raw, release, err := source.pin(t.Context(), entry, localscheduler.SourceTrigger{Signal: "manual-test", Ref: "manual-test"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	envelope, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Source = nil
	raw, err = envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	manual, _, err := source.Queue.Accept(t.Context(), "manual", "alice", raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	scheduler := workerScheduler(t, source, entry)
	runID := strings.TrimPrefix(manual.ID, "trigger-")
	releaseLive, ok, why := scheduler.ReserveContinuation(runID, entry.Gaggle, entry.Workflow)
	if !ok {
		t.Fatal(why)
	}
	defer releaseLive()
	if err = source.Queue.BeginDispatch(t.Context(), manual.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	scheduler.Tick(t.Context(), now)
	// One live owner (whose row is still dispatching), two queued workers, desired=3.
	workerPending(t, source, 2)
	scheduler.Tick(t.Context(), now.Add(time.Minute))
	workerPending(t, source, 2)
	if err = source.Queue.Finish(t.Context(), manual.ID, triggerqueue.Dispatched, runID, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	releaseLive()
	scheduler.Tick(t.Context(), now.Add(2*time.Minute))
	workerPending(t, source, 3)
}

func TestWorkerBacklogAlreadyCoveredDoesNotSuppressDistinctRefillDemand(t *testing.T) {
	source, entry := sourceFixture(t)
	entry.Starter = &workerStarter{}
	entry.Readiness = apiv1.ReadinessConditions{MaxConcurrentRuns: 4, DesiredConcurrentRuns: 3}
	entry.BacklogCounter = workerCount(1)
	entry.RefillDemandCounter = workerCount(10)
	now := time.Now().UTC()
	if err := source.AcceptWorkers(t.Context(), entry, localscheduler.WorkerObservation{Kind: "backlog", At: now.Add(-time.Minute), Eligible: 1, Count: 1, MaxPending: 4}); err != nil {
		t.Fatal(err)
	}
	workerScheduler(t, source, entry).Tick(t.Context(), now)
	pending := workerPending(t, source, 3)
	refill := 0
	for _, record := range pending {
		e, err := Parse(record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if e.Source.WorkerKind == "refill" {
			refill++
		}
	}
	if refill != 2 {
		t.Fatal(refill)
	}
}

func TestWorkerSourceFailureNeverFallsBackToDirectStart(t *testing.T) {
	source, entry := sourceFixture(t)
	starter := &workerStarter{}
	entry.Starter = starter
	entry.BacklogCounter = workerCount(2)
	entry.Readiness.MaxConcurrentRuns = 2
	source.Acquire = func(context.Context, Target) (func(), error) { return nil, errors.New("archive unavailable") }
	scheduler := workerScheduler(t, source, entry)
	scheduler.Tick(t.Context(), time.Now())
	scheduler.Wait()
	workerPending(t, source, 0)
	if starter.calls.Load() != 0 {
		t.Fatal("failed queue capture started directly")
	}
}

func TestWorkerPreparedDispatchUsesCapturedSourceAndCurrentEligibility(t *testing.T) {
	for _, kind := range []string{"backlog", "refill"} {
		for _, removed := range []bool{false, true} {
			t.Run(kind+"/removed="+strconv.FormatBool(removed), func(t *testing.T) {
				source, entry := sourceFixture(t)
				starter := &workerStarter{requests: make(chan localscheduler.StartRequest, 1)}
				entry.Starter = starter
				entry.Readiness = apiv1.ReadinessConditions{MaxConcurrentRuns: 1, DesiredConcurrentRuns: 1}
				entry.BacklogCounter = workerCount(1)
				entry.RefillDemandCounter = workerCount(1)
				if err := source.AcceptWorkers(t.Context(), entry, localscheduler.WorkerObservation{Kind: kind, At: time.Now(), Eligible: 1, Count: 1, MaxPending: 1}); err != nil {
					t.Fatal(err)
				}
				record := workerPending(t, source, 1)[0]
				current := entry
				if removed {
					current.BacklogCounter = nil
					current.RefillDemandCounter = nil
				}
				scheduler := workerScheduler(t, source, current)
				service := &Service{Queue: source.Queue, Now: time.Now, Scheduler: func() *localscheduler.Scheduler { return scheduler }, RunDirectory: func(context.Context, string) (string, error) { return "", nil }, Build: func(context.Context, Target) (Prepared, error) {
					return Prepared{Entry: entry, Release: func() {}}, nil
				}}
				if err := service.Dispatch(t.Context(), t.Context(), record); err != nil {
					t.Fatal(err)
				}
				scheduler.Wait()
				stored, err := source.Queue.Get(t.Context(), record.ID, "scheduler")
				if err != nil {
					t.Fatal(err)
				}
				if removed {
					if stored.State != triggerqueue.Rejected || starter.calls.Load() != 0 {
						t.Fatal(stored, starter.calls.Load())
					}
				} else {
					if stored.State != triggerqueue.Dispatching || starter.calls.Load() != 1 {
						t.Fatal(stored, starter.calls.Load())
					}
					request := <-starter.requests
					if request.Item != nil || request.Trigger.Kind != journal.TriggerItem || request.RunID != strings.TrimPrefix(record.ID, "trigger-") || !request.RequireDurableJournal {
						t.Fatal(request)
					}
				}
			})
		}
	}
}

func TestWorkerExactObservationReplayKeepsPinsAndRejectsChangedCount(t *testing.T) {
	source, entry := sourceFixture(t)
	observation := localscheduler.WorkerObservation{Kind: "backlog", At: time.Now().UTC(), Eligible: 4, Count: 2, MaxPending: 2}
	if err := source.AcceptWorkers(t.Context(), entry, observation); err != nil {
		t.Fatal(err)
	}
	before := workerPending(t, source, 2)
	source.Acquire = func(context.Context, Target) (func(), error) {
		t.Fatal("exact replay recaptured archive")
		return nil, nil
	}
	entry.ConfigGeneration = "new-generation"
	if err := source.AcceptWorkers(t.Context(), entry, observation); err != nil {
		t.Fatal(err)
	}
	after := workerPending(t, source, 2)
	if before[0].ID != after[0].ID || string(before[0].Payload) != string(after[0].Payload) {
		t.Fatal("replay changed accepted start")
	}
	observation.Count = 1
	if err := source.AcceptWorkers(t.Context(), entry, observation); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal(err)
	}
}

func TestWorkerObservationBoundsAndClosedProvenance(t *testing.T) {
	source, entry := sourceFixture(t)
	now := time.Now().UTC()
	for _, observation := range []localscheduler.WorkerObservation{
		{Kind: "backlog", At: now, Eligible: 40, Count: 33, MaxPending: 40},
		{Kind: "backlog", At: now, Eligible: 1, Count: 2, MaxPending: 2},
		{Kind: "backlog", At: now, Eligible: 2, Count: 2, MaxPending: 1},
		{Kind: "invented", At: now, Eligible: 1, Count: 1, MaxPending: 1},
	} {
		if err := source.AcceptWorkers(t.Context(), entry, observation); err == nil {
			t.Fatal("accepted invalid observation", observation)
		}
	}
	for _, provenance := range []localscheduler.SourceTrigger{
		{WorkerKind: "backlog", ObservedAt: now, ObservedCount: 1, WorkerOrdinal: 2},
		{WorkerKind: "backlog", ObservedAt: now, ObservedCount: 1, WorkerOrdinal: 1, Signal: "override"},
		{ObservedAt: now, ObservedCount: 1, WorkerOrdinal: 1, Signal: "override", Ref: "override"},
	} {
		if err := provenance.Validate(); err == nil {
			t.Fatal("accepted contradictory provenance", provenance)
		}
	}
	workerPending(t, source, 0)
}

func TestWorkerRefillRetainsQueueCustodyWhileCapacityIsHeld(t *testing.T) {
	source, entry := sourceFixture(t)
	starter := &workerStarter{requests: make(chan localscheduler.StartRequest, 1)}
	entry.Starter = starter
	entry.Readiness = apiv1.ReadinessConditions{MaxConcurrentRuns: 1, DesiredConcurrentRuns: 1}
	entry.RefillDemandCounter = workerCount(1)
	if err := source.AcceptWorkers(t.Context(), entry, localscheduler.WorkerObservation{Kind: "refill", At: time.Now(), Eligible: 1, Count: 1, MaxPending: 1}); err != nil {
		t.Fatal(err)
	}
	record := workerPending(t, source, 1)[0]
	scheduler := workerScheduler(t, source, entry)
	// A human continuation claims the live slot after this worker was accepted.
	release, ok, why := scheduler.ReserveContinuation(strings.Repeat("a", 32), entry.Gaggle, entry.Workflow)
	if !ok {
		t.Fatal(why)
	}
	defer release()
	service := &Service{Queue: source.Queue, Now: time.Now, Scheduler: func() *localscheduler.Scheduler { return scheduler }, RunDirectory: func(context.Context, string) (string, error) { return "", nil }, Build: func(context.Context, Target) (Prepared, error) {
		return Prepared{Entry: entry, Release: func() {}}, nil
	}}
	if err := service.Dispatch(t.Context(), t.Context(), record); err != nil {
		t.Fatal(err)
	}
	workerPending(t, source, 1)
	if starter.calls.Load() != 0 {
		t.Fatal("refill exceeded live capacity")
	}
	release()
	if err := service.Dispatch(t.Context(), t.Context(), record); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	if starter.calls.Load() != 1 {
		t.Fatal("capacity-held refill was stranded")
	}
	request := <-starter.requests
	if request.RunID != strings.TrimPrefix(record.ID, "trigger-") {
		t.Fatal("retry changed run identity")
	}
}
