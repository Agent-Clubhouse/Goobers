package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

func TestBacklogHealthProviderDispatchesADOAndGitea(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderADO, providers.ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			root, repo := providerDispatchFixture(t, kind)
			provider, err := newBacklogHealthProvider(root, repo, backlogRepoRefForStage(root, repo), true)
			if err != nil {
				t.Fatalf("newBacklogHealthProvider(%s): %v", kind, err)
			}
			assertDispatchedProviderKind(t, provider, kind)
		})
	}
}

func TestBacklogHealthCommandRunsWithADO(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	changedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/project/_apis/wit/wiql", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workItems": []map[string]int{{"id": 42}},
		})
	})
	mux.HandleFunc("/acme/project/_apis/wit/workitems/42", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42,
			"fields": map[string]any{
				"System.WorkItemType": "Task",
				"System.Title":        "Ready backlog item",
				"System.State":        "Active",
				"System.Tags":         "goobers:approved; goobers:ready",
				"System.CreatedDate":  changedAt.Add(-time.Hour).Format(time.RFC3339),
				"System.ChangedDate":  changedAt.Format(time.RFC3339),
			},
		})
	})
	mux.HandleFunc("/acme/project/_apis/wit/workitemtypes/Task/states", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]string{{"name": "Active", "category": "InProgress"}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		if routed != repo {
			t.Fatalf("routed repo = %#v, want %#v", routed, repo)
		}
		return providers.NewADOProvider(
			routed.Owner,
			routed.Project,
			"token",
			func(provider *providers.ADOProvider) { provider.BaseURL = server.URL },
		), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, _, stderr := runArgs(t, "backlog-health", root)
	if code != 0 {
		t.Fatalf("backlog-health: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "backlog-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got backlogHealthReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ReadyPoolDepth != 1 || got.ReadyPoolStarved {
		t.Fatalf("snapshot = %#v", got)
	}
	if got.AverageReadyAgeSeconds < (2*time.Hour - time.Minute).Seconds() {
		t.Fatalf("average ready age = %f, want provider timestamp age near two hours", got.AverageReadyAgeSeconds)
	}
	if len(got.ReadyTransitions) != 0 {
		t.Fatalf("ready transitions = %#v, want no synthesized ADO transitions", got.ReadyTransitions)
	}
}

func TestMeasureReadyPoolDepthAndAge(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	oneHourAgo := now.Add(-time.Hour)
	threeHoursAgo := now.Add(-3 * time.Hour)
	justUpdated := now.Add(-time.Minute)
	items := []providers.WorkItem{
		{ID: "1", State: "open", Labels: []string{"goobers:ready"}, ReadyAt: &oneHourAgo, UpdatedAt: &justUpdated},
		{ID: "2", State: "open", Labels: []string{"goobers:ready"}, ReadyAt: &threeHoursAgo},
		{ID: "3", State: "open", Labels: []string{"goobers:needs-human"}, ReadyAt: &threeHoursAgo},
		{ID: "4", State: "closed", Labels: []string{"goobers:ready"}, ReadyAt: &threeHoursAgo},
	}

	got := measureReadyPool(items, "goobers:ready", now)
	if got.ReadyPoolDepth != 2 || got.ReadyPoolStarved {
		t.Fatalf("depth/starved = %#v", got)
	}
	if got.AverageReadyAgeSeconds != (2*time.Hour).Seconds() ||
		got.OldestReadyAgeSeconds != (3*time.Hour).Seconds() {
		t.Fatalf("ready ages = %#v", got)
	}

	empty := measureReadyPool(nil, "goobers:ready", now)
	if empty.ReadyPoolDepth != 0 || !empty.ReadyPoolStarved {
		t.Fatalf("empty pool = %#v, want starved", empty)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("1", "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim item 1: ok=%v err=%v", ok, err)
	}
	available := unclaimedReadyItems(append([]providers.WorkItem(nil), items...), ledger, apiv1.BacklogIdentity{}, "", "github", now)
	if got := measureReadyPool(available, "goobers:ready", now).ReadyPoolDepth; got != 1 {
		t.Fatalf("unclaimed ready depth = %d, want 1", got)
	}
}

func TestAnnotateReadyTimesSkipsClosedItems(t *testing.T) {
	readyAt := time.Date(2026, time.July, 23, 10, 0, 0, 0, time.UTC)
	items := []providers.WorkItem{
		{ID: "closed", State: "closed", Labels: []string{providers.LabelReady}},
		{ID: "open", State: "open", Labels: []string{providers.LabelReady}},
	}
	transitions := []providers.WorkItemLabelTransition{{
		ItemID: "open", Label: providers.LabelReady, Added: true, OccurredAt: readyAt,
	}}

	if err := annotateReadyTimes(items, providers.LabelReady, transitions); err != nil {
		t.Fatalf("annotateReadyTimes: %v", err)
	}
	if items[0].ReadyAt != nil {
		t.Fatalf("closed item readyAt = %v, want nil", items[0].ReadyAt)
	}
	if items[1].ReadyAt == nil || !items[1].ReadyAt.Equal(readyAt) {
		t.Fatalf("open item readyAt = %v, want %v", items[1].ReadyAt, readyAt)
	}

	if err := annotateReadyTimes(items[1:], providers.LabelReady, nil); err == nil {
		t.Fatal("annotateReadyTimes accepted open ready item without an active label-add event")
	}
}

func TestBacklogHealthCommandWritesFlatSnapshot(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Ready", "goobers:approved", "goobers:ready")
	server.addIssue(8, "Parked", "goobers:approved", "goobers:needs-human")
	server.addIssue(9, "Bounced", "goobers:approved", "goobers:ready")
	readyAt := time.Now().UTC().Add(-2 * time.Hour)
	server.setLabelEventTime(7, providers.LabelReady, true, readyAt)
	server.setLabelEventTime(9, providers.LabelReady, true, readyAt.Add(-time.Hour))
	if _, err := server.newGitHubProvider("token").UpdateWorkItem(
		context.Background(),
		providers.UpdateWorkItemRequest{
			Repository:   providers.RepositoryRef{Owner: "your-org", Name: "your-repo"},
			ID:           "9",
			RemoveLabels: []string{providers.LabelReady},
		},
	); err != nil {
		t.Fatalf("remove ready label: %v", err)
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "health-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "backlog-health", root)
	if code != 0 {
		t.Fatalf("backlog-health: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "backlog-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got backlogHealthReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ReadyPoolDepth != 1 || got.ReadyPoolStarved || got.ReadyPoolObservedAt == "" {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.ReadyTransitions) != 3 || !got.ReadyTransitions[0].Added ||
		!got.ReadyTransitions[1].Added || got.ReadyTransitions[2].Added {
		t.Fatalf("ready transitions = %#v", got.ReadyTransitions)
	}
	if got.AverageReadyAgeSeconds < (2*time.Hour - time.Minute).Seconds() {
		t.Fatalf("average ready age = %f, want label age near two hours", got.AverageReadyAgeSeconds)
	}
}

func TestBacklogHealthFeedbackRecuratesOnlySustainedFailures(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	readyAt := now.Add(-8 * time.Hour)
	for _, number := range []int{7, 8, 9, 10, 11} {
		server.addIssue(number, fmt.Sprintf("Issue %d", number), "goobers:approved", "goobers:ready")
		server.setLabelEventTime(number, providers.LabelReady, true, readyAt)
	}

	writeImplementationOutcomeRun(t, root, "fail-7-a", "7", journal.PhaseFailed, now.Add(-7*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-7-b", "7", journal.PhaseEscalated, now.Add(-6*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-8-a", "8", journal.PhaseFailed, now.Add(-5*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-9-a", "9", journal.PhaseFailed, now.Add(-7*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-9-b", "9", journal.PhaseFailed, now.Add(-6*time.Hour))
	writeImplementationOutcomeRun(t, root, "success-9", "9", journal.PhaseCompleted, now.Add(-5*time.Hour))
	writeImplementationOutcomeRun(t, root, "old-fail-10-a", "10", journal.PhaseFailed, now.Add(-4*time.Hour))
	writeImplementationOutcomeRun(t, root, "old-fail-10-b", "10", journal.PhaseFailed, now.Add(-3*time.Hour))
	server.setLabelEventTime(10, providers.LabelReady, true, now.Add(-2*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-11-a", "11", journal.PhaseFailed, now.Add(-7*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-11-b", "11", journal.PhaseFailed, now.Add(-6*time.Hour))
	rebuildTelemetryQueryRollup(t, root)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "feedback-run")
	t.Setenv("GOOBERS_GAGGLE", "example")
	t.Setenv(providersnapshot.EnvVar, "feedback-tick")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_IMPLEMENTATIONFAILURETHRESHOLD", "2")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "implementation-feedback.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if acquired, _, err := ledger.ClaimScoped(localscheduler.ClaimKey{
		Gaggle: "example", Provider: string(providers.ProviderGitHub), ExternalID: "11",
	}, "active-implementation", "implementation", time.Hour); err != nil || !acquired {
		t.Fatalf("reserve active issue 11: acquired=%v err=%v", acquired, err)
	}

	code, _, stderr := runArgs(t, "backlog-health", "--feedback", root)
	if code != 0 {
		t.Fatalf("backlog-health --feedback: code = %d, stderr = %q", code, stderr)
	}

	server.mu.Lock()
	labels := make(map[int][]string, 5)
	for _, number := range []int{7, 8, 9, 10, 11} {
		labels[number] = append([]string(nil), server.issues[number].labels...)
	}
	comments := append([]string(nil), server.issues[7].comments...)
	server.mu.Unlock()
	if hasAllLabels(labels[7], []string{providers.LabelReady}) {
		t.Fatalf("issue 7 labels = %v, want ready removed", labels[7])
	}
	for _, number := range []int{8, 9, 10, 11} {
		if !hasAllLabels(labels[number], []string{providers.LabelReady}) {
			t.Fatalf("issue %d labels = %v, want one-off/reset/pre-ready failures preserved", number, labels[number])
		}
	}
	if len(comments) != 1 {
		t.Fatalf("issue 7 comments = %v, want one evidence comment", comments)
	}
	comment := comments[0]
	for _, want := range []string{"fail-7-a", "fail-7-b", "curator", "For the human"} {
		if !strings.Contains(comment, want) {
			t.Errorf("feedback comment missing %q: %s", want, comment)
		}
	}

	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var report implementationFeedbackReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.ImplementationFailureThreshold != 2 || report.Recurated != 1 ||
		len(report.Items) != 1 || report.Items[0].ItemID != "7" ||
		report.Items[0].ConsecutiveFailures != 2 {
		t.Fatalf("feedback report = %#v", report)
	}

	t.Setenv("GOOBERS_RUN_ID", "next-implementation-run")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", providers.LabelReady)
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "1")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(workDir, "next-item.json"))
	code, _, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("next backlog-query: code = %d, stderr = %q", code, stderr)
	}
	nextData, err := os.ReadFile(filepath.Join(workDir, "next-item.json"))
	if err != nil {
		t.Fatal(err)
	}
	var next providers.WorkItem
	if err := json.Unmarshal(nextData, &next); err != nil {
		t.Fatal(err)
	}
	if next.ID == "7" {
		t.Fatal("chronically failing issue was claimed for an N+1 implementation attempt")
	}
}

func writeImplementationOutcomeRun(
	t *testing.T,
	root, runID, itemID string,
	status journal.RunPhase,
	startedAt time.Time,
) {
	t.Helper()
	layout := instance.NewLayout(root)
	jr, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "example",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       startedAt,
	}, nil)
	if err != nil {
		t.Fatalf("create implementation outcome run: %v", err)
	}
	defer func() { _ = jr.Close() }()

	if err := jr.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: itemID},
		Runner:      map[string]any{"operation": "claim"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	stageStatus := string(apiv1.ResultSuccess)
	if status == journal.PhaseFailed || status == journal.PhaseEscalated {
		stageStatus = string(apiv1.ResultFailure)
		if err := jr.Append(journal.Event{
			Type:    journal.EventError,
			Stage:   "implement",
			Attempt: 1,
			Error:   &journal.ErrorDetail{Code: "implementation_failed", Message: "fixture implementation failure"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: stageStatus,
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(status)}); err != nil {
		t.Fatal(err)
	}
}
