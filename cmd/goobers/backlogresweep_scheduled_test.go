package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestBacklogScheduledResweepRejectsLegacyInputs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     backlogQueryMode
		max      string
		interval string
		ready    string
	}{
		{"inline max", backlogQueryModeClaim, "1", "", ""},
		{"inline label", backlogQueryModeClaim, "", "", providers.LabelReady},
		{"inline interval", backlogQueryModeClaim, "", "24h", ""},
		{"scheduled interval", backlogQueryModeResweep, "1", "24h", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOBERS_INPUT_CURATION", "true")
			t.Setenv("GOOBERS_INPUT_MAXITEMS", "2")
			t.Setenv("GOOBERS_INPUT_RESWEEPMAXITEMS", tc.max)
			t.Setenv("GOOBERS_INPUT_RESWEEPINTERVAL", tc.interval)
			t.Setenv("GOOBERS_INPUT_RESWEEPREADYLABEL", tc.ready)
			_, err := readBacklogQueryPolicies(tc.mode)
			if err == nil || !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "--claim --resweep") {
				t.Fatalf("legacy input lacks migration refusal: %v", err)
			}
		})
	}
}

func TestBacklogScheduledResweepOwnsCadenceButNotForwardClaims(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Forward item", providers.LabelApproved)
	server.addIssue(7, "In-flight context", providers.LabelApproved, providers.LabelReady, inReviewStatusLabel)
	server.addIssue(8, "Other in-flight context", providers.LabelApproved, providers.LabelReady, inReviewStatusLabel)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "scheduled-resweep")
	configureCurationResweep(t, "2", "2")
	t.Setenv("GOOBERS_INPUT_RECONCILEMETADATA", "false")
	workDir := t.TempDir()
	t.Chdir(workDir)
	for turn := range 2 {
		t.Setenv("GOOBERS_RUN_ID", fmt.Sprintf("scheduled-resweep-%d", turn))
		code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root)
		if code != 0 {
			t.Fatalf("scheduled sweep %d: code=%d stderr=%q", turn, code, stderr)
		}
		items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
		if len(items) != 1 || items[0].ID != fmt.Sprint(7+turn) || !items[0].ReadOnly {
			t.Fatalf("scheduled sweep %d obeyed hidden cooldown or claimed forward work: %+v", turn, items)
		}
		if items[0].Staleness.ThresholdDays != 90 || items[0].Staleness.AutoCloseEnabled {
			t.Fatalf("skipping metadata reconciliation discarded staleness policy: %+v", items[0].Staleness)
		}
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "7", "8"} {
		if _, ok := ledger.Lookup(id); ok {
			t.Fatalf("scheduled sweep claimed forward/read-only item %s", id)
		}
	}
}

func TestBacklogScheduledResweepRejectsConflictingModes(t *testing.T) {
	if code, _, _ := runArgs(t, "backlog-query", "--resweep"); code != 2 {
		t.Fatalf("--resweep without --claim accepted: code=%d", code)
	}
	for _, mode := range []string{"--read-only", "--reconcile", "--release"} {
		if code, _, _ := runArgs(t, "backlog-query", "--resweep", mode); code != 2 {
			t.Fatalf("--resweep %s accepted: code=%d", mode, code)
		}
	}
}

func TestBacklogScheduledResweepLegacyCLIRefusesBeforeSelection(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Ready item", providers.LabelApproved, providers.LabelReady)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "legacy-resweep")
	configureCurationResweep(t, "2", "1")
	t.Chdir(t.TempDir())
	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code == 0 || !strings.Contains(stderr, "inline re-sweep is retired") {
		t.Fatalf("legacy query did not refuse: code=%d stderr=%q", code, stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.issueListRequests != 0 || server.dependencyRequests != 0 || len(server.issues[7].comments) != 0 {
		t.Fatalf("legacy configuration reached selection/mutation: issueLists=%d dependencies=%d comments=%d",
			server.issueListRequests, server.dependencyRequests, len(server.issues[7].comments))
	}
}
