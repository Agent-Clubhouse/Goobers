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

func TestBacklogQueryLabelLists(t *testing.T) {
	tests := []struct {
		name          string
		requireLabels string
		excludeLabels string
		issueLabels   [][]string
		wantIDs       string
	}{
		{
			name:          "require single",
			requireLabels: "a",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}},
			wantIDs:       "7",
		},
		{
			name:          "require multiple",
			requireLabels: "a,b",
			issueLabels:   [][]string{{"trusted", "a", "b"}, {"trusted", "a"}},
			wantIDs:       "7",
		},
		{
			name:          "require spaced",
			requireLabels: "a, b",
			issueLabels:   [][]string{{"trusted", "a", "b"}, {"trusted", "a"}},
			wantIDs:       "7",
		},
		{
			name:        "require empty",
			issueLabels: [][]string{{"trusted"}},
			wantIDs:     "7",
		},
		{
			name:          "exclude single",
			excludeLabels: "a",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}},
			wantIDs:       "8",
		},
		{
			name:          "exclude multiple",
			excludeLabels: "a,b",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}, {"trusted", "c"}},
			wantIDs:       "9",
		},
		{
			name:          "exclude spaced",
			excludeLabels: "a, b",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}, {"trusted", "c"}},
			wantIDs:       "9",
		},
		{
			name:        "exclude empty",
			issueLabels: [][]string{{"trusted"}},
			wantIDs:     "7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			for i, labels := range tt.issueLabels {
				server.addIssue(7+i, "Candidate", labels...)
			}

			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
			t.Setenv("GOOBERS_INPUT_REQUIRELABELS", tt.requireLabels)
			t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", tt.excludeLabels)
			t.Chdir(t.TempDir())

			code, stdout, stderr := runArgs(t, "backlog-query", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			var gotIDs []string
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				if id, _, ok := strings.Cut(line, "\t"); ok {
					gotIDs = append(gotIDs, id)
				}
			}
			if got := strings.Join(gotIDs, ","); got != tt.wantIDs {
				t.Fatalf("eligible IDs = %q, want %q; stdout = %q", got, tt.wantIDs, stdout)
			}
		})
	}
}

func TestBacklogQueryCurationExcludesReadyItem(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Already curated", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers:ready,goobers:needs-human")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want ready-labeled item skipped as no work", stdout)
	}
	assertNoWorkResultFile(t, workDir)
	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("curation should not claim an already-ready item")
	}
}

// TestBacklogQueryUnlabeledItemNeverClaimed proves SEC-047 eligibility is
// enforced in code, not just documented: an item missing the trust label is
// never claimed even though it's otherwise ready. Also issue #233's core
// acceptance: an empty eligible set is a clean no-work exit 0, not a
// business-error exit 1 — every idle tick must not poison telemetry as a
// false run failure.
func TestBacklogQueryUnlabeledItemNeverClaimed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(9, "Untrusted item", "goobers:ready") // no trust label

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (empty backlog is no-work, not a failure), stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want a clear no-work message", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// assertNoWorkResultFile confirms the default resultFile carries the
// structured OutputNoWork signal internal/executor/shell.go's ResultNoWork
// mapping reads (issue #233) — not just a human-facing stdout line.
func assertNoWorkResultFile(t *testing.T, workDir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if got["noWork"] != true {
		t.Fatalf("claimed-item.json = %v, want noWork:true", got)
	}
	if got["claimed"] != false {
		t.Fatalf("claimed-item.json = %v, want claimed:false", got)
	}
}

// TestBacklogQuerySecondRunLosesTheClaimRace is #131's "two concurrent runs,
// one claim wins" acceptance criterion, driven sequentially through the CLI
// (env vars are process-global, so genuinely concurrent goroutines can't
// each carry a distinct GOOBERS_RUN_ID within one test binary) — the
// exclusivity property under test is the claim ledger's own atomicity
// (already proven under real concurrency at the ledger level by
// internal/localscheduler's TestClaimConcurrentRace), not raw goroutine
// timing. Also issue #233: the loser's outcome is now a clean no-work exit
// 0, not a business-error exit 1 — losing a claim race is exactly as
// routine as an empty backlog, not a failure.
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
	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("second claim: code = %d, want 0 (losing the race is no-work, not a failure), stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want a clear no-work message", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// TestBacklogQueryExcludesIssueWithOpenPR is #414's core acceptance: an item
// that's otherwise eligible by label (goobers:approved + goobers:ready, no
// exclude label present — simulating a missed or since-removed in-review/
// claimed label write) must still be excluded once an open goober-authored
// PR references it via a closing keyword, the same convention `goobers
// open-pr` writes and `goobers post-merge` parses at merge time. Requires
// the query-backlog stage to actually declare github:pr:write — the
// backstop is opt-in per stage (see the next test for the ungranted case).
func TestBacklogQueryExcludesIssueWithOpenPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "sha1", "sha2", false, nil, nil)
	server.setPRBody(101, "Implements the fix.\n\nFixes #7")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (no eligible item is no-work, not a failure), stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want a clear no-work message — issue 7 should be excluded by the open-PR backstop", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// TestBacklogQueryExcludesIssueWithImplementsOnlyPR is #980: the open-PR
