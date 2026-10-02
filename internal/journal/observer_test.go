package journal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAppendObserverDoesNotHoldWriterMutex(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	run, err := Create(t.TempDir(), testIdentity(), nil, WithAppendObserver(func(_ string, seq uint64) {
		if seq == 2 {
			close(entered)
			<-release
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	first := make(chan error, 1)
	go func() { first <- run.Append(Event{Type: EventStageStarted, Stage: "first"}) }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- run.Append(Event{Type: EventStageStarted, Stage: "second"}) }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("unrelated append blocked on derived observer")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestAsyncAppendObserverCoalescesAndDrainsHighestSequence(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var observed []uint64
	run, err := Create(t.TempDir(), testIdentity(), nil, WithAsyncAppendObserver(t.Context(), func(_ context.Context, _ string, seq uint64) {
		mu.Lock()
		observed = append(observed, seq)
		first := len(observed) == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	for range 100 {
		if err := run.Append(Event{Type: EventStageStarted, Stage: "work"}); err != nil {
			t.Fatal(err)
		}
	}
	final := run.Seq()
	close(release)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 2 || observed[1] != final {
		t.Fatalf("observed=%v final=%d", observed, final)
	}
}

func TestAsyncAppendObserverCancellationStopsPendingWork(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	canceled := make(chan error, 2)
	var once sync.Once
	run, err := Create(t.TempDir(), testIdentity(), nil, WithAsyncAppendObserver(ctx, func(ctx context.Context, _ string, _ uint64) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		canceled <- ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := run.Append(Event{Type: EventStageStarted, Stage: "work"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- run.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop derived intake")
	}
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("observer error=%v", err)
	}
	reader, err := OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 2 {
		t.Fatalf("durable events=%v err=%v", events, err)
	}
}
