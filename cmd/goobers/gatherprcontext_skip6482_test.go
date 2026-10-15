package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// gatherPRContextSkipFixture is two open PRs on one origin: an older one whose
// diff is unchanged since its recorded escalation (it cannot progress this
// cycle) and a newer one with failing CI that can.
type gatherPRContextSkipFixture struct {
	root    string
	repo    providers.RepositoryRef
	stuck   providers.PullRequestSummary
	healthy providers.PullRequestSummary
	digest  string
	result  string
	parked  []int
	adapter gatherPRContextAdapter
}

func newGatherPRContextSkipFixture(t *testing.T) *gatherPRContextSkipFixture {
	t.Helper()
	const stuckBranch = "goobers/impl/stuck-6482"
	const healthyBranch = "goobers/impl/healthy-6482"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, stuckBranch)
	// The healthy PR's branch is a second ref at the same commit: what makes
	// it progressable is the absence of an escalation record, not its tree.
	runGitT(t, origin, "branch", healthyBranch, stuckBranch)

	checkout := filepath.Join(t.TempDir(), "checkout")
	runGitT(t, filepath.Dir(checkout), "clone", "--branch", "main", origin, checkout)
	digest, err := diffDigestAt(t, checkout, stuckBranch, baseSHA)
	if err != nil {
		t.Fatalf("diffDigest: %v", err)
	}
	escalated, err := remediationStateComment(remediationState{
		Cycles: 2, Escalated: true, LastDiffDigest: digest,
		HeadSHA: headSHA, BaseSHA: baseSHA,
		EscalationOutcome: remediationOutcomeDidNotConverge,
		EscalatedHeadSHA:  headSHA, EscalatedBaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}

	f := &gatherPRContextSkipFixture{
		root: initDemo(t),
		repo: prClaimTestRepo(),
		stuck: providers.PullRequestSummary{
			Number: 10, Head: stuckBranch, Base: "main", HeadSHA: headSHA, BaseSHA: baseSHA,
			Body: escalated, Labels: []string{needsRemediationLabel}, Integrity: apiv1.IntegrityUnapproved,
		},
		healthy: providers.PullRequestSummary{
			Number: 20, Head: healthyBranch, Base: "main", HeadSHA: headSHA, BaseSHA: baseSHA,
			CheckState: providers.CheckStateFailing, Integrity: apiv1.IntegrityUnapproved,
		},
		digest: digest,
		result: filepath.Join(checkout, remediationBriefResultFile),
	}
	f.adapter = gatherPRContextAdapter{
		gitAuth: tokenGitAuthEnvironment("test-token"),
		list: func(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
			return []providers.PullRequestSummary{f.healthy, f.stuck}, nil
		},
		prepare: func(_ context.Context, prs []providers.PullRequestSummary, _ map[string]bool) ([]providers.PullRequestSummary, map[int]int, error) {
			return prs, map[int]int{}, nil
		},
		comments:         func(context.Context, string) ([]providers.Comment, error) { return nil, nil },
		commentOperation: func(n int) string { return "list comments" },
		login:            func(context.Context) (string, error) { return "merge-review-bot", nil },
		park: func(_ context.Context, pr providers.PullRequestSummary, _, _ string) error {
			f.parked = append(f.parked, pr.Number)
			return nil
		},
	}

	t.Setenv("GOOBERS_RUN_ID", "run-6482")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), f.result)
	t.Chdir(checkout)
	return f
}

// diffDigestAt computes branch's diff digest against baseSHA without leaving
// dir on that branch, so the stage under test starts from the runner's own
// default checkout exactly as it does in production.
func diffDigestAt(t *testing.T, dir, branch, baseSHA string) (string, error) {
	t.Helper()
	runGitT(t, dir, "checkout", "-q", branch)
	defer runGitT(t, dir, "checkout", "-q", "main")
	return diffDigest(dir, baseSHA)
}

