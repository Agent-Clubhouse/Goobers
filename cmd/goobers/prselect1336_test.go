package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestPRSelectBelowAgingThresholdPreservesBlockerFIFOOrder(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	eligible := []providers.PullRequestSummary{
		{Number: 103},
		{Number: 100},
		{Number: 102},
		{Number: 101},
	}
	since := map[int]time.Time{}
	for _, pr := range eligible {
		since[pr.Number] = now.Add(-prSelectAgingInterval + time.Second)
	}

	ranked, _, _ := rankEligiblePullRequests(eligible, map[int]int{
		103: 2,
		102: 1,
	}, since, now)

	if got, want := prNumbers(ranked), []int{103, 102, 100, 101}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked PRs = %v, want existing blocker/FIFO order %v", got, want)
	}
}

func TestPRSelectAgingSelectsPRAfterRepeatedFIFOLosses(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	target := providers.PullRequestSummary{Number: 100, HeadSHA: "target-head"}
	cycling := providers.PullRequestSummary{Number: 10, HeadSHA: "cycling-head"}
	start := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	for cycle := 0; cycle <= 3; cycle++ {
		now := start.Add(time.Duration(cycle) * 5 * time.Minute)
		eligible := []providers.PullRequestSummary{target, cycling}
		observation, err := observePRSelectEligibility(
			root, repo, eligible, eligible, prSelectCompleteSnapshot, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		ranked, priorities, _ := rankEligiblePullRequests(
			observation.UnclaimedEligible, nil, observation.EligibleSince, now,
		)
		if cycle < 3 {
			if ranked[0].Number != cycling.Number {
				t.Fatalf("cycle %d selected PR #%d, want lower-numbered cycling PR #%d", cycle, ranked[0].Number, cycling.Number)
			}
			if err := clearPRSelectEligibilityWait(root, repo, cycling); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if ranked[0].Number != target.Number {
			t.Fatalf("cycle %d selected PR #%d, want aged PR #%d", cycle, ranked[0].Number, target.Number)
		}
		if got := priorities[target.Number].Wait; got != prSelectAgingInterval {
			t.Fatalf("aged PR wait = %s, want %s", got, prSelectAgingInterval)
		}
	}
}

func TestPRSelectEquivalentEffectivePriorityUsesFIFO(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	eligible := []providers.PullRequestSummary{{Number: 20}, {Number: 10}}
	since := map[int]time.Time{
		20: now.Add(-2 * prSelectAgingInterval),
		10: now.Add(-prSelectAgingInterval),
	}

	ranked, priorities, _ := rankEligiblePullRequests(eligible, map[int]int{10: 1}, since, now)
	if priorities[10].EffectivePriority != priorities[20].EffectivePriority {
		t.Fatalf("effective priorities = %#v, want an equivalent-priority fixture", priorities)
	}
	if ranked[0].Number != 10 {
		t.Fatalf("selected PR #%d, want lower-numbered PR #10 as the effective-priority tie-breaker", ranked[0].Number)
	}
}

func TestPRSelectStarvationGuardOverridesBlockerPriorityAtOneHour(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	aged := providers.PullRequestSummary{Number: 900}
	blockerHeavy := providers.PullRequestSummary{Number: 1}
	eligible := []providers.PullRequestSummary{blockerHeavy, aged}
	blockedDependents := map[int]int{blockerHeavy.Number: 100}

	t.Run("below guard", func(t *testing.T) {
		since := map[int]time.Time{
			aged.Number:         now.Add(-prSelectStarvationLimit + time.Second),
			blockerHeavy.Number: now,
		}
		ranked, _, _ := rankEligiblePullRequests(eligible, blockedDependents, since, now)
		if ranked[0].Number != blockerHeavy.Number {
			t.Fatalf("selected PR #%d, want blocker-heavy PR #%d below guard", ranked[0].Number, blockerHeavy.Number)
		}
	})

	t.Run("at guard", func(t *testing.T) {
		since := map[int]time.Time{
			aged.Number:         now.Add(-prSelectStarvationLimit),
			blockerHeavy.Number: now,
		}
		ranked, priorities, metrics := rankEligiblePullRequests(eligible, blockedDependents, since, now)
		if ranked[0].Number != aged.Number {
			t.Fatalf("selected PR #%d, want guarded PR #%d", ranked[0].Number, aged.Number)
		}
		if !priorities[aged.Number].StarvationGuarded {
			t.Fatal("aged PR did not enter the starvation guard")
		}
		if len(metrics.Starved) != 0 {
			t.Fatalf("starved PRs = %v at exactly one hour, want none over the bound", metrics.Starved)
		}
	})

	t.Run("oldest guarded candidate wins", func(t *testing.T) {
		since := map[int]time.Time{
			aged.Number:         now.Add(-2 * prSelectStarvationLimit),
			blockerHeavy.Number: now.Add(-prSelectStarvationLimit),
		}
		ranked, priorities, _ := rankEligiblePullRequests(eligible, blockedDependents, since, now)
		if !priorities[aged.Number].StarvationGuarded || !priorities[blockerHeavy.Number].StarvationGuarded {
			t.Fatalf("priorities = %#v, want both candidates guarded", priorities)
		}
		if ranked[0].Number != aged.Number {
			t.Fatalf("selected PR #%d, want oldest guarded PR #%d", ranked[0].Number, aged.Number)
		}
	})
}

func TestPRSelectIneligiblePRDoesNotAccumulateAge(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	pr := providers.PullRequestSummary{Number: 77, HeadSHA: "head-one"}
	start := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	if _, err := observePRSelectEligibility(
		root, repo, []providers.PullRequestSummary{pr}, []providers.PullRequestSummary{pr}, prSelectCompleteSnapshot, start,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := observePRSelectEligibility(root, repo, nil, nil, prSelectCompleteSnapshot, start.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	eligibleAgain := start.Add(2 * time.Hour)
	observation, err := observePRSelectEligibility(
		root, repo, []providers.PullRequestSummary{pr}, []providers.PullRequestSummary{pr}, prSelectCompleteSnapshot, eligibleAgain,
	)
	if err != nil {
		t.Fatal(err)
	}
	since := observation.EligibleSince
	if got := since[pr.Number]; !got.Equal(eligibleAgain) {
		t.Fatalf("eligibleSince after an ineligible observation = %s, want reset to %s", got, eligibleAgain)
	}

	pr.HeadSHA = "head-two"
	headChanged := eligibleAgain.Add(5 * time.Minute)
	observation, err = observePRSelectEligibility(
		root, repo, []providers.PullRequestSummary{pr}, []providers.PullRequestSummary{pr}, prSelectCompleteSnapshot, headChanged,
	)
	if err != nil {
		t.Fatal(err)
	}
	since = observation.EligibleSince
	if got := since[pr.Number]; !got.Equal(headChanged) {
		t.Fatalf("eligibleSince after head change = %s, want reset to %s", got, headChanged)
	}
}

func TestPRSelectActiveClaimDoesNotAccumulateWaitAcrossTicks(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	active := providers.PullRequestSummary{Number: 10, HeadSHA: "active-head"}
	waiting := providers.PullRequestSummary{Number: 20, HeadSHA: "waiting-head"}
	eligible := []providers.PullRequestSummary{active, waiting}
	start := time.Now().UTC()

	first, err := observePRSelectEligibility(
		root, repo, eligible, eligible, prSelectCompleteSnapshot, start,
	)
	if err != nil {
		t.Fatal(err)
	}
	ranked, _, _ := rankEligiblePullRequests(first.UnclaimedEligible, nil, first.EligibleSince, start)
	selected, err := claimPullRequestInOrder(root, ranked, "first-run", "merge-review", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.Number != active.Number {
		t.Fatalf("first selected PR = %+v, want PR #%d", selected, active.Number)
	}
	if err := clearPRSelectEligibilityWait(root, repo, *selected); err != nil {
		t.Fatal(err)
	}

	nextTick := start.Add(prSelectStarvationLimit + time.Minute)
	second, err := observePRSelectEligibility(
		root, repo, eligible, eligible, prSelectCompleteSnapshot, nextTick,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prNumbers(second.UnclaimedEligible), []int{waiting.Number}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unclaimed eligible PRs = %v, want %v", got, want)
	}
	if _, ok := second.EligibleSince[active.Number]; ok {
		t.Fatalf("active claim PR #%d has an eligible wait timestamp", active.Number)
	}
	_, priorities, metrics := rankEligiblePullRequests(
		second.UnclaimedEligible, nil, second.EligibleSince, nextTick,
	)
	if _, ok := priorities[active.Number]; ok {
		t.Fatalf("active claim PR #%d has a fairness priority", active.Number)
	}
	if got, want := metrics.Starved, []int{waiting.Number}; !reflect.DeepEqual(got, want) {
		t.Fatalf("starved PRs = %v, want only unclaimed PRs %v", got, want)
	}

	state, err := readPRSelectFairnessFile(filepath.Join(layoutFor(root).SchedulerDir(), prSelectFairnessFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Candidates) != 1 || state.Candidates[0].Number != waiting.Number {
		t.Fatalf("fairness candidates = %+v, want only unclaimed PR #%d", state.Candidates, waiting.Number)
	}
}

func TestPRSelectMultiCandidateRetryReclaimsOnlyItsOwnPR(t *testing.T) {
	const prNumber = 703
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "retry claim")
	server.addOpenPR(prNumber, "goobers/implementation/retry", "main", "retry-head", "base", false, nil, nil)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-retry")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #703") {
		t.Fatalf("initial attempt: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.addIssue(702, "lower-numbered retry candidate")
	server.addOpenPR(702, "goobers/implementation/other", "main", "other-head", "base", false, nil, nil)
	t.Chdir(t.TempDir())
	code, stdout, stderr = runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #703") {
		t.Fatalf("retry: code = %d, stdout = %q, stderr = %q; want existing claim only", code, stdout, stderr)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if claims := ledger.ForRunAll("merge-run-retry"); len(claims) != 1 || claims[0].ItemID != pullRequestClaimKey(prNumber) {
		t.Fatalf("retry claims = %+v, want only PR #%d", claims, prNumber)
	}
}

func TestPRSelectClaimedIntervalDoesNotResumeAfterReleaseOrExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release bool
	}{
		{name: "release", release: true},
		{name: "expiry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				prNumber = 704
				runID    = "claim-owner"
			)
			root := initDemo(t)
			t.Setenv("GOOBERS_GAGGLE", "goobers")
			t.Setenv("GOOBERS_RUN_ID", runID)
			repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
			pr := providers.PullRequestSummary{Number: prNumber, HeadSHA: "claimed-head"}
			seededAt := time.Now().UTC().Add(-2 * time.Hour)

			if _, err := observePRSelectEligibility(
				root,
				repo,
				[]providers.PullRequestSummary{pr},
				[]providers.PullRequestSummary{pr},
				prSelectCompleteSnapshot,
				seededAt,
			); err != nil {
				t.Fatal(err)
			}
			selected, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{pr}, runID, "merge-review", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if selected == nil {
				t.Fatal("claim returned no selected PR")
			}
			if err := clearPRSelectEligibilityWait(root, repo, *selected); err != nil {
				t.Fatal(err)
			}

			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
			if err != nil {
				t.Fatal(err)
			}
			key := localscheduler.ClaimKey{
				Gaggle:     "goobers",
				Provider:   string(providers.ProviderGitHub),
				ExternalID: pullRequestClaimKey(prNumber),
			}
			entry, ok := ledger.LookupScoped(key)
			if !ok {
				t.Fatal("claimed PR is missing from the ledger")
			}
			unclaimedAt := entry.ExpiresAt.Add(time.Minute)
			if tc.release {
				if err := ledger.ReleaseScoped(key, runID); err != nil {
					t.Fatal(err)
				}
				unclaimedAt = time.Now().UTC().Add(time.Minute)
			}

			observation, err := observePRSelectEligibility(
				root,
				repo,
				[]providers.PullRequestSummary{pr},
				[]providers.PullRequestSummary{pr},
				prSelectCompleteSnapshot,
				unclaimedAt,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got := observation.EligibleSince[prNumber]; !got.Equal(unclaimedAt) {
				t.Fatalf("eligibleSince after claim %s = %s, want new interval %s", tc.name, got, unclaimedAt)
			}
			_, priorities, _ := rankEligiblePullRequests(
				observation.UnclaimedEligible, nil, observation.EligibleSince, unclaimedAt,
			)
			if got := priorities[prNumber].Wait; got != 0 {
				t.Fatalf("eligible wait after claim %s = %s, want 0", tc.name, got)
			}
		})
	}
}

func TestPRSelectPartialObservationPreservesUnobservedEligibility(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	targeted := providers.PullRequestSummary{Number: 10, HeadSHA: "targeted-head"}
	unobserved := providers.PullRequestSummary{Number: 20, HeadSHA: "unobserved-head"}
	all := []providers.PullRequestSummary{targeted, unobserved}
	start := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	if _, err := observePRSelectEligibility(
		root, repo, all, all, prSelectCompleteSnapshot, start,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := observePRSelectEligibility(
		root,
		repo,
		[]providers.PullRequestSummary{targeted},
		[]providers.PullRequestSummary{targeted},
		prSelectPartialSnapshot,
		start.Add(30*time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	complete, err := observePRSelectEligibility(
		root, repo, all, all, prSelectCompleteSnapshot, start.Add(45*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := complete.EligibleSince[unobserved.Number]; !got.Equal(start) {
		t.Fatalf("unobserved PR eligibleSince = %s, want preserved timestamp %s", got, start)
	}

	if _, err := observePRSelectEligibility(
		root,
		repo,
		[]providers.PullRequestSummary{targeted},
		nil,
		prSelectPartialSnapshot,
		start.Add(50*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	state, err := readPRSelectFairnessFile(filepath.Join(layoutFor(root).SchedulerDir(), prSelectFairnessFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Candidates) != 1 || state.Candidates[0].Number != unobserved.Number {
		t.Fatalf("fairness candidates = %+v, want only unobserved PR #%d retained", state.Candidates, unobserved.Number)
	}
}

func TestRunPRSelectWebhookTargetPreservesUnobservedEligibility(t *testing.T) {
	const (
		targetedNumber   = 20
		unobservedNumber = 10
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(targetedNumber, "targeted PR")
	server.addOpenPR(
		targetedNumber,
		"goobers/implementation/targeted",
		"main",
		"targeted-head",
		"base",
		false,
		nil,
		nil,
	)
	server.addIssue(unobservedNumber, "unobserved PR")
	server.addOpenPR(
		unobservedNumber,
		"goobers/implementation/unobserved",
		"main",
		"unobserved-head",
		"base",
		false,
		nil,
		nil,
	)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "targeted-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv(executor.TriggerRefEnvVar, "github-webhook:pull_request#"+strconv.Itoa(targetedNumber))

	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	seededAt := time.Now().UTC().Add(-30 * time.Minute)
	seeded := []providers.PullRequestSummary{
		{Number: targetedNumber, HeadSHA: "targeted-head"},
		{Number: unobservedNumber, HeadSHA: "unobserved-head"},
	}
	if _, err := observePRSelectEligibility(
		root, repo, seeded, seeded, prSelectCompleteSnapshot, seededAt,
	); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #"+strconv.Itoa(targetedNumber)) {
		t.Fatalf("targeted pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	state, err := readPRSelectFairnessFile(filepath.Join(layoutFor(root).SchedulerDir(), prSelectFairnessFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range state.Candidates {
		if entry.Number == unobservedNumber {
			if !entry.EligibleSince.Equal(seededAt) {
				t.Fatalf("unobserved PR eligibleSince = %s, want preserved timestamp %s", entry.EligibleSince, seededAt)
			}
			return
		}
	}
	t.Fatalf("unobserved PR #%d was pruned from fairness state: %+v", unobservedNumber, state.Candidates)
}

func TestRunPRSelectWebhookTargetYieldsToUnobservedGuardedPR(t *testing.T) {
	const (
		targetedNumber = 1
		guardedNumber  = 900
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(targetedNumber, "targeted PR")
	server.addOpenPR(
		targetedNumber,
		"goobers/implementation/targeted",
		"main",
		"targeted-head",
		"base",
		false,
		nil,
		nil,
	)
	server.addIssue(guardedNumber, "guarded PR")
	server.addOpenPR(
		guardedNumber,
		"goobers/implementation/guarded",
		"main",
		"guarded-head",
		"base",
		false,
		nil,
		nil,
	)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "guarded-webhook-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv(executor.TriggerRefEnvVar, "github-webhook:pull_request#"+strconv.Itoa(targetedNumber))

	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	seededAt := time.Now().UTC().Add(-prSelectStarvationLimit - time.Minute)
	guarded := []providers.PullRequestSummary{{Number: guardedNumber, HeadSHA: "guarded-head"}}
	if _, err := observePRSelectEligibility(
		root, repo, guarded, guarded, prSelectCompleteSnapshot, seededAt,
	); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #"+strconv.Itoa(guardedNumber)) {
		t.Fatalf("guarded webhook pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile("selected-pr.json")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	maxWaitSeconds, err := strconv.ParseInt(result["maxEligibleWaitSeconds"], 10, 64)
	if err != nil {
		t.Fatalf("maxEligibleWaitSeconds = %q: %v", result["maxEligibleWaitSeconds"], err)
	}
	if result["number"] != strconv.Itoa(guardedNumber) ||
		result["starvationGuarded"] != "true" ||
		result["starvedEligiblePRsCsv"] != strconv.Itoa(guardedNumber) ||
		maxWaitSeconds <= int64(prSelectStarvationLimit/time.Second) {
		t.Fatalf("guarded webhook selection = %#v, want guarded PR #%d and its starvation metrics", result, guardedNumber)
	}
}

func TestPRSelectRankingIsDeterministicForIdenticalInputs(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	eligible := []providers.PullRequestSummary{{Number: 30}, {Number: 10}, {Number: 20}}
	since := map[int]time.Time{
		10: now.Add(-30 * time.Minute),
		20: now.Add(-30 * time.Minute),
		30: now.Add(-45 * time.Minute),
	}
	blockedDependents := map[int]int{10: 1}

	first, firstPriorities, firstMetrics := rankEligiblePullRequests(eligible, blockedDependents, since, now)
	second, secondPriorities, secondMetrics := rankEligiblePullRequests(eligible, blockedDependents, since, now)
	if !reflect.DeepEqual(first, second) ||
		!reflect.DeepEqual(firstPriorities, secondPriorities) ||
		!reflect.DeepEqual(firstMetrics, secondMetrics) {
		t.Fatalf("identical ranking inputs produced different results:\nfirst=%v %#v %#v\nsecond=%v %#v %#v",
			prNumbers(first), firstPriorities, firstMetrics,
			prNumbers(second), secondPriorities, secondMetrics,
		)
	}
}

func TestPRSelectReportsStarvationMetrics(t *testing.T) {
	const prNumber = 701
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "aged pr")
	server.addOpenPR(prNumber, "goobers/implementation/aged", "main", "aged-head", "base", false, nil, nil)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-aged")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")

	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	seededAt := time.Now().UTC().Add(-prSelectStarvationLimit - time.Minute)
	aged := []providers.PullRequestSummary{{Number: prNumber, HeadSHA: "aged-head"}}
	if _, err := observePRSelectEligibility(
		root, repo, aged, aged, prSelectCompleteSnapshot, seededAt,
	); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile("selected-pr.json")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	waitSeconds, err := strconv.ParseInt(result["eligibleWaitSeconds"], 10, 64)
	if err != nil {
		t.Fatalf("eligibleWaitSeconds = %q: %v", result["eligibleWaitSeconds"], err)
	}
	if waitSeconds <= int64(prSelectStarvationLimit/time.Second) {
		t.Fatalf("eligibleWaitSeconds = %d, want a reported violation over one hour", waitSeconds)
	}
	if result["starvationGuarded"] != "true" || result["starvedEligiblePRsCsv"] != strconv.Itoa(prNumber) {
		t.Fatalf("fairness outputs = %#v, want guarded PR #%d reported starved", result, prNumber)
	}
}

func TestPRSelectRemediationBlockedPRLosesAccumulatedAge(t *testing.T) {
	const prNumber = 702
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "remediation blocked pr")
	server.addOpenPR(
		prNumber,
		"goobers/implementation/remediation-blocked",
		"main",
		"blocked-head",
		"base",
		false,
		[]string{needsRemediationLabel},
		nil,
	)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-blocked")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")

	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	blocked := []providers.PullRequestSummary{{Number: prNumber, HeadSHA: "blocked-head"}}
	if _, err := observePRSelectEligibility(
		root, repo, blocked, blocked, prSelectCompleteSnapshot, time.Now().UTC().Add(-2*time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("blocked pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	server.prs[prNumber].labels = nil
	server.mu.Unlock()

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("unblocked pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile("selected-pr.json")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["number"] != strconv.Itoa(prNumber) ||
		result["agingBoost"] != "0" ||
		result["starvationGuarded"] != "false" {
		t.Fatalf("selection after remediation = %#v, want PR #%d with reset age", result, prNumber)
	}
}

func prNumbers(prs []providers.PullRequestSummary) []int {
	numbers := make([]int, len(prs))
	for i, pr := range prs {
		numbers[i] = pr.Number
	}
	return numbers
}
