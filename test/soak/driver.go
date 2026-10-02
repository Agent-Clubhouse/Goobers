package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
)

type backend interface {
	Submit(context.Context, bool) (string, error)
	Resolve(context.Context, string) (apicontract.TriggerStatusResponse, error)
	List(context.Context, readservice.RunListOptions) ([]readservice.RunSummary, error)
	Health() (invalidReason, error)
	Record(decision) error
}

type clock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }
func (wallClock) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type slot struct {
	acceptance, runID string
	submitted         time.Time
}

type driver struct {
	backend     backend
	clock       clock
	result      result
	slots       []slot
	seen        map[string]readservice.RunSummary
	sequence    int
	logError    error
	rampChecked bool
}

func run(ctx context.Context, p Profile, b backend, c clock) result {
	d := driver{backend: b, clock: c, result: result{Profile: p, Verdict: "fail"}, slots: make([]slot, p.Runs), seen: map[string]readservice.RunSummary{}}
	d.result.Started = c.Now()
	d.result.SustainStarted = d.result.Started.Add(p.RampWindow)
	d.result.SustainEnded = d.result.SustainStarted.Add(p.Duration)
	deadline := d.result.SustainEnded.Add(drainWindow)
	for {
		now := c.Now()
		if err := ctx.Err(); err != nil {
			d.result.invalidate(observationLost, err)
			return d.result
		}
		if reason, err := b.Health(); reason != "" {
			d.result.invalidate(reason, err)
			return d.result
		}
		if err := d.observe(ctx, now); err != nil {
			d.result.invalidate(observationLost, err)
			return d.result
		}
		now = c.Now() // observation/CLI I/O consumes real wall time
		if reason, err := b.Health(); reason != "" {
			d.result.invalidate(reason, err)
			return d.result
		}
		if !now.Before(d.result.SustainStarted) {
			d.checkRamp(now)
		}
		if d.logError != nil {
			d.result.invalidate(observationLost, d.logError)
			return d.result
		}
		if d.result.RampRefused {
			deadline = d.result.SustainStarted.Add(drainWindow)
		}
		if (!now.Before(d.result.SustainEnded) || d.result.RampRefused) && d.drained() {
			break
		}
		if !now.Before(deadline) {
			break
		}
		if now.Before(d.result.SustainEnded) && !d.result.RampRefused {
			if err := d.admit(ctx); err != nil {
				d.result.invalidate(observationLost, err)
				return d.result
			}
		}
		if err := c.Wait(ctx, pollInterval); err != nil {
			d.result.invalidate(observationLost, err)
			return d.result
		}
	}
	d.finish()
	if d.logError != nil {
		d.result.invalidate(observationLost, d.logError)
	}
	return d.result
}

func (d *driver) observe(ctx context.Context, now time.Time) error {
	for i := range d.slots {
		s := &d.slots[i]
		if s.acceptance == "" || s.runID != "" {
			continue
		}
		status, err := d.backend.Resolve(ctx, s.acceptance)
		if err != nil {
			return err
		}
		if status.State == "rejected" {
			return fmt.Errorf("accepted trigger %s rejected: %s", s.acceptance, status.Reason)
		}
		if status.RunID == "" {
			if !d.result.RampRefused && (status.Reason == localscheduler.ReasonInstanceMaxParallel || status.Reason == localscheduler.ReasonMaxParallel) {
				d.log(now, heldCapacity, *s)
			}
			continue
		}
		s.runID = status.RunID
		d.log(now, admitted, *s)
	}
	if err := d.refresh(ctx, now); err != nil {
		return err
	}
	for i := range d.slots {
		s := &d.slots[i]
		if r, ok := d.seen[s.runID]; ok {
			if r.Terminal {
				s.acceptance, s.runID = "", ""
			}
		}
	}
	return nil
}

func (d *driver) admit(ctx context.Context) error {
	for i := range d.slots {
		now := d.clock.Now()
		if !now.Before(d.result.SustainEnded) {
			return nil
		}
		if !now.Before(d.result.SustainStarted) {
			d.checkRamp(now)
		}
		if d.result.RampRefused {
			return nil
		}
		s := &d.slots[i]
		start := d.result.Started.Add(time.Duration(i) * d.result.Profile.RampWindow / time.Duration(d.result.Profile.Runs))
		if now.Before(start) {
			d.log(now, heldRamp, *s)
			continue
		}
		if s.acceptance != "" {
			continue
		}
		d.sequence++
		acceptance, err := d.backend.Submit(ctx, d.sequence%10 == 0)
		if err != nil {
			return err
		}
		if acceptance == "" {
			return fmt.Errorf("submission returned no acceptance identity")
		}
		s.acceptance = acceptance
		s.submitted = now
	}
	return nil
}

// Keep observation work proportional to outstanding runs. Every unretired slot
// was submitted before its run started, so this lower bound cannot hide it.
// Terminal summaries stay in seen once their slots are recycled.
func (d *driver) refresh(ctx context.Context, now time.Time) error {
	since := now
	for _, s := range d.slots {
		if s.acceptance != "" && s.submitted.Before(since) {
			since = s.submitted
		}
	}
	for _, workflow := range []string{"soak", "soak-failure"} {
		for _, phase := range []journal.RunPhase{journal.PhaseRunning, journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated} {
			runs, err := d.backend.List(ctx, readservice.RunListOptions{Gaggle: "demo", Workflow: workflow, Phase: phase, Since: since, Until: now, ShowNoWork: true})
			if err != nil {
				return err
			}
			for _, r := range runs {
				d.seen[r.ID] = r
			}
		}
	}
	return nil
}

