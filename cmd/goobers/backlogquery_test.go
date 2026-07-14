package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/localscheduler"
)

// providerCmdEnv sets the GOOBERS_* env vars the runner would inject for a
// provider-chain stage process (#131/#132) and points newGitHubProvider at a
// fake server — the harness these CLI-level integration tests share.
func providerCmdEnv(t *testing.T, server *fakeGitHubServer, credCapability, runID string) {
	t.Helper()
	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	if credCapability != "" {
		t.Setenv(credCapability, "test-token")
	}
}

// TestBacklogQueryClaimsEligibleItem is #131's core CLI-level acceptance:
// invoking `goobers backlog-query --claim` via the actual CLI entrypoint (not
// just a unit test on the underlying provider/ledger funcs) against a
// fake-provider e2e finds the one item carrying both the trust label
// (SEC-047) and the ready label, claims it in the local ledger (source of
// truth), mirrors the claim on the provider, and writes it to the declared
// result file.
func TestBacklogQueryClaimsEligibleItem(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")
	server.addIssue(8, "Untrusted item", "goobers:ready") // missing trust label

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("stdout = %q, want a mention of the claimed item", stdout)
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
		t.Fatalf("claimed item id = %v, want \"7\"", claimed["id"])
	}

	// The claim ledger — the actual source of truth (#131) — durably holds
	// the claim, not just the provider-side marker.
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	entry, ok := ledger.Lookup("7")
	if !ok || entry.RunID != "run-1" {
		t.Fatalf("ledger entry for item 7 = %+v, ok=%v, want held by run-1", entry, ok)
	}
}

// TestBacklogQueryUnlabeledItemNeverClaimed proves SEC-047 eligibility is
// enforced in code, not just documented: an item missing the trust label is
// never claimed even though it's otherwise ready.
func TestBacklogQueryUnlabeledItemNeverClaimed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(9, "Untrusted item", "goobers:ready") // no trust label

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (no eligible item), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "no eligible item") {
		t.Fatalf("stderr = %q, want a clear no-eligible-item message", stderr)
	}
}

// TestBacklogQuerySecondRunLosesTheClaimRace is #131's "two concurrent runs,
// one claim wins" acceptance criterion, driven sequentially through the CLI
// (env vars are process-global, so genuinely concurrent goroutines can't
// each carry a distinct GOOBERS_RUN_ID within one test binary) — the
// exclusivity property under test is the claim ledger's own atomicity
// (already proven under real concurrency at the ledger level by
// internal/localscheduler's TestClaimConcurrentRace), not raw goroutine
// timing.
func TestBacklogQuerySecondRunLosesTheClaimRace(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Chdir(t.TempDir())
	if code, _, stderr := runArgs(t, "backlog-query", "--claim", root); code != 0 {
		t.Fatalf("first claim: code = %d, stderr = %q", code, stderr)
	}

	t.Setenv("GOOBERS_RUN_ID", "run-2")
	t.Chdir(t.TempDir())
	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("second claim: code = %d, want 1 (already claimed by run-1), stderr = %q", code, stderr)
	}
}

// TestBacklogQueryListsWithoutClaiming proves the no-flag form is read-only:
// it reports eligible items but does not touch the claim ledger.
func TestBacklogQueryListsWithoutClaiming(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "7") {
		t.Fatalf("stdout = %q, want the eligible item listed", stdout)
	}

	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("list-only mode should not have touched the claim ledger")
	}
}
