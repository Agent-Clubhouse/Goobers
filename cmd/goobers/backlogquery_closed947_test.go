package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBacklogQuerySkipsClosedIssueDespiteReadyLabels is #947's defense-in-depth
// half: a closed issue that still carries goobers:approved + goobers:ready (the
// incoherent label state #947 documents, where close-out bookkeeping did not
// run) must never be claimed, regardless of its labels. The provider query
// already filters state:open, but code re-verifies it — the same SEC-047
// "don't trust the provider filter alone" discipline the label re-verify uses.
// The closed issue is given the LOWER number so plain FIFO would otherwise
// claim it first; the state re-verify is what makes the run reach the open one.
func TestBacklogQuerySkipsClosedIssueDespiteReadyLabels(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(5, "Closed but still labeled ready", "goobers:approved", "goobers:ready")
	server.closeIssue(5)
	server.addIssue(7, "Genuinely open work", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var claimed map[string]interface{}
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if claimed["id"] != "7" {
		t.Fatalf("claimed item id = %v, want \"7\" (the closed #5 must be skipped despite its ready label)", claimed["id"])
	}
}
