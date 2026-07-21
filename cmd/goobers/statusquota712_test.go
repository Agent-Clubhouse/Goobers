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

// TestStatusSurfacesProviderQuotaPause is the CLI-level acceptance test for
// #712's 4th criterion: `goobers status` shows the paused state and resume
// time from the shared read service's durable scheduler projection.
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

// TestStatusJSONOmitsProviderQuotaPause confirms the paused-state line remains
// a plain-text-only affordance rather than becoming part of the JSON summary.
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
	if len(got) != 3 || got["warnings"] == nil || got["summary"] == nil || got["runs"] == nil {
		t.Fatalf("stdout = %q, want exactly {warnings, summary, runs} with no paused-state field", stdout)
	}
}
