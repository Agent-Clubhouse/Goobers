package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestBacklogScheduledResweepLanesHaveIndependentBudgets is #4884's
// acceptance test: the dependency-recheck lane and the ready-drift lane each
// spend their own budget. A dependency recheck no longer consumes the
// ready-drift allowance (resweepMaxItems=1 used to select ONLY the blocked
// item), and the ready lane's allowance never caps how many parked items are
// rechecked.
func TestBacklogScheduledResweepLanesHaveIndependentBudgets(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "mutable-ready"
		if readOnly {
			name = "read-only-ready"
		}
		t.Run(name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			for number := 1; number <= 3; number++ {
				server.addIssue(number, "Blocked candidate", providers.LabelApproved, blockedOnSiblingLabel)
				server.addIssue(number+90, "Closed blocker")
				server.setIssueState(number+90, "closed")
				server.setIssueBlockers(number, number+90)
			}
			labels := []string{providers.LabelApproved, providers.LabelReady}
			if readOnly {
				labels = append(labels, inReviewStatusLabel)
			}
			server.addIssue(10, "Ready candidate", labels...)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "independent-resweep-budget")
			configureCurationResweep(t, "5", "1")
			workDir := t.TempDir()
			t.Chdir(workDir)
			code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root)
			if code != 0 {
				t.Fatalf("query: code=%d stderr=%q", code, stderr)
			}
			items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
			modes := map[string]string{}
			for _, item := range items {
				modes[item.ID] = item.CurationMode
			}
			for _, id := range []string{"1", "2", "3"} {
				if modes[id] != "dependency-recheck" {
					t.Fatalf("blocked item %s mode = %q, want dependency-recheck beyond resweepMaxItems=1: %+v", id, modes[id], items)
				}
			}
			wantReady := "resweep"
			if readOnly {
				wantReady = "read-only"
			}
			if modes["10"] != wantReady {
				t.Fatalf("ready item mode = %q, want %q: dependency rechecks must not starve the ready-drift lane: %+v", modes["10"], wantReady, items)
			}
		})
	}
}

// TestBacklogScheduledResweepDependencyBudgetCapsRechecks pins the other
// direction: the dependency lane is bounded by resweepDependencyMaxItems, not
// by resweepMaxItems, and exhausting it leaves the ready lane's budget whole.
func TestBacklogScheduledResweepDependencyBudgetCapsRechecks(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for number := 1; number <= 4; number++ {
		server.addIssue(number, "Blocked candidate", providers.LabelApproved, blockedOnSiblingLabel)
		server.addIssue(number+90, "Closed blocker")
		server.setIssueState(number+90, "closed")
		server.setIssueBlockers(number, number+90)
	}
	server.addIssue(10, "Ready candidate", providers.LabelApproved, providers.LabelReady)
	server.addIssue(11, "Ready candidate", providers.LabelApproved, providers.LabelReady)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "dependency-budget-cap")
	configureCurationResweep(t, "10", "2")
	t.Setenv("GOOBERS_INPUT_RESWEEPDEPENDENCYMAXITEMS", "2")
	workDir := t.TempDir()
	t.Chdir(workDir)
	code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root)
	if code != 0 {
		t.Fatalf("query: code=%d stderr=%q", code, stderr)
	}
	counts := map[string]int{}
	for _, item := range readCurationItems(t, filepath.Join(workDir, "claimed-items.json")) {
		counts[item.CurationMode]++
	}
	if counts["dependency-recheck"] != 2 || counts["resweep"] != 2 {
		t.Fatalf("lane selections = %v, want 2 dependency rechecks and 2 ready re-sweeps", counts)
	}
}

// TestBacklogScheduledResweepCapacityExcludedItemsStayFirstInRotation pins
// the shared-batch-capacity edge: when more blocked items become actionable
// than the batch has slots, the dependency lane fills the batch (both lanes
// share maxItems), and the actionable items left out are NOT recorded as
// swept, so the rotation offers them first next run. Rechecked items that are
// still blocked are recorded as usual.
func TestBacklogScheduledResweepCapacityExcludedItemsStayFirstInRotation(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for number := 1; number <= 3; number++ {
		server.addIssue(number, "Unblocked candidate", providers.LabelApproved, blockedOnSiblingLabel)
		server.addIssue(number+90, "Closed blocker")
		server.setIssueState(number+90, "closed")
		server.setIssueBlockers(number, number+90)
	}
	server.addIssue(4, "Still blocked", providers.LabelApproved, blockedOnSiblingLabel)
	server.addIssue(94, "Open blocker")
	server.setIssueBlockers(4, 94)
	server.addIssue(10, "Ready candidate", providers.LabelApproved, providers.LabelReady)
	// One forward item reserves one of the two batch slots, leaving one.
	server.addIssue(20, "Forward item", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "capacity-rotation")
	configureCurationResweep(t, "2", "1")
	workDir := t.TempDir()
	t.Chdir(workDir)
	code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root)
	if code != 0 {
		t.Fatalf("query: code=%d stderr=%q", code, stderr)
	}
	items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
	if len(items) != 1 || items[0].CurationMode != "dependency-recheck" {
		t.Fatalf("items = %+v, want the single batch slot taken by one dependency recheck", items)
	}
	selected := items[0].ID

	matches, err := filepath.Glob(filepath.Join(root, "scheduler", "backlog-resweep-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("resweep state files = %v, err=%v; want exactly one", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var state backlogResweepState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2", "3"} {
		_, swept := state.LastSweptAt[id]
		if want := id == selected; swept != want {
			t.Fatalf("item %s swept=%t, want %t (selected=%s): %v", id, swept, want, selected, state.LastSweptAt)
		}
	}
	if _, swept := state.LastSweptAt["4"]; !swept {
		t.Fatalf("still-blocked item 4 was rechecked but not recorded as swept: %v", state.LastSweptAt)
	}
}

func TestReadResweepDependencyMaxItems(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: defaultResweepDependencyMaxItems},
		{raw: "40", want: 40},
		{raw: "0", wantErr: true},
		{raw: "-1", wantErr: true},
		{raw: "251", wantErr: true},
		{raw: "many", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv("GOOBERS_INPUT_RESWEEPDEPENDENCYMAXITEMS", tt.raw)
			got, err := readResweepDependencyMaxItems()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("accepted %q", tt.raw)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %d, %v; want %d", got, err, tt.want)
			}
		})
	}
	// Unlike resweepMaxItems, the recheck budget is not bounded by maxItems:
	// only actionable rechecks take a batch slot.
	t.Setenv("GOOBERS_INPUT_RESWEEPMAXITEMS", "1")
	t.Setenv("GOOBERS_INPUT_RESWEEPINTERVAL", "")
	t.Setenv("GOOBERS_INPUT_RESWEEPDEPENDENCYMAXITEMS", "30")
	policy, enabled, err := readBacklogResweepPolicy(2)
	if err != nil || !enabled || policy.dependencyMaxItems != 30 || policy.maxItems != 1 {
		t.Fatalf("policy = %+v enabled=%t err=%v, want dependency budget 30 independent of maxItems=2", policy, enabled, err)
	}
}
