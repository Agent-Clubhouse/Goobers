package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

func loadParkedReportWorkflow(t *testing.T) apiv1.Workflow {
	t.Helper()
	set, report, err := instance.LoadConfigDir("../../reference-workflows")
	if err != nil {
		t.Fatalf("load config: %v: %v", err, report)
	}
	for _, workflow := range set.Workflows {
		if workflow.Name == "parked-item-report" {
			return workflow
		}
	}
	t.Fatal("parked report workflow missing")
	return apiv1.Workflow{}
}

func TestParkedReportScheduleRequiresOptIn(t *testing.T) {
	wf := loadParkedReportWorkflow(t)
	if len(wf.Spec.Triggers) != 1 {
		t.Fatalf("unexpected triggers: %+v", wf.Spec.Triggers)
	}
	trigger := wf.Spec.Triggers[0]
	if trigger.Type != apiv1.TriggerSchedule || !triggerDisabled(trigger) {
		t.Fatalf("report must ship with its schedule disabled: %+v", trigger)
	}
	enabled := true
	trigger.Enabled = &enabled
	if triggerDisabled(trigger) {
		t.Fatal("explicit opt-in did not enable the trigger")
	}
	schedule, err := localscheduler.ParseSchedule(trigger.Schedule)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 9, 7, 6, 41, 0, 0, time.UTC)
	first := before.Add(time.Minute)
	if got := schedule.Next(before); !got.Equal(first) {
		t.Fatalf("first report = %s, want %s", got, first)
	}
	if got := schedule.Next(first); !got.Equal(first.Add(24 * time.Hour)) {
		t.Fatalf("next report = %s, want daily cadence", got)
	}
}

func TestParkedReportReadsCandidatesWithoutMutations(t *testing.T) {
	wf := loadParkedReportWorkflow(t)
	if len(wf.Spec.Tasks) != 1 || len(wf.Spec.Gates) != 0 {
		t.Fatal("report must have no downstream mutation or agentic stages")
	}
	task := wf.Spec.Tasks[0]
	if !reflect.DeepEqual(task.Capabilities, []string{string(capability.GitHubIssuesRead)}) {
		t.Fatalf("report grants must remain read-only: %v", task.Capabilities)
	}
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Parked candidate", "goobers:approved", "goobers:needs-remediation")
	server.addIssue(2, "Unapproved", "goobers:needs-remediation")
	server.addIssue(3, "Ordinary ready work", "goobers:approved", "goobers:ready")
	server.addIssue(4, "Closed", "goobers:approved", "goobers:needs-remediation")
	server.issues[4].state = "closed"
	beforeIssues := make(map[int]fakeIssue)
	for id, item := range server.issues {
		copy := *item
		copy.labels = slices.Clone(item.labels)
		copy.comments = slices.Clone(item.comments)
		beforeIssues[id] = copy
	}
	var mutations atomic.Int32
	handler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			http.Error(w, "report attempted mutation", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	})
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "parked-report")
	for key, value := range task.Inputs {
		t.Setenv(executor.InputEnvVar(key), value)
	}
	beforeScheduler := snapshotDirectoryFiles(t, layoutFor(root).SchedulerDir())
	t.Chdir(t.TempDir())
	args := append(append([]string{}, task.Run.Command[1:]...), root)
	code, stdout, stderr := runArgs(t, args...)
	if code != 0 {
		t.Fatalf("report failed: %d %s %s", code, stdout, stderr)
	}
	data, err := os.ReadFile("parked-item-candidates.json")
	if err != nil {
		t.Fatal(err)
	}
	var report readOnlyBacklogReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.ReadOnly || report.CandidateCount != 1 || len(report.Candidates) != 1 || report.Candidates[0].ID != "1" || report.Truncated || report.ObservedAt.IsZero() {
		t.Fatalf("incorrect candidate report: %s", data)
	}
	if !report.Candidates[0].HasLabel("goobers:needs-remediation") || report.Candidates[0].HasLabel("goobers:ready") {
		t.Fatalf("report changed parked state: %s", data)
	}
	server.mu.Lock()
	afterIssues := make(map[int]fakeIssue)
	for id, item := range server.issues {
		afterIssues[id] = *item
	}
	server.mu.Unlock()
	if !reflect.DeepEqual(beforeIssues, afterIssues) || mutations.Load() != 0 {
		t.Fatalf("report mutated provider: requests=%d", mutations.Load())
	}
	if after := snapshotDirectoryFiles(t, layoutFor(root).SchedulerDir()); !reflect.DeepEqual(beforeScheduler, after) {
		t.Fatal("report changed scheduler state")
	}
}

func TestReadOnlyReportEmptyTruncatedAndWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	t.Setenv(executor.InputEnvVar("resultFile"), path)
	var stderr strings.Builder
	if code := writeReadOnlyBacklogReport(nil, true, &stderr); code != 0 {
		t.Fatalf("write report: %s", stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"candidates": []`) || !strings.Contains(string(data), `"truncated": true`) {
		t.Fatalf("empty truncated report lost coverage: %s", data)
	}
	t.Setenv(executor.InputEnvVar("resultFile"), filepath.Dir(path))
	if code := writeReadOnlyBacklogReport(nil, false, &stderr); code != 1 {
		t.Fatalf("write failure returned success: %d", code)
	}
}
