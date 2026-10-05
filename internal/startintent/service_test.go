package startintent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func intentService(t *testing.T) *Service {
	t.Helper()
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "starts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	return &Service{Queue: queue, Now: time.Now, Capture: func(context.Context, Request) (Target, func(), error) {
		return Target{Gaggle: "own", Workflow: "repair", ConfigGeneration: "generation-1", WorkflowDigest: "workflow-1", GooberDigest: "goober-1"}, func() {}, nil
	}}
}

func TestExactRetryKeepsOriginalPinsDeadlineAndAuthority(t *testing.T) {
	s := intentService(t)
	request := Request{Workflow: "repair"}
	deadline := time.Now().Add(time.Minute)
	first, duplicate, err := s.AcceptBefore(t.Context(), "request", "alice", request, deadline)
	if err != nil || duplicate {
		t.Fatal(first, duplicate, err)
	}
	s.Capture = func(context.Context, Request) (Target, func(), error) {
		t.Fatal("exact retry captured a new target")
		return Target{}, nil, nil
	}
	retry, duplicate, err := s.Accept(t.Context(), "request", "alice", request)
	if err != nil || !duplicate || string(first.Payload) != string(retry.Payload) {
		t.Fatal(retry, duplicate, err)
	}
	e, err := Parse(retry.Payload)
	if err != nil || !e.Deadline.Equal(deadline) || e.Target.ConfigGeneration != "generation-1" {
		t.Fatal(e, err)
	}
	for _, other := range []struct {
		actor   string
		request Request
	}{{"bob", request}, {"alice", Request{Workflow: "repair", Gaggle: "other"}}, {"alice", Request{Workflow: "repair", Force: true}}} {
		if _, _, err := s.Accept(t.Context(), "request", other.actor, other.request); !errors.Is(err, triggerqueue.ErrConflict) {
			t.Fatalf("conflicting retry=%v", err)
		}
	}
	pins, err := RetainedGenerations(t.Context(), s.Queue)
	if err != nil || !pins["generation-1"] {
		t.Fatal(pins, err)
	}
	if err = s.Queue.BeginDispatch(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Queue.RecordDispatch(t.Context(), first.ID, strings.TrimPrefix(first.ID, "trigger-")); err != nil {
		t.Fatal(err)
	}
	pins, err = RetainedGenerations(t.Context(), s.Queue)
	if err != nil || !pins["generation-1"] {
		t.Fatal("uncertain start lost archive", pins, err)
	}
}

func TestConcurrentCapturesConvergeWithoutLeakingLeases(t *testing.T) {
	s := intentService(t)
	var captured, released atomic.Int32
	entered := make(chan struct{}, 2)
	barrier := make(chan struct{})
	s.Capture = func(context.Context, Request) (Target, func(), error) {
		n := captured.Add(1)
		entered <- struct{}{}
		<-barrier
		return Target{Gaggle: "own", Workflow: "repair", ConfigGeneration: strings.Repeat("g", int(n)), WorkflowDigest: "workflow", GooberDigest: "goober"}, func() { released.Add(1) }, nil
	}
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record, _, err := s.Accept(t.Context(), "same", "alice", Request{Workflow: "repair"})
			ids <- record.ID
			failures <- err
		}()
	}
	<-entered
	<-entered
	close(barrier)
	wg.Wait()
	if first, second := <-ids, <-ids; first == "" || first != second {
		t.Fatal(first, second)
	}
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if released.Load() != 2 {
		t.Fatalf("released=%d", released.Load())
	}
}

func TestExpiredQueuedIntentDoesNotBuildOrStart(t *testing.T) {
	s := intentService(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	r, _, err := s.AcceptBefore(t.Context(), "expired", "alice", Request{Workflow: "repair"}, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s.Build = func(context.Context, Target) (Prepared, error) {
		t.Fatal("expired intent built")
		return Prepared{}, nil
	}
	if err = s.Dispatch(t.Context(), t.Context(), r); err != nil {
		t.Fatal(err)
	}
	r, err = s.Queue.Get(t.Context(), r.ID, "alice")
	if err != nil || r.State != triggerqueue.Rejected {
		t.Fatal(r, err)
	}
}

func TestClosedEnvelopeRejectsCallerExecutionOverrides(t *testing.T) {
	s := intentService(t)
	r, _, err := s.Accept(t.Context(), "one", "alice", Request{Workflow: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{string(r.Payload) + `{}`, strings.TrimSuffix(string(r.Payload), "}") + `,"actor":"admin"}`, strings.Replace(string(r.Payload), `"request":{`, `"request":{"target":"mutable",`, 1)} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestInterruptedAdmissionReturnsCustodyToQueue(t *testing.T) {
	s := intentService(t)
	record, _, err := s.Accept(t.Context(), "interrupted", "alice", Request{Workflow: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Queue.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.refuseDispatch(ctx, record.ID, context.Canceled); err != nil {
		t.Fatal(err)
	}
	record, err = s.Queue.Get(t.Context(), record.ID, "alice")
	if err != nil || record.State != triggerqueue.Accepted {
		t.Fatal(record, err)
	}
}
