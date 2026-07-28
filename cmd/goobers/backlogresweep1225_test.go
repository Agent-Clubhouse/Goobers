package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func configureCurationResweep(t *testing.T, maxItems, resweepMaxItems, interval string) {
	t.Helper()
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", providers.LabelReady+","+providers.LabelNeedsHuman)
	t.Setenv("GOOBERS_INPUT_MAXITEMS", maxItems)
	t.Setenv("GOOBERS_INPUT_RESWEEPMAXITEMS", resweepMaxItems)
	t.Setenv("GOOBERS_INPUT_RESWEEPINTERVAL", interval)
	t.Setenv("GOOBERS_INPUT_RESWEEPREADYLABEL", providers.LabelReady)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
	t.Setenv("GOOBERS_INPUT_STALEAFTERDAYS", "90")
	t.Setenv("GOOBERS_INPUT_STALEAUTOCLOSE", "false")
}

func readCurationItems(t *testing.T, path string) []curationClaimedItem {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read curation items: %v", err)
	}
	var items []curationClaimedItem
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("unmarshal curation items: %v; content = %s", err, data)
	}
	return items
}

func TestBacklogQueryForwardCurationKeepsFullBatchPriority(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for number := 1; number <= 3; number++ {
		server.addIssue(number, "Forward item", "goobers:approved")
	}
	server.addIssue(4, "Ready re-sweep item", "goobers:approved", providers.LabelReady)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "forward-run")
	configureCurationResweep(t, "3", "2", "24h")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stderr = %q", code, stderr)
	}
	items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
	if len(items) != 3 {
		t.Fatalf("claimed %d items, want full forward batch of 3", len(items))
	}
	if got := strings.Join([]string{items[0].ID, items[1].ID, items[2].ID}, ","); got != "1,2,3" {
		t.Fatalf("claimed ids = %s, want all forward items 1,2,3", got)
	}
	for _, item := range items {
		if item.CurationMode != "" {
			t.Fatalf("forward item %s curationMode = %q, want empty", item.ID, item.CurationMode)
		}
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.Lookup("4"); ok {
		t.Fatal("ready item 4 was claimed despite a full forward batch")
	}
}

func TestBacklogQueryResweepUsesLeftoverCapacityAfterBlockerCloses(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Forward item", "goobers:approved")
	server.addIssue(2, "Ready item with satisfied gate", "goobers:approved", providers.LabelReady)
	server.addIssue(99, "Closed blocker")
	server.setIssueBlockers(2, 99)
	server.setIssueState(99, "closed")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "leftover-run")
	configureCurationResweep(t, "2", "2", "24h")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stderr = %q", code, stderr)
	}
	items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
	if len(items) != 2 || items[0].ID != "1" || items[1].ID != "2" {
		t.Fatalf("curation items = %+v, want forward item 1 then ready item 2", items)
	}
	if items[1].CurationMode != "resweep" || items[1].ReadOnly {
		t.Fatalf("ready item mode = %q readOnly=%t, want mutable resweep", items[1].CurationMode, items[1].ReadOnly)
	}
}

func TestBacklogQueryResweepRateLimitsReadOnlyInFlightContext(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(
		7,
		"In-flight ready item",
		"goobers:approved",
		providers.LabelReady,
		inReviewStatusLabel,
	)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "resweep-run-1")
	configureCurationResweep(t, "2", "2", "24h")
	if policy, enabled, err := readBacklogResweepPolicy(2); err != nil || !enabled {
		t.Fatalf("re-sweep policy = %+v enabled=%t err=%v", policy, enabled, err)
	}
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("first backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
	if len(items) != 1 || items[0].ID != "7" ||
		items[0].CurationMode != "read-only" || !items[0].ReadOnly {
		t.Fatalf("first re-sweep items = %+v, want item 7 as read-only context", items)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.Lookup("7"); ok {
		t.Fatal("in-flight re-sweep context was claimed")
	}

	t.Setenv("GOOBERS_RUN_ID", "resweep-run-2")
	code, stdout, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("second backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "selected 1 read-only") {
		t.Fatalf("second backlog-query ignored the 24h re-sweep interval: stdout = %q", stdout)
	}
}

func TestReadBacklogResweepPolicyRejectsUnboundedInputs(t *testing.T) {
	tests := []struct {
		name     string
		maxItems string
		interval string
	}{
		{name: "exceeds total batch", maxItems: "3", interval: "24h"},
		{name: "zero interval", maxItems: "1", interval: "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOOBERS_INPUT_RESWEEPMAXITEMS", tt.maxItems)
			t.Setenv("GOOBERS_INPUT_RESWEEPINTERVAL", tt.interval)
			if _, _, err := readBacklogResweepPolicy(2); err == nil {
				t.Fatal("readBacklogResweepPolicy accepted an unbounded configuration")
			}
		})
	}
}

func TestSortBacklogResweepCandidatesComposesPriorityWithRotation(t *testing.T) {
	now := time.Now().UTC()
	items := []providers.WorkItem{
		{ID: "1"},
		{ID: "2", Labels: []string{"security"}},
		{ID: "3", Labels: []string{"security"}},
	}
	lastSwept := map[string]time.Time{
		"2": now,
	}
	if err := sortBacklogResweepCandidates(
		items,
		[]string{"security"},
		fieldpredicate.Order{},
		lastSwept,
	); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(idsOf(items), ","); got != "3,2,1" {
		t.Fatalf("re-sweep order = %s, want unswept priority item, rotated priority item, then default tier", got)
	}
}
