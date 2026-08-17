package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestPRRemediationLifecycleReleasesTerminalPRClaim(t *testing.T) {
	st := &remediationCheckpointServerState{number: 77, state: "closed", merged: true}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	root := remediationCheckpointEnv(t, server.URL, false)
	if _, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{{Number: 77}}, "run-364", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	resultFile := filepath.Join(t.TempDir(), prRemediationLifecycleResultFile)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)

	code, stdout, stderr := runArgs(t, "pr-claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "this run's claimed PR #77 is no longer open") {
		t.Fatalf("stdout = %q, want entry-guard terminal PR reason", stdout)
	}
	var result prRemediationLifecycleResult
	raw, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !result.NoWork || !result.Released || result.Open {
		t.Fatalf("result = %+v, want terminal no-work with released claim", result)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, held := ledger.Lookup(pullRequestClaimKey(77)); held {
		t.Fatal("terminal PR claim remains held")
	}
}

func TestPRRemediationLifecycleKeepsOpenPRClaim(t *testing.T) {
	st := &remediationCheckpointServerState{number: 77, state: "open"}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	root := remediationCheckpointEnv(t, server.URL, false)
	if _, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{{Number: 77}}, "run-364", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), prRemediationLifecycleResultFile))

	code, stdout, stderr := runArgs(t, "pr-claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	entry, held := ledger.Lookup(pullRequestClaimKey(77))
	if !held || entry.RunID != "run-364" {
		t.Fatalf("open PR claim = %+v, held=%v; want run-364 claim retained", entry, held)
	}
}

func TestPRRemediationLifecycleExplicitReleaseIsIdempotent(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "release-run")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), prRemediationLifecycleResultFile))
	if _, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{{Number: 88}}, "release-run", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	ledgerPath := filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if ok, _, err := ledger.Claim("issue-99", "release-run", "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed non-PR claim: ok=%v err=%v", ok, err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		code, stdout, stderr := runArgs(t, "pr-claim", "--release", root)
		if code != 0 {
			t.Fatalf("attempt %d: code = %d, stdout = %q, stderr = %q", attempt, code, stdout, stderr)
		}
	}
	ledger, err = localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatalf("reopen claim ledger: %v", err)
	}
	if entry, held := ledger.Lookup("issue-99"); !held || entry.RunID != "release-run" {
		t.Fatalf("non-PR claim = %+v, held=%v; PR release must leave it untouched", entry, held)
	}
}
