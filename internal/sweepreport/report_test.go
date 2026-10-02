package sweepreport

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestSweepErrorReporterRateLimitsIdenticalConsecutiveErrors(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	reporter := New(log, "claim_recovery_failed", 3)
	repeated := errors.New("ledger unavailable")
	for range 4 {
		reporter.Report(repeated)
	}
	if got := countInstanceErrors(t, dir, "claim_recovery_failed"); got != 2 {
		t.Fatalf("reported identical errors = %d, want first and fourth ticks only", got)
	}

	reporter.Report(errors.New("ledger corrupt"))
	reporter.Report(nil)
	reporter.Report(repeated)
	if got := countInstanceErrors(t, dir, "claim_recovery_failed"); got != 4 {
		t.Fatalf("reported errors after change/reset = %d, want both reported immediately", got)
	}
}

func countInstanceErrors(t *testing.T, schedulerDir, code string) int {
	t.Helper()
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == code {
			count++
		}
	}
	return count
}