func (d *driver) checkRamp(now time.Time) {
	if d.rampChecked {
		return
	}
	d.rampChecked = true
	reached, _ := occupancy(d.seen, d.result.Started, d.result.SustainStarted, d.result.Profile.Runs)
	if !reached {
		d.result.RampRefused = true
		d.log(now, refusedDeadline, slot{})
	}
}

func (d *driver) log(now time.Time, code string, s slot) {
	entry := decision{now, code, s.acceptance, s.runID}
	d.result.Admissions = append(d.result.Admissions, entry)
	if d.logError == nil {
		d.logError = d.backend.Record(entry)
	}
}

func (d *driver) drained() bool {
	for _, s := range d.slots {
		if s.acceptance != "" {
			return false
		}
	}
	return true
}

func (d *driver) finish() {
	var completed []time.Time
	expectedFailures := 0
	for _, r := range d.seen {
		if !r.Terminal {
			continue
		}
		if r.Phase != journal.PhaseCompleted {
			if r.Workflow == "soak-failure" && r.Phase == journal.PhaseFailed && r.TerminalReason == fixtureFailureReason {
				expectedFailures++
			} else {
				d.result.UnexpectedFailures = append(d.result.UnexpectedFailures, r.ID)
			}
			continue
		}
		if r.FinishedAt != nil && !r.FinishedAt.Before(d.result.SustainStarted) && !r.FinishedAt.After(d.result.SustainEnded) {
			completed = append(completed, *r.FinishedAt)
		}
	}
	for _, s := range d.slots {
		if s.acceptance != "" {
			id := s.runID
			if id == "" {
				id = "acceptance:" + s.acceptance
			}
			d.result.Wedged = append(d.result.Wedged, id)
		}
	}
	sort.Strings(d.result.UnexpectedFailures)
	sort.Strings(d.result.Wedged)
	count := len(completed)
	d.result.Completed, d.result.ExpectedFailures = &count, &expectedFailures
	throughput := rollingThroughput(completed, d.result.SustainStarted, d.result.SustainEnded)
	rampReached, _ := occupancy(d.seen, d.result.Started, d.result.SustainStarted, d.result.Profile.Runs)
	if !rampReached && !d.result.RampRefused {
		d.result.RampRefused = true
		d.log(d.clock.Now(), refusedDeadline, slot{})
	}
	_, underfill := occupancy(d.seen, d.result.SustainStarted, d.result.SustainEnded, d.result.Profile.Runs)
	concurrency := underfill < replacementWindow
	d.result.LongestUnderfill = &underfill
	noInfra, noWedge := len(d.result.UnexpectedFailures) == 0, len(d.result.Wedged) == 0
	d.result.Signals = signals{Throughput: &throughput, NoInfraEscalations: &noInfra, NoWedgedRuns: &noWedge, SustainedConcurrency: &concurrency}
	if d.result.RampRefused {
		d.result.Signals.Throughput = nil
		d.result.Signals.SustainedConcurrency = nil
		d.result.LongestUnderfill = nil
		d.result.Completed = nil
	}
	if throughput && concurrency && noInfra && noWedge && !d.result.RampRefused {
		d.result.Verdict = "pass"
	}
}

// Reconstruct actual occupancy, including runs that finish between polls. An
// acceptance waiting for dispatch contributes nothing. Tied finish/start times
// do not manufacture positive-duration overlap. Unfinished runs occupy their
// interval through end; the separate drain signal still catches wedged runs.
func occupancy(runs map[string]readservice.RunSummary, start, end time.Time, target int) (bool, time.Duration) {
	type boundary struct {
		at    time.Time
		delta int
	}
	events := []boundary{{end, 0}}
	for _, r := range runs {
		left, right := r.StartedAt, end
		if r.Terminal {
			if r.FinishedAt == nil {
				continue
			}
			right = *r.FinishedAt
		}
		if left.Before(start) {
			left = start
		}
		if right.After(end) {
			right = end
		}
		if left.Before(right) {
			events = append(events, boundary{left, 1}, boundary{right, -1})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	previous := start
	active := 0
	reached := false
	var gap, longest time.Duration
	for _, event := range events {
		if elapsed := event.at.Sub(previous); elapsed > 0 {
			if active >= target {
				reached, gap = true, 0
			} else {
				gap += elapsed
				if gap > longest {
					longest = gap
				}
			}
		}
		active += event.delta
		previous = event.at
	}
	return reached, longest
}

// Every empty 60s interval, including either edge, fails even after recovery.
func rollingThroughput(completed []time.Time, start, end time.Time) bool {
	if len(completed) == 0 {
		return false
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].Before(completed[j]) })
	previous := start
	for _, t := range completed {
		if t.Sub(previous) >= time.Minute {
			return false
		}
		previous = t
	}
	return end.Sub(previous) < time.Minute
}
