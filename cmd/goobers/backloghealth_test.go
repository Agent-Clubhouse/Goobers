package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestMeasureReadyPoolDepthAndAge(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	oneHourAgo := now.Add(-time.Hour)
	threeHoursAgo := now.Add(-3 * time.Hour)
	items := []providers.WorkItem{
		{ID: "1", State: "open", Labels: []string{"goobers:ready"}, UpdatedAt: &oneHourAgo},
		{ID: "2", State: "open", Labels: []string{"goobers:ready"}, CreatedAt: &threeHoursAgo},
		{ID: "3", State: "open", Labels: []string{"goobers:needs-human"}, UpdatedAt: &threeHoursAgo},
		{ID: "4", State: "closed", Labels: []string{"goobers:ready"}, UpdatedAt: &threeHoursAgo},
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
	available := unclaimedReadyItems(append([]providers.WorkItem(nil), items...), ledger, "", "github", now)
	if got := measureReadyPool(available, "goobers:ready", now).ReadyPoolDepth; got != 1 {
		t.Fatalf("unclaimed ready depth = %d, want 1", got)
	}
}

func TestBacklogHealthCommandWritesFlatSnapshot(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Ready", "goobers:approved", "goobers:ready")
	server.addIssue(8, "Parked", "goobers:approved", "goobers:needs-human")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "health-run")
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
}
