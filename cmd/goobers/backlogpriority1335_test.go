package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBacklogQueryClaimsHigherSelectionPriorityAheadOfOlderItem is #1335's
// end-to-end acceptance: with selectionPriority configured, a newer item
// carrying the higher-priority label claims ahead of an older item that
// carries none — FIFO alone would have claimed the older item first.
func TestBacklogQueryClaimsHigherSelectionPriorityAheadOfOlderItem(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Older, unprioritized", "goobers:approved", "goobers:ready")
	server.addIssue(2, "Newer, security", "goobers:approved", "goobers:ready", "security")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-priority")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_SELECTIONPRIORITY", "security,bug")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 2:") {
		t.Fatalf("stdout = %q, want the security-labeled item (2) claimed ahead of older item 1", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var claimed map[string]interface{}
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if claimed["id"] != "2" {
		t.Fatalf("claimed item id = %v, want \"2\" (higher selectionPriority tier)", claimed["id"])
	}
}

// TestBacklogQueryUnsetSelectionPriorityStillClaimsOldestFirst proves #1335
// is opt-in: with selectionPriority unset, an item's labels have no bearing
// on claim order — the plain FIFO baseline is unchanged.
func TestBacklogQueryUnsetSelectionPriorityStillClaimsOldestFirst(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Older, unprioritized", "goobers:approved", "goobers:ready")
	server.addIssue(2, "Newer, security", "goobers:approved", "goobers:ready", "security")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-no-priority")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 1:") {
		t.Fatalf("stdout = %q, want the older item (1) claimed — selectionPriority unset must not affect order", stdout)
	}
}
