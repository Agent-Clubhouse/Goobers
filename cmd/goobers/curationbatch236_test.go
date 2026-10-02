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
	"github.com/goobers/goobers/providers"
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

func TestForwardCurationEmptyPrimaryClaimUsesClaimedParkedFallback(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Parked but owned fallback", "goobers:approved", providers.LabelNeedsHuman)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_CURATION", "true")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", providers.LabelReady)
	t.Setenv("GOOBERS_INPUT_PARKLABELS", providers.LabelNeedsHuman)
	t.Setenv("GOOBERS_INPUT_FILTERPARKLABELS", "true")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "no work:") {
		t.Fatalf("forward curation fallback must claim custody before curate runs: stdout = %q stderr = %q", stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 7: Parked but owned fallback") {
		t.Fatalf("stdout = %q, want claimed fallback item 7", stdout)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "claimed-items.json"))
	if err != nil {
		t.Fatalf("read claimed-items.json: %v", err)
	}
	var result []map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("claimed-items.json is not a JSON array: %v", err)
	}
	if len(result) != 1 || result[0]["id"] != "7" {
		t.Fatalf("claimed-items.json = %v, want only item 7", result)
	}
	if strings.Contains(string(data), "noWork") {
		t.Fatalf("claimed-items.json = %s, must not carry noWork after claiming fallback custody", data)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	entry, held := ledger.Lookup("7")
	if !held || entry.RunID != "curation-run" {
		t.Fatalf("ledger claim = %+v, held=%v; want curation-run custody for item 7", entry, held)
	}
	server.mu.Lock()
	labels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if !hasAnyLabel(labels, []string{providers.LabelClaimed}) {
		t.Fatalf("labels after fallback claim = %v, want provider claim marker", labels)
	}
}

func TestForwardCurationFallbackAdvancesCursorWhenWindowAlreadyClaimed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for issue := 1; issue <= backlogScanCeiling+1; issue++ {
		server.addIssue(issue, fmt.Sprintf("Parked fallback %d", issue), "goobers:approved", providers.LabelNeedsHuman)
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "fallback-window-1")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_CURATION", "true")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", providers.LabelReady)
	t.Setenv("GOOBERS_INPUT_PARKLABELS", providers.LabelNeedsHuman)
	t.Setenv("GOOBERS_INPUT_FILTERPARKLABELS", "true")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", strconv.Itoa(backlogScanCeiling))
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("first backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("claimed %d items", backlogScanCeiling)) {
		t.Fatalf("first stdout = %q, want first fallback window claimed", stdout)
	}

	t.Setenv("GOOBERS_RUN_ID", "fallback-window-2")
	code, stdout, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("second backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work: every eligible item is already claimed by another run") {
		t.Fatalf("second stdout = %q, want no-work against held fallback window", stdout)
	}

	t.Setenv("GOOBERS_RUN_ID", "fallback-window-3")
	code, stdout, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("third backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("claimed %d: Parked fallback %d", backlogScanCeiling+1, backlogScanCeiling+1)) {
		t.Fatalf("third stdout = %q, want resumed fallback cursor to claim issue %d", stdout, backlogScanCeiling+1)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	entry, held := ledger.Lookup(strconv.Itoa(backlogScanCeiling + 1))
	if !held || entry.RunID != "fallback-window-3" {
		t.Fatalf("fallback resume claim = %+v, held=%v; want fallback-window-3 custody for issue %d", entry, held, backlogScanCeiling+1)
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
