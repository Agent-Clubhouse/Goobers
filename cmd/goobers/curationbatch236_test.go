package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/localscheduler"
)

// TestBacklogQueryClaimsBatchUpToMaxItems is #236's batch regression: with M>1
// eligible items and maxItems=N, `backlog-query --claim` claims exactly N (all
// under one run), writes them as a claimed-items.json ARRAY, and records N
// ledger entries — proving maxItems is honored (it was a dead input) and the
// curator's handoff artifact carries the batch.
func TestBacklogQueryClaimsBatchUpToMaxItems(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	all := []int{7, 8, 9, 10} // M=4 eligible
	for _, n := range all {
		server.addIssue(n, fmt.Sprintf("Item %d", n), "goobers:approved", "goobers:ready")
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "3")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "claimed 3 items") {
		t.Fatalf("stdout = %q, want 'claimed 3 items'", stdout)
	}

	// Result file is a JSON ARRAY of exactly maxItems items (the batch shape).
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-items.json"))
	if err != nil {
		t.Fatalf("read claimed-items.json: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(data, &arr); err != nil {
		t.Fatalf("claimed-items.json is not a JSON array: %v", err)
	}
	if len(arr) != 3 {
		t.Fatalf("claimed-items.json has %d items, want 3", len(arr))
	}

	// The ledger durably holds exactly 3 claims, all for this run.
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	held := 0
	for _, n := range all {
		if entry, ok := ledger.Lookup(strconv.Itoa(n)); ok && entry.RunID == "run-1" {
			held++
		}
	}
	if held != 3 {
		t.Fatalf("ledger holds %d claims for run-1, want 3", held)
	}
}

// TestBacklogQueryRejectsInvalidMaxItems: a non-numeric / non-positive maxItems
// fails closed rather than silently defaulting — a dead input made real must
// validate.
func TestBacklogQueryRejectsInvalidMaxItems(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Item 7", "goobers:approved", "goobers:ready")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "zero")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 || !strings.Contains(stderr, "invalid maxItems") {
		t.Fatalf("code = %d, stderr = %q; want fail-closed on invalid maxItems", code, stderr)
	}
}
