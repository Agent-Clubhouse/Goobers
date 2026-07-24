package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestStatusLeavesLearnedDependenciesToBlockedList(t *testing.T) {
	root := seedBlockedRecords(t, map[string]blockedRecord{
		"510": {Blockers: []string{"441"}},
	})
	startedAt := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	writeStatusRunWithPhase(t, root, "running-run", "default-implement", "example", startedAt, journal.PhaseRunning)
	writeStatusRunWithPhase(t, root, "failed-run", "other-workflow", "example", startedAt.Add(-time.Minute), journal.PhaseFailed)

	tests := []struct {
		name       string
		args       []string
		wantFailed bool
	}{
		{name: "unfiltered", args: []string{"status", root}, wantFailed: true},
		{name: "phase filtered", args: []string{"status", "--phase=running", root}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, tt.args...)
			if code != 0 {
				t.Fatalf("status: code = %d, stderr = %q", code, stderr)
			}
			if strings.Contains(stdout, "Issues parked on learned dependencies") ||
				strings.Contains(stdout, "#510 blocked by #441") {
				t.Fatalf("status stdout = %q, want no learned dependency ledger", stdout)
			}
			if !strings.Contains(stdout, "running-run") {
				t.Fatalf("status stdout = %q, want running run", stdout)
			}
			if got := strings.Contains(stdout, "failed-run"); got != tt.wantFailed {
				t.Fatalf("status stdout = %q, failed run present = %t, want %t", stdout, got, tt.wantFailed)
			}
		})
	}

	code, stdout, stderr := runArgs(t, "blocked", "list", root)
	if code != 0 {
		t.Fatalf("blocked list: code = %d, stderr = %q", code, stderr)
	}
	if stdout != "#510 blocked by #441\n" {
		t.Fatalf("blocked list stdout = %q, want learned dependency ledger", stdout)
	}
}

func TestBlockedListDropsResolvedRecordOnBacklogEligibilityRefresh(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "510"): {
			Repository: repo,
			ItemID:     "510",
			Blockers:   []string{"441"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "resolved prerequisite")
	server.addIssue(510, "parked item")
	server.closeIssue(441)
	if _, err := refreshBlockedEligibility(context.Background(), l, provider, repo, nil); err != nil {
		t.Fatalf("refreshBlockedEligibility: %v", err)
	}

	code, stdout, stderr := runArgs(t, "blocked", "list", root)
	if code != 0 {
		t.Fatalf("blocked list: code = %d, stderr = %q", code, stderr)
	}
	if stdout != "no blocked records\n" {
		t.Fatalf("blocked list stdout = %q, want resolved record removed", stdout)
	}
}

func TestBlockedListPrunesResolvedBlockerOnBacklogEligibilityRefresh(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "510"): {
			Repository: repo,
			ItemID:     "510",
			Blockers:   []string{"441", "442"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "resolved prerequisite")
	server.addIssue(442, "open prerequisite")
	server.addIssue(510, "parked item")
	server.closeIssue(441)
	if _, err := refreshBlockedEligibility(context.Background(), l, provider, repo, nil); err != nil {
		t.Fatalf("refreshBlockedEligibility: %v", err)
	}

	code, stdout, stderr := runArgs(t, "blocked", "list", root)
	if code != 0 {
		t.Fatalf("blocked list: code = %d, stderr = %q", code, stderr)
	}
	if stdout != "#510 blocked by #442\n" {
		t.Fatalf("blocked list stdout = %q, want only the open blocker", stdout)
	}
}
