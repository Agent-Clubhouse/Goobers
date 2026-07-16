package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestRunsListNewestFirstWithLimit(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	start := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	createListRun(t, l.RunsDir(), "old-run", start)
	createListRun(t, l.RunsDir(), "middle-run", start.Add(time.Minute))
	createListRun(t, l.RunsDir(), "new-run", start.Add(2*time.Minute))

	code, stdout, stderr := runArgs(t, "runs", "list", "--limit=2", root)
	if code != 0 {
		t.Fatalf("runs list: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "RUN ID") {
		t.Fatalf("runs list stdout = %q, want summary table header", stdout)
	}
	newIndex := strings.Index(stdout, "new-run")
	middleIndex := strings.Index(stdout, "middle-run")
	if newIndex == -1 || middleIndex == -1 || newIndex > middleIndex {
		t.Fatalf("runs list stdout = %q, want newest run before middle run", stdout)
	}
	if strings.Contains(stdout, "old-run") {
		t.Fatalf("runs list stdout = %q, want --limit=2 to exclude oldest run", stdout)
	}
}

func TestRunsListJSON(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	start := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	createListRun(t, l.RunsDir(), "old-run", start)
	createListRun(t, l.RunsDir(), "middle-run", start.Add(time.Minute))
	createListRun(t, l.RunsDir(), "new-run", start.Add(2*time.Minute))

	code, stdout, stderr := runArgs(t, "runs", "list", "--json", "--limit=2", root)
	if code != 0 {
		t.Fatalf("runs list --json: code = %d, stderr = %q", code, stderr)
	}
	want := fmt.Sprintf(
		`[{"runId":"new-run","workflow":"implementation","gaggle":"goobers","phase":"running","startedAt":%q},{"runId":"middle-run","workflow":"implementation","gaggle":"goobers","phase":"running","startedAt":%q}]`+"\n",
		start.Add(2*time.Minute).Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339),
	)
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunsListJSONEmptyInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "runs", "list", "--json", root)
	if code != 0 {
		t.Fatalf("runs list --json: code = %d, stderr = %q", code, stderr)
	}
	if stdout != "[]\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "[]\n")
	}
}

func TestRunsCommandUsage(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "top-level help", args: []string{"help"}, wantCode: 0, wantStdout: "goobers runs list"},
		{name: "runs help", args: []string{"runs", "help"}, wantCode: 0, wantStdout: "Usage: goobers runs"},
		{name: "missing action", args: []string{"runs"}, wantCode: 2, wantStderr: "Usage: goobers runs"},
		{name: "unknown action", args: []string{"runs", "bogus"}, wantCode: 2, wantStderr: `unknown subcommand "bogus"`},
		{name: "negative limit", args: []string{"runs", "list", "--limit=-1"}, wantCode: 2, wantStderr: "Usage: goobers runs list"},
		{name: "too many paths", args: []string{"runs", "list", "one", "two"}, wantCode: 2, wantStderr: "Usage: goobers runs list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d (stdout=%q, stderr=%q)", code, tt.wantCode, stdout, stderr)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout, tt.wantStdout) {
				t.Fatalf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.wantStderr)
			}
		})
	}
}

func createListRun(t *testing.T, runsDir, runID string, startedAt time.Time) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       startedAt,
	}, nil)
	if err != nil {
		t.Fatalf("create run %q: %v", runID, err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run %q: %v", runID, err)
	}
}
