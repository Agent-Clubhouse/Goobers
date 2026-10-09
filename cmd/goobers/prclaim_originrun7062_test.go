package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestFilterClaimAvailablePullRequestsHonorsLiveOriginRun covers #7062: the
// implementation run that opened a PR keeps pushing to its run-scoped branch
// holding only an issue claim, so PR-consuming lanes must treat the PR as
// claimed while that run's claim is live.
func TestFilterClaimAvailablePullRequestsHonorsLiveOriginRun(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	schedulerDir := layoutFor(root).SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	const originRun = "0123456789abcdef0123456789abcdef"
	issueKey := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "42"}
	if ok, _, err := raw.ClaimScoped(issueKey, originRun, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed issue claim: ok=%t err=%v", ok, err)
	}
	ledger, err := fileClaimLedger(layoutFor(root))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	originPR := providers.PullRequestSummary{Number: 7, Head: providers.BranchNameIn("goobernetes", "implementation", originRun)}
	otherPR := providers.PullRequestSummary{Number: 8, Head: "feature/some-branch"}
	// A run-shaped branch whose run holds no claim at all.
	staleRunPR := providers.PullRequestSummary{Number: 9, Head: providers.BranchNameIn("", "implementation", "fedcba9876543210fedcba9876543210")}
	all := []providers.PullRequestSummary{originPR, otherPR, staleRunPR}

	numbers := func(prs []providers.PullRequestSummary) []int {
		var out []int
		for _, pr := range prs {
			out = append(out, pr.Number)
		}
		return out
	}
	filter := func(run string, at time.Time) []int {
		t.Helper()
		got, err := filterClaimAvailablePullRequests(ledger, "goobers", providers.ProviderGitHub, run, all, at)
		if err != nil {
			t.Fatal(err)
		}
		return numbers(got)
	}
	equal := func(got []int, want ...int) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// (a) live origin claim hides the PR; (d) non-run branches are unaffected.
	if got := filter("remediation-run", now); !equal(got, 8, 9) {
		t.Fatalf("live origin: available = %v, want [8 9]", got)
	}
	// (c) the origin run still sees its own PR.
	if got := filter(originRun, now); !equal(got, 7, 8, 9) {
		t.Fatalf("origin run: available = %v, want [7 8 9]", got)
	}
	// (b) expired claim frees the PR.
	if got := filter("remediation-run", now.Add(2*time.Hour)); !equal(got, 7, 8, 9) {
		t.Fatalf("expired origin: available = %v, want [7 8 9]", got)
	}
	// (b) released claim frees the PR.
	if err := raw.ReleaseScoped(issueKey, originRun); err != nil {
		t.Fatal(err)
	}
	if got := filter("remediation-run", now); !equal(got, 7, 8, 9) {
		t.Fatalf("released origin: available = %v, want [7 8 9]", got)
	}
}

func TestRunBranchOriginRunID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for head, want := range map[string]string{
		"goobernetes/implementation/" + id: id,
		"goobers/implementation/" + id:     id,
		"team/ns/implementation/" + id:     id,
		"implementation/" + id:             "",
		"feature/x":                        "",
		"goobers/implementation/short":     "",
		"goobers/implementation/" + "ZZZZ456789abcdef0123456789abcdef": "",
		"": "",
	} {
		if got := runBranchOriginRunID(head); got != want {
			t.Errorf("runBranchOriginRunID(%q) = %q, want %q", head, got, want)
		}
	}
}
