package readservice

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
)

type startupCountReader struct {
	readmodel.Reader
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	failure error
}

func (r *startupCountReader) ActiveRunCounts(ctx context.Context) ([]readmodel.WorkflowCount, error) {
	r.calls.Add(1)
	close(r.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.release:
		return []readmodel.WorkflowCount{{Gaggle: "example", Workflow: "test", Count: 3}}, r.failure
	}
}

func TestWaitForInitialActiveRunSample(t *testing.T) {
	for _, mode := range []string{"success", "sample-error", "canceled-wait", "deadline-wait"} {
		t.Run(mode, func(t *testing.T) {
			reader := &startupCountReader{entered: make(chan struct{}), release: make(chan struct{})}
			if mode == "sample-error" {
				reader.failure = errors.New("initial query failed")
			}
			service, err := NewLocal(LocalSources{Layout: instance.NewLayout(t.TempDir()), Definitions: testDefinitions(), ReadModel: reader}, func() bool { return false })
			if err != nil {
				t.Fatal(err)
			}
			if err := service.WaitForInitialActiveRunSample(context.Background()); !errors.Is(err, ErrActiveCountsUnavailable) {
				t.Fatalf("unstarted wait=%v", err)
			}
			stop := service.StartActiveRunSampler(time.Hour)
			defer func() {
				if err := stop(); err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-reader.entered:
			case <-time.After(time.Second):
				t.Fatal("sampler did not start")
			}
			if _, _, err := service.activeRunCountsWithAge(context.Background()); !errors.Is(err, ErrActiveCountsUnavailable) {
				t.Fatalf("request path with held sample=%v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "canceled-wait" {
				cancel()
			}
			if mode == "deadline-wait" {
				short, stopWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
				defer stopWait()
				ctx = short
			}
			if mode == "canceled-wait" || mode == "deadline-wait" {
				want := context.Canceled
				if mode == "deadline-wait" {
					want = context.DeadlineExceeded
				}
				if err := service.WaitForInitialActiveRunSample(ctx); !errors.Is(err, want) {
					t.Fatalf("bounded wait=%v want=%v", err, want)
				}
				// Timing out a startup waiter does not cancel the independently owned
				// sampler, start another query, or publish an invented empty sample.
				if reader.calls.Load() != 1 {
					t.Fatal("wait triggered another sample")
				}
				if _, _, err := service.activeRunCountsWithAge(context.Background()); !errors.Is(err, ErrActiveCountsUnavailable) {
					t.Fatalf("wait published count: %v", err)
				}
				ctx = context.Background()
			}
			close(reader.release)
			result := make(chan error, 1)
			go func() { result <- service.WaitForInitialActiveRunSample(ctx) }()
			select {
			case err := <-result:
				if !errors.Is(err, reader.failure) {
					t.Fatalf("initial result=%v want=%v", err, reader.failure)
				}
			case <-time.After(time.Second):
				t.Fatal("initial result not published")
			}
			if reader.failure == nil {
				counts, err := service.activeRunCounts(context.Background())
				if err != nil || len(counts) != 1 {
					t.Fatalf("sample=%v err=%v", counts, err)
				}
				for _, n := range counts {
					if n != 3 {
						t.Fatalf("count=%d", n)
					}
				}
			}
		})
	}
}

type cancellationCountReader struct {
	readmodel.Reader
	entered chan struct{}
}

func (r *cancellationCountReader) ActiveRunCounts(ctx context.Context) ([]readmodel.WorkflowCount, error) {
	close(r.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestInstanceCancellationReachesProjectedActiveRunCount(t *testing.T) {
	reader := &cancellationCountReader{entered: make(chan struct{})}
	service, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: testDefinitions(),
		ReadModel:   reader,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := service.Instance(ctx)
		result <- err
	}()
	select {
	case <-reader.entered:
	case <-time.After(time.Second):
		t.Fatal("projected active-run query did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Instance() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Instance() did not return after request cancellation")
	}
}