func (f *gatherPRContextSkipFixture) run(t *testing.T) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := runGatherPRContextCore(f.root, f.repo, f.adapter, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (f *gatherPRContextSkipFixture) brief(t *testing.T) apiv1.RemediationBrief {
	t.Helper()
	data, err := os.ReadFile(f.result)
	if err != nil {
		t.Fatalf("read remediation brief: %v", err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		t.Fatalf("decode remediation brief %s: %v", data, err)
	}
	return brief
}

// TestGatherPRContextSkipsUnprogressableCandidate is #6482's first half: a PR
// at the front of the order whose diff is unchanged since its last recorded
// escalation used to end the whole cycle as no-work, so a parked PR burned
// every scheduled run while the PRs behind it waited. It must be skipped — its
// own unchanged-digest bookkeeping (and, at the limit, its visible park)
// intact — and the cycle must go on to remediate the next candidate.
func TestGatherPRContextSkipsUnprogressableCandidate(t *testing.T) {
	for _, tt := range []struct {
		name       string
		priorRuns  int
		wantParked bool
		wantReason string
	}{
		{name: "first unchanged-digest observation", priorRuns: 0, wantReason: "no progress possible this cycle"},
		{name: "visibly parked at the no-op limit", priorRuns: remediationNoopLimit - 1, wantParked: true, wantReason: "visibly parked"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newGatherPRContextSkipFixture(t)
			key := remediationNoopKey("", f.stuck.Number)
			for i := range tt.priorRuns {
				seedRemediationNoopState(t, layoutFor(f.root), key,
					remediationNoopSignature{HeadSHA: f.stuck.HeadSHA, DiffDigest: f.digest}, fmt.Sprintf("prior-run-%d", i))
			}

			code, stdout, stderr := f.run(t)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if !strings.Contains(stdout, "skipping PR #10") || !strings.Contains(stdout, tt.wantReason) {
				t.Fatalf("stdout = %q, want PR #10 skipped with %q", stdout, tt.wantReason)
			}
			if got := f.brief(t).SelectedNumber; got != "20" {
				t.Fatalf("selected PR = %s, want #20 — the cycle must continue past the unprogressable PR #10", got)
			}
			if head := strings.TrimSpace(runGitOutputT(t, ".", "rev-parse", "--abbrev-ref", "HEAD")); head != f.healthy.Head {
				t.Fatalf("worktree branch = %q, want the selected PR's %q", head, f.healthy.Head)
			}
			if claimed, ok, err := claimedPullRequestNumber(f.root); err != nil || !ok || claimed != 20 {
				t.Fatalf("run's claim = #%d (ok=%v, err=%v), want only PR #20 — the skipped PR's claim must be surrendered", claimed, ok, err)
			}
			if wantParked := tt.wantParked; wantParked != (len(f.parked) == 1 && f.parked[0] == 10) {
				t.Fatalf("parked = %v, want parked=%v for PR #10 only", f.parked, wantParked)
			}
			record := remediationNoopStateRecord(t, layoutFor(f.root), key)
			if record.Attempts != tt.priorRuns+1 || record.Parked != tt.wantParked {
				t.Fatalf("PR #10 no-op record = %+v, want attempts %d parked %v — skipping must not lose the guard's bookkeeping", record, tt.priorRuns+1, tt.wantParked)
			}
		})
	}
}

// TestGatherPRContextReportsTheSkipWhenNothingElseIsClaimable pins the no-work
// account once the only claimable candidate was skipped: the cycle names the
// skip, not the generic idle-tick reason, and does not take a PR another run
// holds.
func TestGatherPRContextReportsTheSkipWhenNothingElseIsClaimable(t *testing.T) {
	f := newGatherPRContextSkipFixture(t)
	if other, err := claimPullRequestInOrder(f.root, f.repo, []providers.PullRequestSummary{f.healthy}, "other-run", "pr-remediation", time.Hour); err != nil || other == nil {
		t.Fatalf("pre-claim PR #20 for another run: %+v, %v", other, err)
	}

	code, stdout, stderr := f.run(t)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, f.result)
	result := readProviderStageResult(t, f.result)
	reason, _ := result["noWorkReason"].(string)
	if !strings.Contains(reason, "PR #10's diff") || strings.Contains(reason, "no PR needs remediation") {
		t.Fatalf("noWorkReason = %q, want PR #10's skip reason", reason)
	}
	if claimed, ok, err := claimedPullRequestNumber(f.root); err != nil || !ok || claimed != 10 {
		t.Fatalf("run's claim = #%d (ok=%v, err=%v), want PR #10 only — PR #20 belongs to another run", claimed, ok, err)
	}
}

func TestGatherPRContextExhaustedReason(t *testing.T) {
	if got := gatherPRContextExhaustedReason(nil, true); got != "every eligible PR is already claimed by another run" {
		t.Fatalf("no skips: reason = %q", got)
	}
	skips := []string{"PR #10 was visibly parked", "PR #11's diff is unchanged"}
	if got := gatherPRContextExhaustedReason(skips, false); got != "PR #10 was visibly parked; PR #11's diff is unchanged" {
		t.Fatalf("skips, nothing left: reason = %q", got)
	}
	got := gatherPRContextExhaustedReason(skips, true)
	if got != "PR #10 was visibly parked; PR #11's diff is unchanged; every other eligible PR is already claimed by another run" {
		t.Fatalf("skips, rest held elsewhere: reason = %q", got)
	}
}