// backstop must exclude an issue whose only open PR references it via the
// non-closing "Implements #N" convention (a structured body whose "Fixes #N"
// footer was overridden or absent), not just one carrying a closing keyword.
// This is the gap that let issue #774 be implemented twice (#966/#969).
func TestBacklogQueryExcludesIssueWithImplementsOnlyPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Convert the logs", "goobers:approved", "goobers:ready")
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "sha1", "sha2", false, nil, nil)
	server.setPRBody(101, "## Summary\n\nImplements #7: **Convert the logs**.")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work — issue 7 should be excluded by the widened open-PR backstop", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// TestBacklogQueryOpenPRBackstopSkippedWithoutCapability proves the backstop
// is opt-in, not a hard requirement: a stage that never declared
// github:pr:write gets exactly the pre-#414 label-only behavior (the item is
// still eligible) rather than backlog-query failing closed on a capability
// it was never granted — the label check above remains the primary
// eligibility gate; this backstop only adds to it when available.
func TestBacklogQueryOpenPRBackstopSkippedWithoutCapability(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "sha1", "sha2", false, nil, nil)
	server.setPRBody(101, "Fixes #7")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	// Deliberately no GOOBERS_CRED_GITHUB_PR_WRITE.
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("stdout = %q, want item 7 claimed (backstop not active without the capability)", stdout)
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
	// #321: a live local-ci `go test ./...` inherits the run's real
	// GOOBERS_RUN_ID/GOOBERS_WORKFLOW from internal/executor.buildStageEnv, which
	// silently defeated this fail-closed test on every run. Simulate that
	// parent-process leak, then clear it — so the test genuinely exercises the
	// missing-run-context path AND regression-guards the fix under normal CI
	// (which has no ambient run context of its own to reproduce the leak).
	t.Setenv("GOOBERS_RUN_ID", "ambient-parent-leak")
	t.Setenv("GOOBERS_WORKFLOW", "ambient-parent-leak")
	unsetRunContext(t)
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

// TestBacklogQueryReleaseUnblocksAFollowUpClaim covers #234 and #1003 together:
// a real curation claim adds both the authoritative ledger lease and its
// provider mirror; release removes both while preserving curation's ready label,
// and an implementation run can immediately claim the item.
func TestBacklogQueryReleaseUnblocksAFollowUpClaim(t *testing.T) {
	root := initDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers:ready,goobers:needs-human")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("claim: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	server.mu.Lock()
	server.issues[7].labels = append(server.issues[7].labels, "goobers:ready")
	claimedLabels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if !hasAnyLabel(claimedLabels, []string{"goobers:claimed"}) {
		t.Fatalf("labels after claim = %v, want goobers:claimed", claimedLabels)
	}

	code, stdout, stderr = runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "released 7") {
		t.Fatalf("stdout = %q, want a mention of the released item", stdout)
	}
	server.mu.Lock()
	releasedLabels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if hasAnyLabel(releasedLabels, []string{"goobers:claimed"}) {
		t.Fatalf("labels after release = %v, want goobers:claimed removed", releasedLabels)
	}
	if !hasAnyLabel(releasedLabels, []string{"goobers:ready"}) {
		t.Fatalf("labels after release = %v, want curator's goobers:ready preserved", releasedLabels)
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

func TestBacklogQueryReleaseReleasesAllClaims(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for _, itemID := range []int{7, 8, 9, 10} {
		server.addIssue(itemID, "Claimed item", "goobers:claimed")
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")

	ledgerPath := filepath.Join(root, "scheduler", claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []string{"7", "8", "9"} {
		if ok, _, err := ledger.Claim(itemID, "curation-run", "backlog-curation", DefaultClaimLease); err != nil || !ok {
			t.Fatalf("seed curation claim %s: ok=%v err=%v", itemID, ok, err)
		}
	}
	if ok, _, err := ledger.Claim("10", "other-run", "implementation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed other run claim: ok=%v err=%v", ok, err)
	}

	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if stdout != "released 7, 8, 9\n" {
		t.Fatalf("stdout = %q, want every released item", stdout)
	}

	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read claim ledger: %v", err)
	}
	var entries map[string]localscheduler.ClaimEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("unmarshal claim ledger: %v", err)
	}
	for itemID, entry := range entries {
		if entry.RunID == "curation-run" {
			t.Fatalf("claim %s leaked for curation-run: %+v", itemID, entry)
		}
	}
	if len(entries) != 1 || entries["10"].RunID != "other-run" {
		t.Fatalf("claim ledger = %+v, want only item 10 held by other-run", entries)
	}
	server.mu.Lock()
	for _, itemID := range []int{7, 8, 9} {
		if hasAnyLabel(server.issues[itemID].labels, []string{"goobers:claimed"}) {
			t.Errorf("issue %d labels = %v, want goobers:claimed removed", itemID, server.issues[itemID].labels)
		}
	}
	otherLabels := append([]string(nil), server.issues[10].labels...)
	server.mu.Unlock()
	if !hasAnyLabel(otherLabels, []string{"goobers:claimed"}) {
		t.Fatalf("other run's issue labels = %v, want goobers:claimed preserved", otherLabels)
	}
}

func TestBacklogQueryReleaseRetainsClaimWhenProviderCleanupFails(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")

	ledgerPath := filepath.Join(root, "scheduler", claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("7", "curation-run", "backlog-curation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed curation claim: ok=%v err=%v", ok, err)
	}
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--release", root)
	if code != 1 || !strings.Contains(stderr, "remove provider claim marker for 7") {
		t.Fatalf("release: code = %d, stderr = %q, want provider cleanup failure", code, stderr)
	}
	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	entry, held := reopened.Lookup("7")
	if !held || entry.RunID != "curation-run" {
		t.Fatalf("claim after failed cleanup = %+v, held=%v; want retained for retry", entry, held)
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
