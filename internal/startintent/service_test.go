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

func TestExactRetryKeepsOriginalPinsAndAuthority(t *testing.T) {
	s := intentService(t)
	request := Request{Workflow: "repair"}
	first, duplicate, err := s.Accept(t.Context(), "request", "alice", request)
	if err != nil || duplicate {
		t.Fatal(first, duplicate, err)
	}
	s.Capture = func(context.Context, Request) (Target, func(), error) {
		t.Fatal("retry recaptured mutable definitions")
		return Target{}, nil, nil
	}
	retry, duplicate, err := s.Accept(t.Context(), "request", "alice", request)
	if err != nil || !duplicate || retry.ID != first.ID || string(retry.Payload) != string(first.Payload) {
		t.Fatal(retry, duplicate, err)
	}
	for _, other := range []struct {
		actor   string
		request Request
	}{
		{"bob", request}, {"alice", Request{Workflow: "repair", Force: true}}, {"alice", Request{Workflow: "repair", Gaggle: "other"}},
	} {
		if _, _, err := s.Accept(t.Context(), "request", other.actor, other.request); !errors.Is(err, triggerqueue.ErrConflict) {
			t.Fatal(err)
		}
	}
	if err := s.Queue.BeginDispatch(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	pins, err := RetainedGenerations(t.Context(), s.Queue)
	if err != nil || !pins["generation-1"] {
		t.Fatal("uncertain dispatch lost its archive", pins, err)
	}
}

func TestConcurrentCapturesConvergeWithoutLeakingLeases(t *testing.T) {
	s := intentService(t)
	var captured, released atomic.Int32
	entered := make(chan struct{}, 2)
	proceed := make(chan struct{})
	s.Capture = func(context.Context, Request) (Target, func(), error) {
		n := captured.Add(1)
		entered <- struct{}{}
		<-proceed
		return Target{Gaggle: "own", Workflow: "repair", ConfigGeneration: strings.Repeat("generation", int(n)), WorkflowDigest: "workflow", GooberDigest: "goober"}, func() { released.Add(1) }, nil
	}
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			record, _, err := s.Accept(t.Context(), "same", "alice", Request{Workflow: "repair"})
			ids <- record.ID
			failures <- err
		})
	}
	<-entered
	<-entered
	close(proceed)
	wg.Wait()
	if a, b := <-ids, <-ids; a == "" || a != b {
		t.Fatal(a, b)
	}
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if released.Load() != 2 {
		t.Fatal("leaked capture lease", released.Load())
	}
}
