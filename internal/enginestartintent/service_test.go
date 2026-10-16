package enginestartintent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type directBackend struct {
	mu       sync.Mutex
	starts   int
	observed bool
	lost     bool
	mismatch bool
	input    engine.RunInput
}

func (b *directBackend) Close() {}
func (b *directBackend) Start(_ context.Context, in engine.RunInput, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	b.input = in
	if b.lost {
		return errors.New("lost reply")
	}
	b.observed = true
	return nil
}
func (b *directBackend) Observe(context.Context, engine.RunInput, string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mismatch {
		return false, errors.New("different provider identity")
	}
	return b.observed, nil
}
func fixture(t *testing.T) (*Service, Request, *directBackend) {
	t.Helper()
	q, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	b := &directBackend{}
	r := Request{HostPort: "engine:7233", Namespace: "private", TaskQueue: "workers", Gaggle: "team", Workflow: "work", DedupeKey: "once", Binding: Digest([]byte("selectors")), Directory: t.TempDir()}
	s := &Service{Queue: q, Now: func() time.Time { return time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC) }, Open: func(context.Context, Request) (Backend, error) { return b, nil }, Capture: func(_ context.Context, r Request) (engine.RunInput, func(), error) {
		return engine.RunInput{RunID: engine.RunID(r.Gaggle, r.Workflow, r.DedupeKey), Gaggle: r.Gaggle, WorkflowName: r.Workflow, ConfigGeneration: "generation-one", InstanceID: "instance", WorkflowDigest: "workflow-one"}, func() {}, nil
	}}
	return s, r, b
}

func TestExactReplayRetainsFirstInputAndRejectsChangedTarget(t *testing.T) {
	s, r, b := fixture(t)
	record, duplicate, err := s.Accept(t.Context(), r)
	if err != nil || duplicate {
		t.Fatal(record, duplicate, err)
	}
	s.Capture = func(context.Context, Request) (engine.RunInput, func(), error) {
		t.Fatal("recaptured accepted input")
		return engine.RunInput{}, nil, nil
	}
	replayed, duplicate, err := s.Accept(t.Context(), r)
	if err != nil || !duplicate || replayed.ID != record.ID {
		t.Fatal(replayed, duplicate, err)
	}
	r.TaskQueue = "other-workers"
	if _, _, err = s.Accept(t.Context(), r); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal(err)
	}
	if err = s.Dispatch(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	finished, err := s.Queue.Get(t.Context(), record.ID, Actor)
	if err != nil || finished.State != triggerqueue.Dispatched || finished.RunID != b.input.RunID {
		t.Fatal(finished, err)
	}
	if err = s.Dispatch(t.Context(), finished); err != nil || b.starts != 1 {
		t.Fatal(err, b.starts)
	}
}

func TestLostReplyMissingHistoryNeverResendsAndLaterExactObservationSettles(t *testing.T) {
	s, r, b := fixture(t)
	b.lost = true
	record, _, err := s.Accept(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Dispatch(t.Context(), record); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	uncertain, err := s.Queue.Get(t.Context(), record.ID, Actor)
	if err != nil || uncertain.State != triggerqueue.Dispatching {
		t.Fatal(uncertain, err)
	}
	for range 3 {
		if err = s.Dispatch(t.Context(), uncertain); !errors.Is(err, ErrUncertain) {
			t.Fatal(err)
		}
	}
	if b.starts != 1 {
		t.Fatal("resent uncertain effect", b.starts)
	}
	pins, err := RetainedGenerations(t.Context(), s.Queue)
	if err != nil || !pins["generation-one"] {
		t.Fatal(pins, err)
	}
	b.observed = true
	b.mismatch = true
	if err = s.Dispatch(t.Context(), uncertain); err == nil {
		t.Fatal("acknowledged mismatched execution")
	}
	b.mismatch = false
	if err = s.Dispatch(t.Context(), uncertain); err != nil {
		t.Fatal(err)
	}
	if b.starts != 1 {
		t.Fatal(b.starts)
	}
}

func TestPreAttemptFailureRemainsAcceptedAndIndependentWritersStartOnce(t *testing.T) {
	s, r, b := fixture(t)
	record, _, err := s.Accept(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	opener := s.Open
	s.Open = func(context.Context, Request) (Backend, error) {
		return nil, errors.New("cannot dial configured frontend")
	}
	if err = s.Dispatch(t.Context(), record); err == nil {
		t.Fatal("expected pre-attempt refusal")
	}
	current, err := s.Queue.Get(t.Context(), record.ID, Actor)
	if err != nil || current.State != triggerqueue.Accepted {
		t.Fatal(current, err)
	}
	s.Open = opener
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			err := s.Dispatch(t.Context(), record)
			if err != nil && !errors.Is(err, triggerqueue.ErrTransition) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if b.starts != 1 {
		t.Fatal(b.starts)
	}
}

func TestConcurrentCapturesRetainWinningInput(t *testing.T) {
	s, request, _ := fixture(t)
	entered := make(chan struct{}, 2)
	proceed := make(chan struct{})
	var captures, released atomic.Int32
	s.Capture = func(_ context.Context, r Request) (engine.RunInput, func(), error) {
		n := captures.Add(1)
		entered <- struct{}{}
		<-proceed
		generation := "first"
		if n == 2 {
			generation = "second"
		}
		return engine.RunInput{RunID: engine.RunID(r.Gaggle, r.Workflow, r.DedupeKey), Gaggle: r.Gaggle, WorkflowName: r.Workflow, ConfigGeneration: generation, InstanceID: "instance"}, func() { released.Add(1) }, nil
	}
	records := make(chan triggerqueue.Record, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { record, _, err := s.Accept(t.Context(), request); records <- record; errs <- err })
	}
	<-entered
	<-entered
	close(proceed)
	wg.Wait()
	first, second := <-records, <-records
	if first.ID == "" || first.ID != second.ID || string(first.Payload) != string(second.Payload) {
		t.Fatal(first, second)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if released.Load() != 2 {
		t.Fatal("capture leases leaked", released.Load())
	}
}
