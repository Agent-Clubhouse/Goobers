package terminalreport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestQueueQuotaReplaySurvivesRestart(t *testing.T) {
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(provider), func(t *testing.T) {
			now := time.Now()
			dir := t.TempDir()
			var events []journal.Event
			q := Queue{Dir: dir, Now: func() time.Time { return now }, Log: func(e journal.Event) error { events = append(events, e); return nil }}
			effect := Effect{Kind: WorkFailure, Repository: providers.RepositoryRef{Provider: provider, Owner: "acme", Name: "app"}, ItemID: "42", Count: 2, RunID: "run-1"}
			calls := 0
			apply := func(_ context.Context, e Effect) error {
				calls++
				if e.Count != 2 || e.RunID != "run-1" {
					t.Fatalf("projection changed on replay: %+v", e)
				}
				if calls <= 3 {
					return &providers.RateLimitError{Provider: provider, Reset: now.Add(time.Minute)}
				}
				return nil
			}
			if err := q.Publish(context.Background(), effect, apply); err == nil {
				t.Fatal("expected deferred report")
			}
			for range 3 {
				before := calls
				// A fresh queue simulates another daemon process after restart.
				q = Queue{Dir: dir, Now: q.Now, Log: q.Log}
				if err := q.Reconcile(context.Background(), 8, apply); err != nil {
					t.Fatal(err)
				}
				if calls != before {
					t.Fatal("provider called before quota reset")
				}
				now = now.Add(2 * time.Minute)
				err := q.Reconcile(context.Background(), 8, apply)
				if (calls < 4) != (err != nil) {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			}
			if calls != 4 {
				t.Fatalf("calls=%d", calls)
			}
			if err := q.Reconcile(context.Background(), 8, apply); err != nil {
				t.Fatal(err)
			}
			if calls != 4 {
				t.Fatal("delivered report replayed")
			}
			if len(events) != 4 || events[3].Runner["kind"] != "terminal_report_delivered" {
				t.Fatalf("events=%+v", events)
			}
			paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
			if len(paths) != 0 {
				t.Fatalf("pending=%v", paths)
			}
		})
	}
}

func TestQueueNewProjectionSupersedesDeferredFailure(t *testing.T) {
	now := time.Now()
	q := Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
	e := Effect{Kind: WorkFailure, ItemID: "42", Count: 2}
	quota := func(context.Context, Effect) error { return &providers.RateLimitError{Reset: now.Add(time.Hour)} }
	if err := q.Publish(context.Background(), e, quota); err == nil {
		t.Fatal("expected quota")
	}
	e.Reset = true
	e.Count = 0
	if err := q.Publish(context.Background(), e, quota); err == nil {
		t.Fatal("expected quota")
	}
	now = now.Add(2 * time.Hour)
	if err := q.Reconcile(context.Background(), 1, func(_ context.Context, e Effect) error {
		if !e.Reset || e.Count != 0 {
			t.Fatalf("stale failure resurrected: %+v", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQueueConcurrentReplayAndCancelledWait(t *testing.T) {
	now := time.Now()
	q := Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
	e := Effect{Kind: AbortLabel, ItemID: "42", RunID: "run-1"}
	if err := q.Publish(context.Background(), e, func(context.Context, Effect) error { return errors.New("offline") }); err == nil {
		t.Fatal("expected failure")
	}
	now = now.Add(2 * time.Minute)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	apply := func(context.Context, Effect) error { calls.Add(1); close(started); <-release; return nil }
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := q.Reconcile(context.Background(), 8, apply); err != nil {
			t.Error(err)
		}
	})
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.withEntry(ctx, effectKey(e), func(string) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	wg.Go(func() {
		if err := q.Reconcile(context.Background(), 8, apply); err != nil {
			t.Error(err)
		}
	})
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("writes=%d", calls.Load())
	}
}

func TestQueuePersistsBeforeCallingProviderAndRetainsOnLogFailure(t *testing.T) {
	dir := t.TempDir()
	q := Queue{Dir: filepath.Join(dir, "queue"), Log: func(journal.Event) error { return errors.New("log unavailable") }}
	e := Effect{Kind: AbortLabel, ItemID: "42"}
	if err := q.Publish(context.Background(), e, func(context.Context, Effect) error {
		files, _ := filepath.Glob(filepath.Join(q.Dir, "*.json"))
		if len(files) != 1 {
			t.Fatal("intent not durable before write")
		}
		return nil
	}); err == nil {
		t.Fatal("expected logging error")
	}
	q.Log = nil
	if err := q.Reconcile(context.Background(), 1, func(context.Context, Effect) error { return ErrSuperseded }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocked"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	q.Dir = filepath.Join(dir, "blocked", "queue")
	if err := q.Publish(context.Background(), e, func(context.Context, Effect) error { t.Fatal("write without durable intent"); return nil }); err == nil {
		t.Fatal("expected storage error")
	}
}

func TestQueueCancelledPublisherStillPersistsIntent(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := Effect{Kind: WorkFailure, ItemID: "42", Count: 1}
	if err := q.Publish(ctx, e, func(context.Context, Effect) error { t.Fatal("called provider after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	files, _ := filepath.Glob(filepath.Join(q.Dir, "*.json"))
	if len(files) != 1 {
		t.Fatal("lost canceled terminal report")
	}
}
