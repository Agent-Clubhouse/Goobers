package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestParseProviderQuotaResumeTimeRoundTrips(t *testing.T) {
	resetAt := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	reason := localscheduler.ReasonProviderQuota + ": resumes at " + resetAt.Format(time.RFC3339)
	got, ok := parseProviderQuotaResumeTime(reason)
	if !ok || !got.Equal(resetAt) {
		t.Fatalf("parseProviderQuotaResumeTime(%q) = %v, %v; want %v, true", reason, got, ok, resetAt)
	}
}

func TestParseProviderQuotaResumeTimeRejectsOtherReasons(t *testing.T) {
	for _, reason := range []string{
		localscheduler.ReasonMaxParallel,
		localscheduler.ReasonBudget,
		"provider-quota", // prefix alone, no ": resumes at <time>" suffix
		localscheduler.ReasonProviderQuota + ": resumes at not-a-time",
	} {
		if _, ok := parseProviderQuotaResumeTime(reason); ok {
			t.Errorf("parseProviderQuotaResumeTime(%q) = ok, want rejected", reason)
		}
	}
}

func TestProviderQuotaStatusLineActiveWindow(t *testing.T) {
	now := time.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)
	resetAt := now.Add(5 * time.Minute)
	events := []journal.Event{
		{Type: journal.EventTriggerFired, Workflow: "implementation"},
		{Type: journal.EventTickSkipped, Workflow: "implementation", Reason: localscheduler.ReasonProviderQuota + ": resumes at " + resetAt.Format(time.RFC3339)},
	}
	line := providerQuotaStatusLine(events, now)
	if !strings.Contains(line, "GitHub quota exhausted") || !strings.Contains(line, resetAt.UTC().Format(time.RFC3339)) {
		t.Fatalf("providerQuotaStatusLine = %q, want it to mention exhaustion and the resume time", line)
	}
}

func TestProviderQuotaStatusLineEmptyAfterReset(t *testing.T) {
	now := time.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)
	resetAt := now.Add(-time.Minute) // already past
	events := []journal.Event{
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + resetAt.Format(time.RFC3339)},
	}
	if line := providerQuotaStatusLine(events, now); line != "" {
		t.Fatalf("providerQuotaStatusLine after reset = %q, want empty", line)
	}
}

func TestProviderQuotaStatusLineEmptyWithoutAnySkip(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonMaxParallel},
		{Type: journal.EventRunStarted},
	}
	if line := providerQuotaStatusLine(events, time.Now()); line != "" {
		t.Fatalf("providerQuotaStatusLine with no provider-quota skip = %q, want empty", line)
	}
}

func TestProviderQuotaStatusLineUsesMostRecentSkip(t *testing.T) {
	now := time.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)
	staleReset := now.Add(-time.Hour)        // an earlier, already-passed report
	activeReset := now.Add(10 * time.Minute) // the most recent, still-active one
	events := []journal.Event{
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + staleReset.Format(time.RFC3339)},
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + activeReset.Format(time.RFC3339)},
	}
	line := providerQuotaStatusLine(events, now)
	if !strings.Contains(line, activeReset.UTC().Format(time.RFC3339)) {
		t.Fatalf("providerQuotaStatusLine = %q, want the most recent (active) resume time %v", line, activeReset)
	}
}

// TestStatusSurfacesProviderQuotaPause is the CLI-level acceptance test for
// #712's 4th criterion: `goobers status` shows the paused state and resume
// time, sourced from the instance journal (a separate process invocation
// from the live daemon — it reads what was durably journaled, not in-memory
// scheduler state).
func TestStatusSurfacesProviderQuotaPause(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	resetAt := time.Now().Add(10 * time.Minute)
	if err := instanceLog.Append(journal.Event{
		Type:     journal.EventTickSkipped,
		Workflow: "implementation",
		Reason:   localscheduler.ReasonProviderQuota + ": resumes at " + resetAt.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("append tick.skipped: %v", err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatalf("close instance log: %v", err)
	}

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "GitHub quota exhausted") || !strings.Contains(stdout, resetAt.UTC().Format(time.RFC3339)) {
		t.Fatalf("stdout = %q, want a paused-state line naming the resume time", stdout)
	}
}

// TestStatusOmitsProviderQuotaPauseAfterResume confirms the line disappears
// once the recorded resume time has passed — the same "no explicit clear
// step" contract Admit itself relies on.
func TestStatusOmitsProviderQuotaPauseAfterResume(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	pastReset := time.Now().Add(-time.Minute)
	if err := instanceLog.Append(journal.Event{
		Type:     journal.EventTickSkipped,
		Workflow: "implementation",
		Reason:   localscheduler.ReasonProviderQuota + ": resumes at " + pastReset.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("append tick.skipped: %v", err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatalf("close instance log: %v", err)
	}

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "GitHub quota exhausted") {
		t.Fatalf("stdout = %q, want no paused-state line once the resume time has passed", stdout)
	}
}

// TestStatusJSONOmitsProviderQuotaPause confirms --json mode's output stays
// exactly {warnings, runs} (#696's schema) — the paused-state line is a
// plain-text-only affordance, not a third field grafted onto the JSON shape.
func TestStatusJSONOmitsProviderQuotaPause(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	resetAt := time.Now().Add(10 * time.Minute)
	if err := instanceLog.Append(journal.Event{
		Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + resetAt.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("append tick.skipped: %v", err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatalf("close instance log: %v", err)
	}

	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal stdout %q: %v", stdout, err)
	}
	if len(got) != 2 || got["warnings"] == nil || got["runs"] == nil {
		t.Fatalf("stdout = %q, want exactly {warnings, runs} with no paused-state field", stdout)
	}
}
