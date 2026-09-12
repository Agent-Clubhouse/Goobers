package main

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestBacklogScheduledResweepSharesBudgetAcrossBlockedAndReady(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "mutable-ready"
		if readOnly {
			name = "read-only-ready"
		}
		t.Run(name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(1, "Blocked candidate", providers.LabelApproved, blockedOnSiblingLabel)
			server.addIssue(99, "Closed blocker")
			server.setIssueState(99, "closed")
			server.setIssueBlockers(1, 99)
			labels := []string{providers.LabelApproved, providers.LabelReady}
			if readOnly {
				labels = append(labels, inReviewStatusLabel)
			}
			server.addIssue(2, "Ready candidate", labels...)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "shared-resweep-budget")
			configureCurationResweep(t, "3", "1")
			workDir := t.TempDir()
			t.Chdir(workDir)
			code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root)
			if code != 0 {
				t.Fatalf("query: code=%d stderr=%q", code, stderr)
			}
			items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
			if len(items) != 1 || items[0].ID != "1" || items[0].CurationMode != "dependency-recheck" {
				t.Fatalf("shared resweepMaxItems=1 must select only the higher-priority blocked recheck: %+v", items)
			}
		})
	}
}
