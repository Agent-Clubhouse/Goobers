package main

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// #5638: the failure streak reads why the stall sweep cancelled an engine run
// from the sweep's own instance-log record, because the engine journals every
// cancellation as run_canceled. An operator's cancellation of the same run
// must win, so it is never mistaken for an infrastructure interruption.
func TestEngineWatchdogInterruptionReadsTheSweepsRecord(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	request := func(runID, interrupt string) {
		t.Helper()
		if err := appendEngineRecovery(log, journal.RunIdentity{RunID: runID, Workflow: "w"}, "stopped", journal.RecoveryActionEngineCancelRequested, interrupt); err != nil {
			t.Fatal(err)
		}
	}
	request("run-expired", runner.RunDurationExceededErrorCode)
	request("run-stalled", runner.RunStalledErrorCode)
	request("run-operator", "")
	request("run-both", runner.RunStalledErrorCode)
	request("run-both", "")

	for runID, want := range map[string]string{
		"run-expired":  runner.RunDurationExceededErrorCode,
		"run-stalled":  runner.RunStalledErrorCode,
		"run-operator": "",
		"run-both":     "",
		"run-unknown":  "",
	} {
		got, err := engineWatchdogInterruption(dir, runID)
		if err != nil || got != want {
			t.Errorf("%s: interruption = %q, %v; want %q", runID, got, err, want)
		}
	}
}
