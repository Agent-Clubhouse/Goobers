package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}

type fakeSubmission struct {
	at      time.Time
	started time.Time
	failure bool
}
type fakeBackend struct {
	clock             *fakeClock
	submissions       []fakeSubmission
	latency, duration time.Duration
	queryErr          error
	invalid           invalidReason
	unexpected        bool
	recordErr         error
	invalidAt         time.Time
	serializeAt       time.Time
}

func (b *fakeBackend) Submit(_ context.Context, failure bool) (string, error) {
	started := b.clock.Now().Add(b.latency)
	if !b.serializeAt.IsZero() && !b.clock.Now().Before(b.serializeAt) {
		for _, s := range b.submissions {
			if finish := s.started.Add(b.duration); finish.After(started) {
				started = finish
			}
		}
	}
	b.submissions = append(b.submissions, fakeSubmission{b.clock.Now(), started, failure})
	return fmt.Sprint(len(b.submissions)), nil
}
func (b *fakeBackend) Resolve(_ context.Context, id string) (apicontract.TriggerStatusResponse, error) {
	var index int
	if _, err := fmt.Sscan(id, &index); err != nil {
		return apicontract.TriggerStatusResponse{}, err
	}
	if b.clock.Now().Before(b.submissions[index-1].started) {
		return apicontract.TriggerStatusResponse{State: "accepted", Reason: "conditions: instance max-parallel"}, nil
	}
	return apicontract.TriggerStatusResponse{State: "dispatched", RunID: id}, nil
}
func (b *fakeBackend) List(_ context.Context, o readservice.RunListOptions) ([]readservice.RunSummary, error) {
	if b.queryErr != nil {
		return nil, b.queryErr
	}
	var runs []readservice.RunSummary
	for i, s := range b.submissions {
		start := s.started
		if b.clock.Now().Before(start) {
			continue
		}
		if start.Before(o.Since) || start.After(o.Until) {
			continue
		}
		workflow := "soak"
		if s.failure {
			workflow = "soak-failure"
		}
		if workflow != o.Workflow {
			continue
		}
		r := readservice.RunSummary{ID: fmt.Sprint(i + 1), Workflow: workflow, StartedAt: start, Phase: journal.PhaseRunning}
		finish := start.Add(b.duration)
		if !b.clock.Now().Before(finish) {
			r.Terminal, r.Phase, r.FinishedAt = true, journal.PhaseCompleted, &finish
			if s.failure {
				r.Phase, r.TerminalReason = journal.PhaseFailed, fixtureFailureReason
			}
			if b.unexpected {
				r.Phase, r.TerminalReason = journal.PhaseEscalated, "harness.crash"
			}
		}
		if o.Phase == "" || o.Phase == r.Phase {
			runs = append(runs, r)
		}
	}
	return runs, nil
}
func (b *fakeBackend) Health() (invalidReason, error) {
	if b.clock.Now().Before(b.invalidAt) {
		return "", nil
	}
	return b.invalid, nil
}
func (b *fakeBackend) Record(decision) error { return b.recordErr }

func fakeSetup() (Profile, *fakeClock, *fakeBackend) {
	p := Presets["smoke"]
	c := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return p, c, &fakeBackend{clock: c, duration: 8 * time.Second}
}

func TestDriverRampsSustainsAndDrainsRealisticAdmissions(t *testing.T) {
	p, c, b := fakeSetup()
	r := run(context.Background(), p, b, c)
	if r.Verdict != "pass" || r.ExpectedFailures == nil || *r.ExpectedFailures == 0 || r.Completed == nil || *r.Completed == 0 || r.Signals.SustainedConcurrency == nil || !*r.Signals.SustainedConcurrency {
		t.Fatalf("result: %+v", r)
	}
	if c.Now().After(r.SustainEnded.Add(drainWindow)) {
		t.Fatal("unbounded drain")
	}
	secondSlot := r.Started.Add(p.RampWindow / 2)
	var earlyActive int
	for _, s := range b.submissions {
		if !s.at.Before(r.SustainEnded) {
			t.Fatal("admitted during drain")
		}
		if s.at.Before(secondSlot) && s.at.Add(b.duration).After(secondSlot.Add(-time.Second)) {
			earlyActive++
		}
	}
	if earlyActive > 1 {
		t.Fatal("second slot was not staggered")
	}
	var ramp, admittedCount int
	for _, d := range r.Admissions {
		if d.Code == heldRamp {
			ramp++
		}
		if d.Code == admitted {
			admittedCount++
		}
	}
	if ramp == 0 || admittedCount != len(b.submissions) {
		t.Fatalf("missing decision history: %+v", r.Admissions)
	}
}

