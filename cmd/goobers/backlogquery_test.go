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

// TestBacklogQueryClaimFailsClosedWithoutTrustLabel proves SEC-047 fails
// CLOSED, not open: --claim with no declared trustLabel must refuse to
// claim rather than silently skip the trust check and claim anything
// eligible by requireLabels alone (an item with no trust label at all must
// still never be claimed).
func TestBacklogQueryClaimFailsClosedWithoutTrustLabel(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:ready") // no trust label at all

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	// Deliberately no GOOBERS_INPUT_TRUSTLABEL.
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on missing trustLabel), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "trustLabel is required") {
		t.Fatalf("stderr = %q, want a clear missing-trustLabel message", stderr)
	}

	// Confirm nothing was claimed.
	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("fail-closed rejection should not have touched the claim ledger")
	}
}

// TestBacklogQueryListWithoutTrustLabelStillWorks proves the fail-closed
// SEC-047 guard is scoped to --claim (the mutating, consequential action) —
// a plain read-only list doesn't require trustLabel to be declared.
func TestBacklogQueryListWithoutTrustLabelStillWorks(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "7") {
		t.Fatalf("stdout = %q, want the item listed", stdout)
	}
}

// TestBacklogQueryMissingRunIDFailsClosed proves backlog-query refuses to
// claim without a real run identity (GOOBERS_RUN_ID) rather than proceeding
// under an empty/synthetic one that could collide with a real run's claims.
func TestBacklogQueryMissingRunIDFailsClosed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	// Deliberately no GOOBERS_RUN_ID.
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on missing GOOBERS_RUN_ID), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "GOOBERS_RUN_ID") {
		t.Fatalf("stderr = %q, want a clear missing-run-id message", stderr)
	}
}

// TestBacklogQueryRejectsNonPositiveLeaseDuration is issue #235's edge 1,
// exercised at the CLI level: a workflow's leaseDuration input of "0s" (or
// any non-positive duration) must fail closed with an actionable message —
// this is the same class of authoring mistake trustLabel's own fail-closed
// check guards against, and it must be caught here, not just deep inside
// ClaimLedger.Claim, so the error names the actual bad input.
func TestBacklogQueryRejectsNonPositiveLeaseDuration(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_LEASEDURATION", "0s")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on non-positive leaseDuration), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "leaseDuration") || !strings.Contains(stderr, "must be positive") {
		t.Fatalf("stderr = %q, want an actionable leaseDuration message", stderr)
	}

	// Confirm nothing was claimed — the ledger file must not even exist.
	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("fail-closed rejection should not have touched the claim ledger")
	}
}

// TestBacklogQueryReleaseUnblocksAFollowUpClaim is issue #234's core
// acceptance criterion at the CLI level: immediately after a curation run
// releases its claim, an implementation run can claim the same item — no
// residual lease survives, and no provider/credential access is needed for
// --release (a pure ledger operation, unlike --claim).
func TestBacklogQueryReleaseUnblocksAFollowUpClaim(t *testing.T) {
	root := initDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	// Seed the claim curation's own query-backlog stage would have made.
	if ok, _, err := ledger.Claim("7", "curation-run", "backlog-curation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed curation claim: ok=%v err=%v", ok, err)
	}

	t.Setenv("GOOBERS_RUN_ID", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "released 7") {
		t.Fatalf("stdout = %q, want a mention of the released item", stdout)
	}

	// No residual lease: ForRun finds nothing for the curation run, and an
	// implementation run can now claim the exact item curation just readied.
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.ForRun("curation-run"); held {
		t.Fatal("curation run should hold no claim after release")
	}
	ok, holder, err := reopened.Claim("7", "impl-run", "implementation", DefaultClaimLease)
	if err != nil || !ok || holder != "impl-run" {
		t.Fatalf("implementation run should be able to claim the released item: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

// TestBacklogQueryReleaseIsIdempotent is issue #234's crash-resume
// acceptance criterion: releasing a claim the run does not hold (already
// released, or a crash-resume of the release stage itself) is a no-op
// success, not an error — critical since a checkpoint-trust resume of a
// deterministic stage may retry it after its work already durably landed.
func TestBacklogQueryReleaseIsIdempotent(t *testing.T) {
	root := initDemo(t)

	t.Setenv("GOOBERS_RUN_ID", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Chdir(t.TempDir())

	// No claim ledger exists at all yet — the run holds nothing.
	code, stdout, stderr := runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release with nothing held: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to release") {
		t.Fatalf("stdout = %q, want a clear no-op message", stdout)
	}

	// A second release call (simulating a crash-resume retry) is the same
	// clean no-op, not an error on an already-released claim.
	code, stdout, stderr = runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("second release call: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to release") {
		t.Fatalf("stdout = %q, want the same no-op message on retry", stdout)
	}
}

// TestBacklogQueryClaimAndReleaseAreMutuallyExclusive proves the CLI-level
// usage guard: --claim and --release together is a usage error, not an
// attempt to do both or a silent pick of one.
func TestBacklogQueryClaimAndReleaseAreMutuallyExclusive(t *testing.T) {
	root := initDemo(t)
	t.Chdir(t.TempDir())

	code, _, _ := runArgs(t, "backlog-query", "--claim", "--release", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2 (usage error)", code)
	}
}
