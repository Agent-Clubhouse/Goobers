package readservice

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
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

func TestProjectedActiveSamplerDoesNotWalkRetainedRunHistory(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	projected := &activeCountReader{called: make(chan struct{}, 1)}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
		ReadModel:   projected,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	stop := service.StartActiveRunSampler(time.Hour)
	defer func() { _ = stop() }()
	select {
	case <-projected.called:
	case <-time.After(time.Second):
		t.Fatal("projected sampler did not query the read model")
	}
}

func TestDisabledProjectionRetainsHistoricalSampling(t *testing.T) {
	projected := &activeCountReader{called: make(chan struct{}, 1)}
	service, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: testDefinitions(),
		ReadModel:   projected,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	service.DisableReadModelReads()

	stop := service.StartActiveRunSampler(time.Hour)
	defer func() { _ = stop() }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, _, err := service.activeSampler.Load().Counts(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("historical sampler did not publish a sample")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-projected.called:
		t.Fatal("disabled projection was queried instead of historical run state")
	default:
	}
}

func TestTypedNilReadModelUsesHistoricalSampling(t *testing.T) {
	var store *readmodel.Store
	service, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: testDefinitions(),
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if service.sources.ReadModel != nil || service.readModelReads {
		t.Fatal("typed-nil read model selected projected sampling")
	}
}

type activeCountReader struct {
	readmodel.Reader
	called chan struct{}
}

func (r *activeCountReader) ActiveRunCounts(context.Context) ([]readmodel.WorkflowCount, error) {
	r.called <- struct{}{}
	return nil, nil
}

// TestActiveSamplerDoesNotOverlapWalks pins that a slow walk is skipped rather
// than queued. At 1x a walk is ~4s; an interval shorter than the walk would
// otherwise stack them and turn the mitigation into a load generator.
func TestActiveSamplerDoesNotOverlapWalks(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Millisecond, time.Now)
	sampler.Start()
	t.Cleanup(func() { _ = sampler.Stop() })

	// Hold the sampling lock as an in-flight walk would, then confirm refresh
	// returns immediately instead of blocking on it.
	sampler.sampling.Lock()
	done := make(chan struct{})
	go func() { sampler.refresh(context.Background()); close(done) }()
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
	if err := sampler.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := sampler.Stop(); err != nil {
		t.Fatalf("repeated Stop: %v", err)
	}
	sampler.Start()

	select {
	case <-sampler.done:
	default:
		t.Fatal("sampler did not remain stopped after repeated lifecycle calls")
	}

	stoppedBeforeStart := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Hour, time.Now)
	if err := stoppedBeforeStart.Stop(); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
	if err := stoppedBeforeStart.Stop(); err != nil {
		t.Fatalf("repeated Stop before Start: %v", err)
	}
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
			_ = sampler.Stop()
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
	stops := make(chan func() error, callers)
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
			if err := stop(); err != nil {
				t.Errorf("stop reused sampler: %v", err)
			}
		}()
	}
	shutdown.Wait()
}

func TestActiveSamplerStopCancelsInFlightWalk(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Hour, time.Now)
	started := make(chan struct{})
	returned := make(chan struct{})
	sampler.walk = func(ctx context.Context) (map[localscheduler.WorkflowIdentity]int, error) {
		close(started)
		<-ctx.Done()
		close(returned)
		return nil, ctx.Err()
	}

	sampler.Start()
	<-started
	if err := sampler.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Stop returned before the canceled walk exited")
	}
}

func TestActiveSamplerStopTimesOutOnWedgedOperation(t *testing.T) {
	sampler := newActiveRunSampler(instance.NewLayout(t.TempDir()), time.Hour, time.Now)
	sampler.stopTimeout = 20 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	sampler.walk = func(context.Context) (map[localscheduler.WorkflowIdentity]int, error) {
		close(started)
		<-release
		return nil, nil
	}

	sampler.Start()
	<-started
	if err := sampler.Stop(); !errors.Is(err, ErrActiveSamplerStopTimeout) {
		t.Fatalf("Stop error = %v, want ErrActiveSamplerStopTimeout", err)
	}

	close(release)
	select {
	case <-sampler.done:
	case <-time.After(time.Second):
		t.Fatal("sampler worker did not exit after the wedged operation returned")
	}
	if err := sampler.Stop(); err != nil {
		t.Fatalf("Stop after worker exit: %v", err)
	}
}
