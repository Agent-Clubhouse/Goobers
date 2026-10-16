package decomposition

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// scanFake serves a fixed escalated-run set through the OfflineRuns seam.
type scanFake struct {
	readservice.OfflineRuns
	runs     []readservice.RunSummary
	details  map[string]readservice.RunDetail
	events   map[string][]readservice.RunEvent
	getErr   map[string]error
	delay    time.Duration
	inflight atomic.Int32
	peak     atomic.Int32
}

func (f *scanFake) ListRuns(_ context.Context, o readservice.RunListOptions) (readservice.RunList, error) {
	// Two pages, to exercise cursor paging.
	if o.Cursor == "" {
		return readservice.RunList{Runs: f.runs[:len(f.runs)/2], NextCursor: "p2"}, nil
	}
	return readservice.RunList{Runs: f.runs[len(f.runs)/2:]}, nil
}

func (f *scanFake) enter() func() {
	n := f.inflight.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(f.delay)
	return func() { f.inflight.Add(-1) }
}

func (f *scanFake) GetRun(_ context.Context, id string) (readservice.RunDetail, error) {
	defer f.enter()()
	if err := f.getErr[id]; err != nil {
		return readservice.RunDetail{}, err
	}
	return f.details[id], nil
}

func (f *scanFake) RunEvents(_ context.Context, id string) (readservice.EventList, error) {
	defer f.enter()()
	return readservice.EventList{Events: f.events[id]}, nil
}

func scanFixture(n int) *scanFake {
	f := &scanFake{details: map[string]readservice.RunDetail{}, events: map[string][]readservice.RunEvent{}, getErr: map[string]error{}}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("run-%03d", i)
		// Newest first in list order, so the stable sort has real work to do.
		started := base.Add(time.Duration(n-i) * time.Minute)
		f.runs = append(f.runs, readservice.RunSummary{ID: id, Workflow: "wf", StartedAt: started})
		var cause *readservice.EscalationCause
		switch i % 4 {
		case 0, 1: // qualifies when the claim exists
			cause = &readservice.EscalationCause{Selector: readservice.EscalationSelector{Kind: "stage", Name: "impl"}, CausalEventSeq: 2}
		case 2: // gate-mediated
			cause = &readservice.EscalationCause{Selector: readservice.EscalationSelector{Kind: "gate", Name: "g"}, CausalEventSeq: 2}
		} // case 3: no escalation record
		d := readservice.RunDetail{}
		d.Escalation = cause
		f.details[id] = d
		code := "NEEDS_DECOMPOSITION"
		for c := range RecognizedErrorCodes {
			code = c
			break
		}
		evs := []readservice.RunEvent{{
			Seq: 1, KnownSchema: true, Type: journal.EventStageFinished, Stage: claimStageName,
			Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"id": fmt.Sprint(100 + i), "provider": "github"},
		}, {
			Seq: 2, KnownSchema: true, Type: journal.EventStageFinished, Stage: "impl",
			Status: string(apiv1.ResultFailure), Error: &journal.ErrorDetail{Code: code, Message: "m"},
		}}
		if i%4 == 1 && i%8 == 1 { // drop the claim: must not qualify
			evs = evs[1:]
			evs[0].Seq = 2
		}
		f.events[id] = evs
	}
	return f
}

// referenceScan is the original sequential algorithm, kept verbatim as the
// oracle that the pooled scan must match.
func referenceScan(ctx context.Context, reads readservice.OfflineRuns) ([]EscalationCandidate, error) {
	runs, err := listEscalatedRuns(ctx, reads)
	if err != nil {
		return nil, err
	}
	out := make([]EscalationCandidate, 0, len(runs))
	for _, run := range runs {
		r := scanEscalatedRun(ctx, reads, run)
		if r.err != nil {
			return nil, r.err
		}
		if r.candidate != nil {
			out = append(out, *r.candidate)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

func TestFindEscalationCandidatesPooledScanMatchesSequential(t *testing.T) {
	f := scanFixture(64)
	want, err := referenceScan(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FindEscalationCandidates(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || len(want) == len(f.runs) {
		t.Fatalf("fixture should qualify some but not all runs, got %d of %d", len(want), len(f.runs))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pooled scan diverged from sequential scan:\n got %v\nwant %v", got, want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].StartedAt.Before(got[i-1].StartedAt) {
			t.Fatal("candidates are not oldest first")
		}
	}
}

func TestFindEscalationCandidatesScanIsBoundedAndConcurrent(t *testing.T) {
	f := scanFixture(40)
	f.delay = 2 * time.Millisecond
	if _, err := FindEscalationCandidates(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if p := f.peak.Load(); p > escalationScanWorkers || p < 2 {
		t.Fatalf("peak concurrent reads = %d, want 2..%d", p, escalationScanWorkers)
	}
}

func TestFindEscalationCandidatesReportsTheFirstErrorInListOrder(t *testing.T) {
	f := scanFixture(32)
	boom := errors.New("boom")
	f.getErr["run-005"] = boom
	f.getErr["run-020"] = errors.New("later")
	_, err := FindEscalationCandidates(context.Background(), f)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the run-005 error", err)
	}
}

// gatedScanFake blocks GetRun for chosen runs until their gate closes or
// their context is cancelled, reporting entry on entered.
type gatedScanFake struct {
	*scanFake
	gates   map[string]chan struct{}
	entered chan string
}

func (f *gatedScanFake) GetRun(ctx context.Context, id string) (readservice.RunDetail, error) {
	gate, ok := f.gates[id]
	if !ok {
		return f.scanFake.GetRun(ctx, id)
	}
	f.entered <- id
	select {
	case <-ctx.Done():
		return readservice.RunDetail{}, ctx.Err()
	case <-gate:
		return f.scanFake.GetRun(ctx, id)
	}
}

// A later run failing first must neither cancel nor skip an earlier run: the
// earlier run's own error is still the one reported, exactly as the
// sequential scan would report it. Runs after the failure are cancelled.
func TestEscalationScanPoolLaterFailureDoesNotMaskEarlierRun(t *testing.T) {
	base := scanFixture(32)
	boom := errors.New("boom")
	base.getErr["run-005"] = boom
	base.getErr["run-020"] = errors.New("later")
	f := &gatedScanFake{scanFake: base, gates: map[string]chan struct{}{
		"run-005": make(chan struct{}), "run-025": make(chan struct{}),
	}, entered: make(chan string)}
	runs, err := listEscalatedRuns(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	pool := newEscalationScanPool(len(runs))
	var wg sync.WaitGroup
	for _, i := range []int{5, 25} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.scan(context.Background(), f, runs, i)
		}()
		<-f.entered
	}
	pool.scan(context.Background(), f, runs, 20) // fails while 5 and 25 are in flight
	pool.scan(context.Background(), f, runs, 30) // after the failure: skipped
	close(f.gates["run-005"])
	wg.Wait()

	if pool.failed != 5 || !errors.Is(pool.results[5].err, boom) {
		t.Fatalf("failed = %d, err = %v; want run-005's error", pool.failed, pool.results[5].err)
	}
	if !errors.Is(pool.results[25].err, context.Canceled) {
		t.Fatalf("run-025 err = %v, want it cancelled by the earlier failure", pool.results[25].err)
	}
	if pool.results[30] != (escalationScanResult{}) {
		t.Fatalf("run-030 = %+v, want it skipped", pool.results[30])
	}
}

func TestFindEscalationCandidatesEmptyList(t *testing.T) {
	f := scanFixture(0)
	got, err := FindEscalationCandidates(context.Background(), f)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}
