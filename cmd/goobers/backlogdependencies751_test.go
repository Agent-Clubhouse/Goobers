package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBacklogQueryNativeBlockedByEligibility(t *testing.T) {
	tests := []struct {
		name         string
		blockerState []string
		wantClaimed  bool
		wantCalls    int
	}{
		{name: "no dependency", wantClaimed: true},
		{name: "blocked by open", blockerState: []string{"open"}, wantCalls: 1},
		{name: "blocked by closed", blockerState: []string{"closed"}, wantClaimed: true, wantCalls: 1},
		{name: "mixed open and closed", blockerState: []string{"closed", "open"}, wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(7, "Candidate", "goobers:approved", "goobers:ready")
			blockerIDs := make([]int, 0, len(tt.blockerState))
			for i, state := range tt.blockerState {
				number := 8 + i
				server.addIssue(number, "Blocker")
				server.setIssueState(number, state)
				blockerIDs = append(blockerIDs, number)
			}
			server.setIssueBlockers(7, blockerIDs...)

			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
			t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
			workDir := t.TempDir()
			t.Chdir(workDir)

			code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if got := server.dependencyRequestCount(); got != tt.wantCalls {
				t.Fatalf("blocked-by API calls = %d, want %d", got, tt.wantCalls)
			}
			if tt.wantClaimed {
				if !strings.Contains(stdout, "claimed 7") {
					t.Fatalf("stdout = %q, want candidate claimed", stdout)
				}
				return
			}
			if !strings.Contains(stdout, "no work") {
				t.Fatalf("stdout = %q, want blocked candidate skipped", stdout)
			}
			assertNoWorkResultFile(t, workDir)
			if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); !os.IsNotExist(err) {
				t.Fatalf("blocked candidate created claim ledger: %v", err)
			}
		})
	}
}