func TestDriverClassifiesInvalidAndHealthFailures(t *testing.T) {
	for _, reason := range []invalidReason{containerLaunchFailed, daemonHealthFailed, loadInjectorCrashed, hostOOMKilled, observationLost} {
		t.Run(string(reason), func(t *testing.T) {
			p, c, b := fakeSetup()
			b.invalid = reason
			r := run(context.Background(), p, b, c)
			if r.Verdict != "invalid" || r.InvalidReason != reason || r.Signals.Throughput != nil || r.Signals.NoInfraEscalations != nil || r.Signals.NoWedgedRuns != nil || r.Signals.SustainedConcurrency != nil || r.LongestUnderfill != nil {
				t.Fatalf("result: %+v", r)
			}
		})
	}
	p, c, b := fakeSetup()
	b.queryErr = errors.New("read service unavailable")
	r := run(context.Background(), p, b, c)
	if r.InvalidReason != observationLost {
		t.Fatalf("result: %+v", r)
	}
}

func TestDriverUnexpectedFailureAndWedge(t *testing.T) {
	p, c, b := fakeSetup()
	b.unexpected = true
	r := run(context.Background(), p, b, c)
	if r.Verdict != "fail" || *r.Signals.NoInfraEscalations || *r.ExpectedFailures != 0 {
		t.Fatalf("result: %+v", r)
	}
	p, c, b = fakeSetup()
	b.duration = time.Hour
	r = run(context.Background(), p, b, c)
	if r.Verdict != "fail" || *r.Signals.NoWedgedRuns || *r.Signals.Throughput || len(r.Wedged) != p.Runs {
		t.Fatalf("result: %+v", r)
	}
	if !c.Now().Equal(r.SustainEnded.Add(drainWindow)) {
		t.Fatalf("drain ended at %v", c.Now())
	}
}

func TestDriverRefusesSerializedRampDespiteHealthyCompletions(t *testing.T) {
	p, c, b := fakeSetup()
	b.duration, b.serializeAt = 3*time.Second, c.Now()
	r := run(context.Background(), p, b, c)
	if r.Verdict != "fail" || !r.RampRefused || r.Signals.SustainedConcurrency != nil || r.LongestUnderfill != nil {
		t.Fatalf("serialized work passed the concurrency ramp: %+v", r)
	}
	if !*r.Signals.NoWedgedRuns || !*r.Signals.NoInfraEscalations {
		t.Fatalf("serialization must fail independently of terminal health: %+v", r)
	}
	if len(b.submissions) <= p.Runs {
		t.Fatal("regression did not exercise replacements during ramp")
	}
}

func TestDriverFailsSustainedUnderfillDespiteHealthyThroughput(t *testing.T) {
	p, c, b := fakeSetup()
	b.duration, b.serializeAt = 3*time.Second, c.Now().Add(p.RampWindow)
	r := run(context.Background(), p, b, c)
	if r.Verdict != "fail" || r.RampRefused || r.Signals.SustainedConcurrency == nil || *r.Signals.SustainedConcurrency {
		t.Fatalf("serialized replacements passed sustain: %+v", r)
	}
	if !*r.Signals.Throughput || !*r.Signals.NoWedgedRuns || !*r.Signals.NoInfraEscalations || *r.LongestUnderfill < replacementWindow {
		t.Fatalf("underfill must fail independently of the other signals: %+v", r)
	}
}

func TestOccupancyUsesActualIntervalsAndContinuousUnderfill(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		intervals [][2]int
		reached   bool
		gap       time.Duration
	}{
		{"serial touching boundaries", [][2]int{{0, 10}, {10, 20}, {20, 30}}, false, 30 * time.Second},
		{"overlap between polls", [][2]int{{0, 30}, {4, 5}, {14, 15}, {24, 25}}, true, 9 * time.Second},
		{"exact replacement deadline", [][2]int{{0, 30}, {0, 10}, {20, 30}}, true, replacementWindow},
		{"clipped intervals", [][2]int{{-10, 40}, {-5, 5}, {25, 35}}, true, 20 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := map[string]readservice.RunSummary{}
			for i, interval := range tc.intervals {
				finished := start.Add(time.Duration(interval[1]) * time.Second)
				runs[fmt.Sprint(i)] = readservice.RunSummary{StartedAt: start.Add(time.Duration(interval[0]) * time.Second), FinishedAt: &finished, Terminal: true}
			}
			reached, gap := occupancy(runs, start, start.Add(30*time.Second), 2)
			if reached != tc.reached || gap != tc.gap {
				t.Fatalf("got reached=%v gap=%v; want reached=%v gap=%v", reached, gap, tc.reached, tc.gap)
			}
		})
	}
}

func TestDriverCapacityHoldHasBoundedRampAndTracksPendingDrain(t *testing.T) {
	p, c, b := fakeSetup()
	b.latency = time.Hour
	r := run(context.Background(), p, b, c)
	if r.Verdict != "fail" || !r.RampRefused || len(r.Wedged) != p.Runs {
		t.Fatalf("result: %+v", r)
	}
	if len(b.submissions) != p.Runs {
		t.Fatal("duplicated pending acceptance on retry")
	}
	if !c.Now().Equal(r.SustainStarted.Add(drainWindow)) || r.Signals.Throughput != nil {
		t.Fatal("refused ramp continued sustaining or claimed throughput")
	}
	var held, refused bool
	for _, d := range r.Admissions {
		held = held || d.Code == heldCapacity
		refused = refused || d.Code == refusedDeadline
	}
	if !held || !refused {
		t.Fatalf("missing capacity decisions: %+v", r.Admissions)
	}
}

