package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestBacklogQueryDebugExplainsNativeDependencyExclusion(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(664, "Prerequisite")
	server.addIssue(1579, "Apparently ready", "goobers:approved")
	server.setIssueBlockers(1579, 664)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "debug-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--debug", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query --debug: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"debug: candidate 1579 reached eligibility evaluation",
		"debug: excluded 1579: native issue dependencies include open blocker(s): 664",
		"debug: eligible set empty",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "Apparently ready") || strings.Contains(stderr, "Prerequisite") {
		t.Fatalf("stderr exposed issue title: %q", stderr)
	}
}

func TestBacklogQueryDebugDistinguishesLedgerClaimLoss(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Contested candidate", "goobers:approved")

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("7", "other-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok = %v, err = %v", ok, err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "debug-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--debug", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query --debug: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"debug: candidate 7 reached eligibility evaluation",
		"debug: eligible 7",
		"debug: claim lost 7: ledger claim held by another run",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "eligible set empty") {
		t.Fatalf("stderr = %q, claim contention must not be reported as an empty eligible set", stderr)
	}
}

func TestBacklogQueryDebugReportsReadyResweepBeforeFinalEligibility(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Ready re-sweep item", "goobers:approved", providers.LabelReady)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "debug-resweep-run")
	configureCurationResweep(t, "1", "1", "24h")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--debug", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query --debug: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"debug: candidate 7 reached eligibility evaluation (ready re-sweep)",
		"debug: eligible 7",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "eligible set empty") {
		t.Fatalf("stderr = %q, re-sweep candidate must be included in final eligibility", stderr)
	}
}

func TestBacklogQueryDebugExplainsBlockedResweepExclusion(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Blocked re-sweep item", "goobers:approved", blockedOnSiblingLabel)
	server.addIssue(8, "Open blocker")
	server.setIssueBlockers(7, 8)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "debug-blocked-resweep-run")
	configureCurationResweep(t, "1", "1", "24h")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--debug", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query --debug: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"debug: candidate 7 reached eligibility evaluation (blocked re-sweep)",
		"debug: excluded 7: dependency recheck still has actionable open blocker(s): 8",
		"debug: eligible set empty",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
}

func TestBacklogQueryWithoutDebugKeepsDiagnosticsSilent(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Visible item", "goobers:approved")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "normal-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 || stdout != "7\tVisible item\n" || stderr != "" {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestLabelExclusionReasonIsProviderIndependent(t *testing.T) {
	opts := backlogScanOptions{
		requireLabels: []string{"goobers:ready"},
		excludeLabels: []string{"goobers:paused"},
	}
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		item := providers.WorkItem{Provider: provider, ID: "42", Labels: []string{"goobers:paused"}}
		if got := labelExclusionReason(item, opts); got != `missing required label "goobers:ready"` {
			t.Fatalf("%s reason = %q", provider, got)
		}
	}
}
