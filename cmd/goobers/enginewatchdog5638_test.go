package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
)

// #5638: the failure streak reads why the stall sweep cancelled an engine run
// from the sweep's own instance-log record, because the engine journals every
// cancellation as run_canceled. These drive the real producers: the stall
// sweep for a stall and for an expiry, and the daemon's operator cancel plane.

func TestStallSweepRecordsAStallInterruption(t *testing.T) {
	f := newStalledEngineSweepFixture(t)
	if err := f.sweep(t, &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}, f.writer(t), f.started.Add(2*stalledEngineSweepTimeout)); err != nil {
		t.Fatal(err)
	}
	if got, err := engineWatchdogInterruption(f.layout.SchedulerDir(), f.runID); err != nil || got != runner.RunStalledErrorCode {
		t.Fatalf("interruption = %q, %v; want %q", got, err, runner.RunStalledErrorCode)
	}
}

func TestStallSweepRecordsAnExpiryInterruption(t *testing.T) {
	f := newStalledEngineSweepFixture(t)
	f.maxDuration = 2 * time.Hour
	// Recent activity: the run is not stalled, only over its maximum duration.
	lastActivity := f.started.Add(f.maxDuration - 5*time.Minute)
	stage := liveOpenBatch(f.runID, "goobers", lastActivity).Ops[1]
	stage.Time = lastActivity
	writer := f.writer(t)
	if _, err := writer.Emit(context.Background(), livejournal.EmitRequest{RunID: f.runID, Gaggle: "goobers", Ops: []livejournal.Op{stage}}); err != nil {
		t.Fatalf("emit stage activity: %v", err)
	}
	writer.CloseIdle(-time.Hour)
	if err := f.sweep(t, &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}, f.writer(t), f.started.Add(f.maxDuration+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := engineWatchdogInterruption(f.layout.SchedulerDir(), f.runID); err != nil || got != runner.RunDurationExceededErrorCode {
		t.Fatalf("interruption = %q, %v; want %q", got, err, runner.RunDurationExceededErrorCode)
	}
}

// The sweep writes its record before asking the engine to cancel, so the
// engine's terminal hooks can never see the cancellation without it. A cancel
// that then fails leaves the record behind, which is harmless: no run_canceled
// terminal follows from it.
func TestStallSweepRecordsItsInterruptionBeforeCancelling(t *testing.T) {
	f := newStalledEngineSweepFixture(t)
	failing := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, cancelErr: errStallSweepCancelRefused}
	if err := f.sweep(t, failing, f.writer(t), f.started.Add(2*stalledEngineSweepTimeout)); err == nil {
		t.Fatal("a refused cancellation must be reported")
	}
	if got, err := engineWatchdogInterruption(f.layout.SchedulerDir(), f.runID); err != nil || got != runner.RunStalledErrorCode {
		t.Fatalf("interruption = %q, %v; want the record written before the cancel", got, err)
	}
}

// A real operator cancel through the daemon's cancel plane wins over a sweep
// record for the same run: the engine journals both the same way, and an
// operator's cancel must never be read as infrastructure.
func TestOperatorCancelPlaneWinsOverTheSweepsRecord(t *testing.T) {
	const runID = "engine-operator-5638"
	f := newEngineCancelFixture(t, runID, runID)
	sweepRecord := journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: "web"}
	if err := appendEngineRecovery(f.service.engine.log, sweepRecord, "run exceeded maximum duration", journal.RecoveryActionEngineCancelRequested, runner.RunDurationExceededErrorCode); err != nil {
		t.Fatal(err)
	}
	if got, _ := engineWatchdogInterruption(f.layout.SchedulerDir(), runID); got != runner.RunDurationExceededErrorCode {
		t.Fatalf("precondition: sweep record read back as %q", got)
	}
	if response := f.request(runID, `{}`); response.Code != http.StatusOK {
		t.Fatalf("operator cancel status=%d body=%s", response.Code, response.Body)
	}
	if got, err := engineWatchdogInterruption(f.layout.SchedulerDir(), runID); err != nil || got != "" {
		t.Fatalf("interruption = %q, %v; want the operator's cancel to win", got, err)
	}
}

var errStallSweepCancelRefused = errors.New("engine refused the cancellation")