func TestDriverCancellationIsInvalid(t *testing.T) {
	p, c, b := fakeSetup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := run(ctx, p, b, c)
	if r.Verdict != "invalid" || r.Signals.NoWedgedRuns != nil {
		t.Fatalf("result: %+v", r)
	}
}

func TestDriverLosesInjectorMidWindowAndCannotPersistDecisions(t *testing.T) {
	p, c, b := fakeSetup()
	b.invalid, b.invalidAt = loadInjectorCrashed, c.Now().Add(40*time.Second)
	r := run(context.Background(), p, b, c)
	if r.InvalidReason != loadInjectorCrashed || len(r.Admissions) == 0 || r.Completed != nil || r.Signals.Throughput != nil {
		t.Fatalf("partial observation reported as measured: %+v", r)
	}
	p, c, b = fakeSetup()
	b.recordErr = errors.New("evidence disk full")
	r = run(context.Background(), p, b, c)
	if r.InvalidReason != observationLost || r.ExpectedFailures != nil {
		t.Fatalf("lost decision log: %+v", r)
	}
}

func TestRollingThroughputIncludesInteriorAndEdgeWindows(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		seconds []int
		want    bool
	}{
		{"healthy", []int{30, 60, 90, 120}, true},
		{"empty", nil, false},
		{"late first", []int{61, 90, 120}, false},
		{"middle stall recovered", []int{1, 80, 100, 120}, false},
		{"stalled tail", []int{1, 30, 59}, false},
		{"exact minute gap", []int{1, 61, 90}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var times []time.Time
			for _, s := range tc.seconds {
				times = append(times, start.Add(time.Duration(s)*time.Second))
			}
			if got := rollingThroughput(times, start, start.Add(120*time.Second)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// Slow I/O must not turn an incomplete observation into a permanent refusal.
func TestRampWaitsForObservationCoveringDeadline(t *testing.T) {
	for _, operation := range []string{"list", "submit"} {
		t.Run(operation, func(t *testing.T) {
			p, c, b := fakeSetup()
			start := c.Now()
			cutoff := start.Add(p.RampWindow)
			c.now = cutoff.Add(-500 * time.Millisecond)
			b.duration = time.Minute
			b.submissions = []fakeSubmission{{at: start, started: start}}
			costly := &clockAdvancingBackend{fakeBackend: b, operation: operation}
			d := driver{backend: costly, clock: c, result: result{Profile: p, Started: start, SustainStarted: cutoff, SustainEnded: cutoff.Add(p.Duration)}, slots: []slot{{acceptance: "1", runID: "1", submitted: start}, {}}, seen: map[string]readservice.RunSummary{}, observedUntil: c.Now()}
			if operation == "list" {
				b.submissions = append(b.submissions, fakeSubmission{at: c.Now(), started: cutoff.Add(-200 * time.Millisecond)})
				d.slots[1] = slot{acceptance: "2", submitted: c.Now()}
				if err := d.observe(context.Background(), c.Now()); err != nil {
					t.Fatal(err)
				}
			} else {
				b.latency = 100 * time.Millisecond
				if err := d.admit(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if c.Now().Before(cutoff) {
				t.Fatal("test did not cross the deadline during I/O")
			}
			d.checkRamp(c.Now())
			if err := d.admit(context.Background()); err != nil {
				t.Fatal(err)
			}
			if d.rampChecked || d.result.RampRefused {
				t.Fatal("ramp decided from observation ending before deadline")
			}
			if err := d.observe(context.Background(), c.Now()); err != nil {
				t.Fatal(err)
			}
			d.checkRamp(c.Now())
			if !d.rampChecked || d.result.RampRefused {
				t.Fatal("actual overlap before deadline was not accepted")
			}
		})
	}
}

type clockAdvancingBackend struct {
	*fakeBackend
	operation string
}

func (b *clockAdvancingBackend) List(ctx context.Context, opts readservice.RunListOptions) ([]readservice.RunSummary, error) {
	if b.operation == "list" {
		b.clock.now = b.clock.now.Add(600 * time.Millisecond)
		b.operation = ""
	}
	return b.fakeBackend.List(ctx, opts)
}

func (b *clockAdvancingBackend) Submit(ctx context.Context, failure bool) (string, error) {
	id, err := b.fakeBackend.Submit(ctx, failure)
	if b.operation == "submit" {
		b.clock.now = b.clock.now.Add(600 * time.Millisecond)
		b.operation = ""
	}
	return id, err
}
