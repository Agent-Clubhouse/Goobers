package runner

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

type parallelCapacityScheduler struct {
	scheduler *localscheduler.Scheduler
	suspends  atomic.Int32
}

func (s *parallelCapacityScheduler) SuspendChildParent(ctx context.Context, run string) (ChildParentSuspension, error) {
	s.suspends.Add(1)
	return s.scheduler.SuspendChildParent(ctx, run)
}

type parallelCapacityFixture struct {
	capacity *parallelCapacityScheduler
	request  localscheduler.ChildAdmissionRequest
	cleanup  func()
	log      *journal.InstanceLog
}

func newParallelCapacityFixture(t *testing.T) parallelCapacityFixture {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	entry := localscheduler.WorkflowEntry{Gaggle: "own", Workflow: "parent", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub}, Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 2}}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log)
	request := localscheduler.ChildAdmissionRequest{RunID: strings.Repeat("b", 32), ParentRunID: strings.Repeat("a", 32), Parent: localscheduler.WorkflowIdentity{Gaggle: "own", Workflow: "parent"}, Child: localscheduler.WorkflowEntry{Gaggle: "own", Workflow: "generated", RepoRef: entry.RepoRef}}
	cleanup, ok, reason := scheduler.ReserveContinuation(request.ParentRunID, entry.Gaggle, entry.Workflow)
	if !ok {
		t.Fatal(reason)
	}
	t.Cleanup(cleanup)
	return parallelCapacityFixture{capacity: &parallelCapacityScheduler{scheduler: scheduler}, request: request, cleanup: cleanup, log: log}
}

func (f parallelCapacityFixture) coordinator(t *testing.T, parked, finished []int) *parallelChildCapacity {
	t.Helper()
	p, err := newParallelChildCapacity(f.request.ParentRunID, f.capacity, []int{1, 2}, parked, finished)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f parallelCapacityFixture) requireChildBlocked(t *testing.T, reason string) {
	t.Helper()
	release, err := f.capacity.scheduler.ReserveChild(t.Context(), f.request, time.Now())
	if err == nil {
		release()
		t.Fatal("child stole a runnable parent or exceeded its budget")
	}
	var rejected *localscheduler.TriggerRejectedError
	if !errors.As(err, &rejected) || rejected.Reason != reason {
		t.Fatalf("refusal=%v, want %s", err, reason)
	}
}

