package localscheduler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// rateResetFileName is the marker, written into an instance's scheduler/
// directory, whose timestamp raises the floor of the MaxRunsPerHour rolling
// window: run.started events at or before it no longer count toward any
// workflow's budget (see Scheduler.Reconcile). It lives alongside the instance
// journal, entirely separate from runs/ (the append-only per-run journals) —
// which is the whole point of #315: an operator can reset the hourly rate
// window without destroying the durable execution record, instead of the
// `rm -rf <instance>` workaround that took runs/ down with it.
const rateResetFileName = "rate-reset"

// WriteRateReset records at as the rate-limit reset floor in schedulerDir,
// creating the directory if needed. It overwrites any prior marker — a later
// reset supersedes an earlier one — and never touches runs/. The timestamp is
// stored UTC RFC3339Nano so it round-trips exactly through ReadRateReset.
func WriteRateReset(schedulerDir string, at time.Time) error {
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return fmt.Errorf("localscheduler: rate-reset dir: %w", err)
	}
	path := filepath.Join(schedulerDir, rateResetFileName)
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		return fmt.Errorf("localscheduler: write rate-reset marker: %w", err)
	}
	return nil
}

// ReadRateReset returns the rate-limit reset floor previously written by
// WriteRateReset, if any. A missing marker is the normal case (ok=false, nil
// error) — most instances never reset. A present-but-malformed marker is an
// error, not a silent ignore: a corrupt marker should surface rather than
// quietly disable the reset an operator is relying on.
func ReadRateReset(schedulerDir string) (at time.Time, ok bool, err error) {
	path := filepath.Join(schedulerDir, rateResetFileName)
	data, rerr := os.ReadFile(path)
	if errors.Is(rerr, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if rerr != nil {
		return time.Time{}, false, fmt.Errorf("localscheduler: read rate-reset marker: %w", rerr)
	}
	parsed, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if perr != nil {
		return time.Time{}, false, fmt.Errorf("localscheduler: malformed rate-reset marker %q: %w", path, perr)
	}
	return parsed, true, nil
}
