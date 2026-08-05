package readservice

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// TestActiveSamplerNeverWalksOnTheReadPath is the property #1741 exists for, and
// the one §17 Wave 1.4 explicitly corrects an earlier design about.
//
// A reader with no sample must get a typed error, NOT a synchronous walk. The
// earlier design "kept the 17-second scan as the no-sample fallback and so
// preserved the exact failure on a cold daemon" — a fallback taken only when the
// cache is cold is taken exactly when the instance is busiest.
func TestActiveSamplerNeverWalksOnTheReadPath(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Minute, time.Now)

	counts, age, err := sampler.Counts()
	if !errors.Is(err, ErrActiveCountsUnavailable) {
		t.Fatalf("Counts with no sample returned (%v, %v, %v); want ErrActiveCountsUnavailable.\n"+
			"Falling back to the walk here reinstates the cold-daemon failure this change removes.", counts, age, err)
	}
}

// TestActiveSamplerServesFromMemoryWithAge pins that a reader gets the last
// sample and knows how old it is — §17 Wave 1.4's "served from memory with its
// age reported", which is what lets the UI say "as of N ago" rather than
// implying the number is current.
func TestActiveSamplerServesFromMemoryWithAge(t *testing.T) {
	clock := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Minute, now)

	if err := sampler.SampleNow(context.Background()); err != nil {
		t.Fatalf("SampleNow: %v", err)
	}
	clock = clock.Add(45 * time.Second)

	_, age, err := sampler.Counts()
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if age != 45*time.Second {
		t.Fatalf("sample age = %s, want 45s", age)
	}
}

// TestActiveSamplerDoesNotOverlapWalks pins that a slow walk is skipped rather
// than queued. At 1x a walk is ~4s; an interval shorter than the walk would
// otherwise stack them and turn the mitigation into a load generator.
func TestActiveSamplerDoesNotOverlapWalks(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Millisecond, time.Now)
	sampler.Start()
	t.Cleanup(sampler.Stop)

	// Hold the sampling lock as an in-flight walk would, then confirm refresh
	// returns immediately instead of blocking on it.
	sampler.sampling.Lock()
	done := make(chan struct{})
	go func() { sampler.refresh(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh blocked while a walk was in flight; overlapping walks would compound rather than refresh")
	}
	sampler.sampling.Unlock()
}

// TestActiveSamplerSurfacesWalkErrors pins that a persistently failing sample is
// reported rather than silently serving an ever-staler success. One unreadable
// event log fails the whole walk today, so this is reachable.
func TestActiveSamplerSurfacesWalkErrors(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Minute, time.Now)
	sampler.mu.Lock()
	sampler.sample = &activeSample{takenAt: time.Now(), err: errors.New("unreadable event log")}
	sampler.mu.Unlock()

	if _, _, err := sampler.Counts(); err == nil {
		t.Fatal("a failed sample was served as a success; a stale wrong number is worse than a stated failure")
	}
}

func TestActiveSamplerLifecycleIsIdempotent(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Hour, time.Now)

	sampler.Start()
	sampler.Start()
	sampler.Stop()
	sampler.Stop()
	sampler.Start()

	select {
	case <-sampler.done:
	default:
		t.Fatal("sampler did not remain stopped after repeated lifecycle calls")
	}

	stoppedBeforeStart := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Hour, time.Now)
	stoppedBeforeStart.Stop()
	stoppedBeforeStart.Stop()
	stoppedBeforeStart.Start()
	select {
	case <-stoppedBeforeStart.done:
	default:
		t.Fatal("sampler started after it had already been stopped")
	}
}

func TestActiveSamplerConcurrentLifecycle(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Hour, time.Now)

	var calls sync.WaitGroup
	for range 50 {
		calls.Add(2)
		go func() {
			defer calls.Done()
			sampler.Start()
		}()
		go func() {
			defer calls.Done()
			sampler.Stop()
		}()
	}
	calls.Wait()

	select {
	case <-sampler.done:
	default:
		t.Fatal("sampler was not stopped after concurrent lifecycle calls")
	}
}

func TestLocalReusesActiveRunSampler(t *testing.T) {
	service, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	const callers = 50
	stops := make(chan func(), callers)
	samplers := make(chan *activeRunSampler, callers)
	var starts sync.WaitGroup
	for range callers {
		starts.Add(1)
		go func() {
			defer starts.Done()
			stops <- service.StartActiveRunSampler(time.Hour)
			samplers <- service.activeSampler.Load()
		}()
	}
	starts.Wait()
	close(stops)
	close(samplers)

	first := <-samplers
	for sampler := range samplers {
		if sampler != first {
			t.Fatal("concurrent start replaced the running sampler")
		}
	}

	var shutdown sync.WaitGroup
	for stop := range stops {
		shutdown.Add(1)
		go func() {
			defer shutdown.Done()
			stop()
		}()
	}
	shutdown.Wait()
}