func (f parallelCapacityFixture) runChild(t *testing.T) func() {
	t.Helper()
	release, err := f.capacity.scheduler.ReserveChild(t.Context(), f.request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

func TestParallelChildCapacityQueuedSiblingAndBudget(t *testing.T) {
	f := newParallelCapacityFixture(t)
	p := f.coordinator(t, nil, nil)
	publish := func() error { return nil }
	first, err := p.Park(t.Context(), 1, publish)
	if err != nil {
		t.Fatal(err)
	}
	// Branch 2 has not started, but is declared and runnable.
	f.requireChildBlocked(t, localscheduler.ReasonMaxParallel)
	if _, err = p.Park(t.Context(), 2, publish); err != nil {
		t.Fatal(err)
	}
	childDone := f.runChild(t)
	var continued int
	continuation := func() error { continued++; return nil }
	if err = first.Resume(t.Context(), continuation); err == nil || continued != 0 {
		t.Fatal("parent continued while child held its capacity", err, continued)
	}
	childDone()
	if err = first.Resume(t.Context(), continuation); err != nil {
		t.Fatal(err)
	}
	if err = first.Resume(t.Context(), continuation); err != nil || continued != 1 {
		t.Fatal("duplicate continuation published", err, continued)
	}
	f.requireChildBlocked(t, localscheduler.ReasonMaxParallel)
	second, err := p.Park(t.Context(), 1, publish)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Resume(t.Context(), continuation); err == nil {
		t.Fatal("superseded suspension resumed a new wait")
	}
	f.request.RunID = strings.Repeat("c", 32)
	f.runChild(t)()
	if err = second.Resume(t.Context(), continuation); err != nil || continued != 2 {
		t.Fatal(err, continued)
	}
	if err = p.Finish(t.Context(), 1, publish); err != nil {
		t.Fatal(err)
	}
	// The remaining parked branch holds no run permit. Only two actual child
	// admissions consumed the configured budget; repeated resumes did not.
	f.request.RunID = strings.Repeat("d", 32)
	f.requireChildBlocked(t, localscheduler.ReasonBudget)
	events, err := journal.ReadInstanceLog(f.log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	admissions := 0
	for _, event := range events {
		if event.Runner["note"] == "child.admission" {
			admissions++
		}
	}
	if admissions != 2 {
		t.Fatal("unexpected extra start admission", admissions)
	}
}

func TestParallelChildCapacitySerializesResumeAndSiblingFinish(t *testing.T) {
	f := newParallelCapacityFixture(t)
	p := f.coordinator(t, []int{1, 2}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	resumed := make(chan error, 1)
	go func() {
		resumed <- p.Resume(t.Context(), 1, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	// The permit has been reacquired but continued is not yet durable. Hold
	// sibling transitions until publication completes.
	if p.mu.TryLock() {
		p.mu.Unlock()
		close(release)
		t.Fatal("resume publication escaped aggregate ownership")
	}
	finished := make(chan error, 1)
	go func() { finished <- p.Finish(t.Context(), 2, func() error { return nil }) }()
	close(release)
	if err := <-resumed; err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if f.capacity.suspends.Load() != 1 {
		t.Fatal("sibling surrendered freshly reacquired parent capacity")
	}
	f.requireChildBlocked(t, localscheduler.ReasonMaxParallel)
}

func TestParallelChildCapacityFailedPublicationRequiresRecovery(t *testing.T) {
	for _, operation := range []string{"park", "resume", "finish"} {
		t.Run(operation, func(t *testing.T) {
			f := newParallelCapacityFixture(t)
			var parked []int
			if operation == "resume" {
				parked = []int{1, 2}
			}
			p := f.coordinator(t, parked, nil)
			uncertain := errors.New("journal write outcome uncertain")
			fail := func() error { return uncertain }
			var err error
			switch operation {
			case "park":
				_, err = p.Park(t.Context(), 1, fail)
			case "resume":
				err = p.Resume(t.Context(), 1, fail)
			case "finish":
				err = p.Finish(t.Context(), 1, fail)
			}
			if !errors.Is(err, uncertain) {
				t.Fatal(err)
			}
			if _, err = p.Park(t.Context(), 2, func() error { t.Error("sibling executed after uncertain publication"); return nil }); !errors.Is(err, uncertain) {
				t.Fatal(err)
			}
			f.requireChildBlocked(t, localscheduler.ReasonMaxParallel)
		})
	}
}

func TestParallelChildCapacityCancellationAndFinishedBranches(t *testing.T) {
	f := newParallelCapacityFixture(t)
	p := f.coordinator(t, []int{1}, []int{2})
	f.cleanup()
	if err := p.Resume(t.Context(), 1, func() error { t.Error("cancelled parent resumed"); return nil }); err == nil {
		t.Fatal("released scheduler owner resurrected")
	}
	f = newParallelCapacityFixture(t)
	p = f.coordinator(t, nil, []int{2})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Park(ctx, 1, func() error { t.Error("cancelled transition published"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := p.Park(t.Context(), 2, func() error { return nil }); err == nil {
		t.Fatal("finished branch parked")
	}
	if _, err := p.Park(t.Context(), 3, func() error { return nil }); err == nil {
		t.Fatal("undeclared branch changed ownership")
	}
	if _, err := p.Park(t.Context(), 1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.runChild(t)()
}

func TestParallelChildCapacityRejectsConflictingRecovery(t *testing.T) {
	f := newParallelCapacityFixture(t)
	for _, test := range []struct {
		name                       string
		branches, parked, finished []int
	}{
		{name: "empty"},
		{name: "serial-id", branches: []int{0}},
		{name: "duplicate-declaration", branches: []int{1, 1}},
		{name: "undeclared-wait", branches: []int{1}, parked: []int{2}},
		{name: "duplicate-wait", branches: []int{1}, parked: []int{1, 1}},
		{name: "wait-and-finish", branches: []int{1}, parked: []int{1}, finished: []int{1}},
		{name: "too-many", branches: make([]int, 129)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newParallelChildCapacity(f.request.ParentRunID, f.capacity, test.branches, test.parked, test.finished); err == nil {
				t.Fatal("invalid recovered ownership accepted")
			}
		})
	}
	if f.capacity.suspends.Load() != 0 {
		t.Fatal("invalid ownership reached scheduler")
	}
}
