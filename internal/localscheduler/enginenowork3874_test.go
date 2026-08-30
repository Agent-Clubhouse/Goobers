package localscheduler_test

// The far side of RunResult.NoWork (#3874, plan item E2).
//
// The engine now reports the #233 short-circuit accounting on its RunResult.
// That flag is inert on its own: it only means anything once a Starter carries
// it into StartResult and the scheduler's idle backoff consumes it. The engine
// package can prove it SETS the flag; only this package can prove the flag
// still reaches the backoff after the mapping — and the mapping is where a
// field quietly stops being copied.
//
// The test therefore walks the whole join: an engine RunResult -> the phase
// derived from its status -> StartResult -> Tick -> a suppressed poll. It is
// deliberately written against engine.RunResult rather than a hand-built
// StartResult, so deleting `NoWork: res.NoWork` from a real Starter mapping
// fails here rather than silently restoring full-rate polling on every idle
// curation lane.
//
// It lives in the EXTERNAL test package because internal/engine reaches
// internal/localscheduler transitively (engine -> dispatcher -> apicontract ->
// readservice -> localscheduler), so an in-package test importing the engine is
// an import cycle. That constraint is also the reason this is the right side of
// the seam to test from: the scheduler cannot depend on the engine, so the
// engine's contract with it has to be asserted from outside both.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// minuteSchedule fires every minute, mirroring the in-package fakeSchedule the
// other idle-backoff tests use (unreachable from here).
type minuteSchedule struct{ d time.Duration }

func (s minuteSchedule) Next(after time.Time) time.Time { return after.Add(s.d) }

// newBackoffScheduler builds a one-entry scheduler with idle backoff enabled,
// returning it and the instance-log directory its tick events land in.
func newBackoffScheduler(t *testing.T, starter localscheduler.Starter, now *time.Time) (*localscheduler.Scheduler, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	entries := []localscheduler.WorkflowEntry{{
		Workflow:  "poll",
		Schedules: []localscheduler.Schedule{minuteSchedule{d: time.Minute}},
		ScheduleBackoffs: []localscheduler.IdleBackoffConfig{{
			Enabled: true,
			Floor:   time.Minute,
			Ceiling: 4 * time.Minute,
		}},
		Starter: starter,
	}}
	return localscheduler.New(entries, log,
		localscheduler.WithClock(func() time.Time { return *now }, time.After)), dir
}

// engineStarter is the mapping the daemon's trackedStarter performs for the
// local runner, expressed for the engine's RunResult. It is the seam plan item
// D1 will make production wiring; pinning it here keeps E2's half of the
// contract — "the engine reports NoWork in a shape the scheduler can consume" —
// asserted before that wiring exists, rather than after it silently doesn't.
type engineStarter struct {
	result engine.RunResult
	starts int
}

func (s *engineStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.starts++
	phase, err := engine.PhaseForStatus(s.result.Status)
	if err != nil {
		return localscheduler.StartResult{}, err
	}
	return localscheduler.StartResult{
		Phase:          phase,
		FinalState:     s.result.FinalState,
		NoWork:         s.result.NoWork,
		FailureCode:    s.result.FailureCode,
		FailureMessage: s.result.FailureMessage,
	}, nil
}

// An engine run that short-circuited on no work must engage the scheduler's
// idle backoff exactly as a local-runner run does.
func TestEngineNoWorkResultEngagesIdleBackoff(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &engineStarter{result: engine.RunResult{
		Status:     "completed",
		FinalState: "poll",
		Steps:      1,
		NoWork:     true,
	}}
	scheduler, dir := newBackoffScheduler(t, starter, &now)

	for _, tickAt := range []time.Time{
		base.Add(time.Minute),
		base.Add(2 * time.Minute),
		base.Add(3 * time.Minute),
		base.Add(4 * time.Minute),
	} {
		now = tickAt
		scheduler.Tick(context.Background(), tickAt)
		scheduler.Wait()
	}

	if starter.starts != 3 {
		t.Fatalf("starts = %d, want 3 — the engine's NoWork never reached the backoff, so an idle lane is "+
			"re-polled at full rate", starter.starts)
	}
	if !hasIdleBackoffSkip(t, dir) {
		t.Fatal("idle backoff suppression was not journaled for an engine-reported no-work run")
	}
}

// The negative half, and the one that matters most: a run that DID work must
// not back off. A mapping that hard-coded NoWork true, or a scheduler that
// keyed on the completed phase alone, would pass the positive test and stall
// every busy lane in production.
func TestEngineWorkingResultDoesNotEngageIdleBackoff(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &engineStarter{result: engine.RunResult{
		Status:     "completed",
		FinalState: "curate",
		Steps:      4,
	}}
	scheduler, dir := newBackoffScheduler(t, starter, &now)

	ticks := []time.Time{
		base.Add(time.Minute),
		base.Add(2 * time.Minute),
		base.Add(3 * time.Minute),
		base.Add(4 * time.Minute),
	}
	for _, tickAt := range ticks {
		now = tickAt
		scheduler.Tick(context.Background(), tickAt)
		scheduler.Wait()
	}

	if starter.starts != len(ticks) {
		t.Fatalf("starts = %d, want %d — a run that completed real work must not be backed off",
			starter.starts, len(ticks))
	}
	if hasIdleBackoffSkip(t, dir) {
		t.Error("idle backoff engaged for a run that did work")
	}
}

// An engine terminal the scheduler cannot phase-map must surface as an error
// rather than as a zero phase. This is the critic's correction to finding 002
// in its operational form: the mapping keys on the journal PHASE, and a status
// with no phase is a bug to report, not a run to silently record as whatever
// the empty phase happens to sort as.
func TestEngineUnknownStatusDoesNotSilentlyMap(t *testing.T) {
	starter := &engineStarter{result: engine.RunResult{Status: "paused"}}
	if _, err := starter.Start(context.Background(), localscheduler.StartRequest{}); err == nil {
		t.Fatal("Start returned no error for a status with no journal phase; a terminal hook keyed on the " +
			"resulting empty phase would skip the run entirely")
	}
}

func hasIdleBackoffSkip(t *testing.T, dir string) bool {
	t.Helper()
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventTickSkipped && strings.HasPrefix(event.Reason, "idle backoff:") {
			return true
		}
	}
	return false
}
