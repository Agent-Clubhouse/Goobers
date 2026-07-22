package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestStatusAndRunsListShareRunTable(t *testing.T) {
	root := initScheduledDemo(t)
	start := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	writeStatusRunWithPhase(t, root, "old-run", "implementation", "goobers", start, journal.PhaseFailed)
	writeStatusRunWithPhase(t, root, "middle-run", "implementation", "goobers", start.Add(time.Minute), journal.PhaseFailed)
	writeStatusRunWithPhase(t, root, "new-run", "implementation", "goobers", start.Add(2*time.Minute), journal.PhaseFailed)
	writeStatusRunWithPhase(t, root, "other-run", "merge-review", "goobers", start.Add(3*time.Minute), journal.PhaseFailed)

	flags := []string{"--phase=failed", "--workflow=implementation", "--limit=2", root}
	statusCode, statusStdout, statusStderr := runArgs(t, append([]string{"status"}, flags...)...)
	runsCode, runsStdout, runsStderr := runArgs(t, append([]string{"runs", "list"}, flags...)...)
	if statusCode != 0 || runsCode != 0 {
		t.Fatalf("status code = %d, stderr = %q; runs list code = %d, stderr = %q",
			statusCode, statusStderr, runsCode, runsStderr)
	}
	for _, want := range []string{
		"Issues parked on learned dependencies: 0\n",
		"Open PRs with goobers:blocked-on-sibling: 0\n",
		"Open PRs with goobers:merge-escalated: 0\n",
	} {
		if !strings.Contains(statusStdout, want) {
			t.Fatalf("status stdout = %q, want it to contain %q", statusStdout, want)
		}
	}
	statusRunTableAt := strings.LastIndex(statusStdout, "\nRUN ID")
	runsRunTableAt := strings.LastIndex(runsStdout, "\nRUN ID")
	if statusRunTableAt == -1 || runsRunTableAt == -1 ||
		runsStdout[runsRunTableAt+1:] != statusStdout[statusRunTableAt+1:] {
		t.Fatalf("runs list stdout = %q, want status run table %q", runsStdout, statusStdout)
	}
	if !reflect.DeepEqual(warningLines(runsStdout), warningLines(statusStdout)) {
		t.Fatalf("runs list warnings = %#v, want status warnings %#v", warningLines(runsStdout), warningLines(statusStdout))
	}
	newIndex := strings.Index(statusStdout, "new-run")
	middleIndex := strings.Index(statusStdout, "middle-run")
	if newIndex == -1 || middleIndex == -1 || newIndex > middleIndex {
		t.Fatalf("shared stdout = %q, want newest run before middle run", statusStdout)
	}
	if strings.Contains(statusStdout, "old-run") || strings.Contains(statusStdout, "other-run") {
		t.Fatalf("shared stdout = %q, want filters before limit", statusStdout)
	}

	jsonFlags := append([]string{"--json"}, flags...)
	statusCode, statusStdout, statusStderr = runArgs(t, append([]string{"status"}, jsonFlags...)...)
	runsCode, runsStdout, runsStderr = runArgs(t, append([]string{"runs", "list"}, jsonFlags...)...)
	if statusCode != 0 || runsCode != 0 {
		t.Fatalf("status --json code = %d, stderr = %q; runs list --json code = %d, stderr = %q",
			statusCode, statusStderr, runsCode, runsStderr)
	}
	var statusOutput, runsOutput statusJSONOutput
	if err := json.Unmarshal([]byte(statusStdout), &statusOutput); err != nil {
		t.Fatalf("status JSON = %q: %v", statusStdout, err)
	}
	if err := json.Unmarshal([]byte(runsStdout), &runsOutput); err != nil {
		t.Fatalf("runs list JSON = %q: %v", runsStdout, err)
	}
	if statusOutput.Summary == nil || runsOutput.Summary != nil {
		t.Fatalf("status summary = %+v, runs list summary = %+v", statusOutput.Summary, runsOutput.Summary)
	}
	if !reflect.DeepEqual(runsOutput.Warnings, statusOutput.Warnings) ||
		!reflect.DeepEqual(runsOutput.Runs, statusOutput.Runs) {
		t.Fatalf("runs list output = %+v, want status warnings/runs %+v", runsOutput, statusOutput)
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
		{name: "top-level help mentions runs list", args: []string{"help"}, wantCode: 0, wantStdout: "goobers runs list"},
		{name: "top-level help mentions runs du", args: []string{"help"}, wantCode: 0, wantStdout: "goobers runs du [--json]"},
		{name: "runs help", args: []string{"runs", "help"}, wantCode: 0, wantStdout: "alias for the goobers status run table"},
		{name: "missing action", args: []string{"runs"}, wantCode: 2, wantStderr: "Usage: goobers runs"},
		{name: "unknown action", args: []string{"runs", "bogus"}, wantCode: 2, wantStderr: `unknown subcommand "bogus"`},
		{name: "negative limit", args: []string{"runs", "list", "--limit=-1"}, wantCode: 2, wantStderr: "--limit must be non-negative"},
		{name: "too many paths", args: []string{"runs", "list", "one", "two"}, wantCode: 2, wantStderr: "Usage: goobers runs list"},
		{name: "du too many paths", args: []string{"runs", "du", "one", "two"}, wantCode: 2, wantStderr: "Usage: goobers runs du"},
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

func TestRunsDULargestFirstAndJSONMatchesHuman(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	start := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	createListRun(t, l.RunsDir(), "small-run", start)
	createListRun(t, l.RunsDir(), "large-run", start)
	writeUsageFile(t, filepath.Join(l.RunsDir(), "small-run", "inputs", "item.json"), "input")
	writeUsageFile(t, filepath.Join(l.RunsDir(), "small-run", "artifacts", "small"), "artifact")
	writeUsageFile(t, filepath.Join(l.RunsDir(), "large-run", "spans", "trace.jsonl"), strings.Repeat("s", 64))
	writeUsageFile(t, filepath.Join(l.RunsDir(), "large-run", "artifacts", "large"), strings.Repeat("a", 512))

	code, stdout, stderr := runArgs(t, "runs", "du", "--json", root)
	if code != 0 {
		t.Fatalf("runs du --json: code = %d, stderr = %q", code, stderr)
	}
	var got runsDiskUsage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode runs du JSON: %v\noutput: %s", err, stdout)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("runs count = %d, want 2", len(got.Runs))
	}
	if got.Runs[0].RunID != "large-run" || got.Runs[1].RunID != "small-run" {
		t.Fatalf("run order = [%s, %s], want largest first", got.Runs[0].RunID, got.Runs[1].RunID)
	}

	var wantTotal int64
	for _, run := range got.Runs {
		runDir := filepath.Join(l.RunsDir(), run.RunID)
		wantArtifacts := fileBytes(t, filepath.Join(runDir, "artifacts"))
		wantRunTotal := fileBytes(t, runDir)
		if run.ArtifactBytes != wantArtifacts {
			t.Errorf("%s artifact bytes = %d, want %d", run.RunID, run.ArtifactBytes, wantArtifacts)
		}
		if run.JournalStateBytes != wantRunTotal-wantArtifacts {
			t.Errorf("%s journal/state bytes = %d, want %d", run.RunID, run.JournalStateBytes, wantRunTotal-wantArtifacts)
		}
		if run.TotalBytes != wantRunTotal {
			t.Errorf("%s total bytes = %d, want %d", run.RunID, run.TotalBytes, wantRunTotal)
		}
		wantTotal += wantRunTotal
	}
	if got.TotalBytes != wantTotal ||
		got.TotalBytes != got.JournalStateBytes+got.ArtifactBytes {
		t.Fatalf("aggregate = %+v, want total %d and matching category sum", got, wantTotal)
	}

	code, human, stderr := runArgs(t, "runs", "du", root)
	if code != 0 {
		t.Fatalf("runs du: code = %d, stderr = %q", code, stderr)
	}
	if strings.Index(human, "large-run") > strings.Index(human, "small-run") {
		t.Fatalf("runs du output is not largest-first:\n%s", human)
	}
	for _, run := range got.Runs {
		wantRow := fmt.Sprintf("%-34s  %18d  %14d  %11d",
			run.RunID, run.JournalStateBytes, run.ArtifactBytes, run.TotalBytes)
		if !strings.Contains(human, wantRow) {
			t.Errorf("human output missing JSON-equivalent row %q:\n%s", wantRow, human)
		}
	}
	wantTotalRow := fmt.Sprintf("%-34s  %18d  %14d  %11d",
		"runs/ total", got.JournalStateBytes, got.ArtifactBytes, got.TotalBytes)
	if !strings.Contains(human, wantTotalRow) {
		t.Errorf("human output missing total row %q:\n%s", wantTotalRow, human)
	}
}

func TestRunsDUEmptyInstanceJSON(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "runs", "du", "--json", root)
	if code != 0 {
		t.Fatalf("runs du --json: code = %d, stderr = %q", code, stderr)
	}
	if stdout != `{"runs":[],"journalStateBytes":0,"artifactBytes":0,"totalBytes":0}`+"\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRunsDUReportsIncompleteRunEntry(t *testing.T) {
	root := initDemo(t)
	runDir := filepath.Join(instance.NewLayout(root).RunsDir(), "removed-run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "runs", "du", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2 (stdout=%q, stderr=%q)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, `run entry "removed-run" disappeared or is incomplete`) ||
		!strings.Contains(stderr, "retry `goobers runs du`") {
		t.Fatalf("stderr = %q, want actionable incomplete-entry error", stderr)
	}
}

func TestMeasureRunDiskUsageReportsConcurrentRemoval(t *testing.T) {
	_, err := measureRunDiskUsage(filepath.Join(t.TempDir(), "removed-run"), "removed-run")
	if err == nil {
		t.Fatal("measureRunDiskUsage error = nil, want missing-run error")
	}
	if !strings.Contains(err.Error(), `run "removed-run" changed or was removed`) ||
		!strings.Contains(err.Error(), "retry `goobers runs du`") {
		t.Fatalf("error = %q, want actionable removal error", err)
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

func writeUsageFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fileBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	if err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	}); err != nil {
		t.Fatalf("measure %s: %v", root, err)
	}
	return total
}