// TestGatherPRContextFallsBackToBehindBaseWhenEveryStrongCandidateIsSkipped
// closes the remaining shape of #6482's first half: a strong candidate that
// cannot progress must not suppress the behind-base fallback for the cycle
// either. With the only strong candidate skipped, the crowned lander behind
// its base is remediated exactly as it would have been had the stuck PR not
// carried a signal at all.
func TestGatherPRContextFallsBackToBehindBaseWhenEveryStrongCandidateIsSkipped(t *testing.T) {
	f := newGatherPRContextSkipFixture(t)
	f.healthy.CheckState = providers.CheckStatePassing
	f.adapter.features.siblingBlocking = true
	f.adapter.prepare = func(_ context.Context, prs []providers.PullRequestSummary, _ map[string]bool) ([]providers.PullRequestSummary, map[int]int, error) {
		return prs, map[int]int{f.healthy.Number: 1}, nil
	}
	var probed []int
	f.adapter.behindBase = func(pr providers.PullRequestSummary) (bool, error) {
		probed = append(probed, pr.Number)
		return pr.Number == f.healthy.Number, nil
	}

	code, stdout, stderr := f.run(t)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "skipping PR #10") {
		t.Fatalf("stdout = %q, want the stuck strong candidate skipped", stdout)
	}
	if got := f.brief(t).SelectedNumber; got != "20" {
		t.Fatalf("selected PR = %s, want the crowned behind-base lander #20", got)
	}
	if fmt.Sprint(probed) != "[20]" {
		t.Fatalf("behind-base probes = %v, want only the non-strong PR probed, and only after the skip", probed)
	}
	if claimed, ok, err := claimedPullRequestNumber(f.root); err != nil || !ok || claimed != 20 {
		t.Fatalf("run's claim = #%d (ok=%v, err=%v), want PR #20 only", claimed, ok, err)
	}
}

// TestStrongRemediationCandidatesShareOneFIFOOrder is #6482's second half: the
// two strong signals share one oldest-first order, so a failing-CI PR waits
// only behind strong candidates older than itself.
func TestStrongRemediationCandidatesShareOneFIFOOrder(t *testing.T) {
	candidates, best := strongRemediationCandidates([]providers.PullRequestSummary{
		{Number: 40, Labels: []string{needsRemediationLabel}},
		{Number: 15, CheckState: providers.CheckStateFailing},
		{Number: 30, Labels: []string{needsRemediationLabel}},
		{Number: 5},
		{Number: 10, Labels: []string{needsRemediationLabel}},
	})
	if best != remediationPriorityNeedsRemediation {
		t.Fatalf("best = %d, want needs-remediation", best)
	}
	if got := fmt.Sprint(prNumbers(candidates)); got != "[10 15 30 40]" {
		t.Fatalf("candidates = %v, want oldest first across both signals", got)
	}
}

// TestSteadyNeedsRemediationSupplyDoesNotStarveFailingCI simulates the soak's
// starvation shape cycle by cycle: one remediation per cycle, a new
// needs-remediation PR arriving every cycle, and a PR whose build a human broke
// after it opened. Under the old strict tiers the failing-CI PR is never
// selected; under the shared FIFO order it is selected as soon as the
// needs-remediation PRs OLDER than it have had their turn.
func TestSteadyNeedsRemediationSupplyDoesNotStarveFailingCI(t *testing.T) {
	open := []providers.PullRequestSummary{
		{Number: 90, Labels: []string{needsRemediationLabel}},
		{Number: 95, Labels: []string{needsRemediationLabel}},
		{Number: 100, CheckState: providers.CheckStateFailing},
	}
	next := 101
	const cycles = 20
	for cycle := 1; cycle <= cycles; cycle++ {
		open = append(open, providers.PullRequestSummary{Number: next, Labels: []string{needsRemediationLabel}})
		next++
		candidates, _, err := selectRemediationCandidates(open, nil, func(providers.PullRequestSummary) (bool, error) {
			t.Fatal("behind-base probe must not run while strong candidates exist")
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		selected := candidates[0].Number
		if selected == 100 {
			// Exactly the two older needs-remediation PRs went first.
			if cycle != 3 {
				t.Fatalf("failing-CI PR #100 selected on cycle %d, want cycle 3", cycle)
			}
			return
		}
		open = removePR(open, selected)
	}
	t.Fatalf("failing-CI PR #100 not selected in %d cycles of steady needs-remediation supply", cycles)
}

func removePR(prs []providers.PullRequestSummary, number int) []providers.PullRequestSummary {
	out := prs[:0:0]
	for _, pr := range prs {
		if pr.Number != number {
			out = append(out, pr)
		}
	}
	return out
}
