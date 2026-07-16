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

// TestBacklogQueryOldestBeyondFetchWindowIsClaimed is #532's core acceptance:
// an eligible item must never be permanently invisible just because many NEWER
// eligible items exist. The fake server models real api.github.com behavior
// (newest-first default, per_page-capped pages, Link rel="next") — before the
// fix, backlog-query's fetch window was max(maxItems, 20) filled newest-first,
// so with 120 eligible items the oldest 100 were never fetched at all and
// sortEligibleFIFO (#350) could only reorder the already-truncated tail
// (confirmed live: issue #441 sat goobers:ready 4+ hours with zero claim
// attempts). 120 items also forces the ascending fetch across a Link-header
// page boundary (per_page caps at 100), covering the paginated path, and
// exceeds curation's old effective window several times over.
func TestBacklogQueryOldestBeyondFetchWindowIsClaimed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= 120; i++ {
		server.addIssue(i, fmt.Sprintf("Item %d", i), "goobers:approved", "goobers:ready")
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-fifo")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 1:") {
		t.Fatalf("stdout = %q, want the OLDEST eligible item (1) claimed", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var claimed map[string]interface{}
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if claimed["id"] != "1" {
		t.Fatalf("claimed item id = %v, want \"1\" (oldest filed)", claimed["id"])
	}
}

// TestBacklogQueryBatchClaimsOldestFirst covers #532's second live starvation
// instance (curation): a batch claim (maxItems > 1) must take the OLDEST
// eligible items, not whichever ones happened to land inside a newest-first
// fetch window (issues #434–#463 starved 3+ hours while every curation tick
// re-scanned the newest 20).
func TestBacklogQueryBatchClaimsOldestFirst(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= 30; i++ {
		server.addIssue(i, fmt.Sprintf("Item %d", i), "goobers:approved")
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-batch")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 20 items") {
		t.Fatalf("stdout = %q, want 20 items claimed", stdout)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	for i := 1; i <= 20; i++ {
		if _, ok := ledger.Lookup(strconv.Itoa(i)); !ok {
			t.Fatalf("oldest-20 item %d not claimed — batch window is not FIFO", i)
		}
	}
	for i := 21; i <= 30; i++ {
		if _, ok := ledger.Lookup(strconv.Itoa(i)); ok {
			t.Fatalf("item %d claimed but 20 older items were eligible — claim order is not FIFO", i)
		}
	}
}
