package main

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// Characterization of the pure remediation-selection path that the local
// schedule-demand counter and the pr-remediation / pr-select stages share
// (#8131). It pins call counts, ordering, and exact error text for the paths the
// stage-level suites reach only through a forge fake, so that moving the logic
// into its own package cannot change what either caller observes.

type selectionProvider struct {
	remediationProvider

	workItems map[int]providers.WorkItem
	prs       map[int]providers.PullRequestSummary
	comments  map[int][]providers.Comment
	tips      map[string]string
	merges    map[string]string // "<tip>...<head>" -> merge-base

	errWorkItem, errComments, errTip, errCompare error

	workItemCalls []int
	tipCalls      []string
	compareCalls  []string
}

func (p *selectionProvider) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	n, _ := strconv.Atoi(id)
	p.workItemCalls = append(p.workItemCalls, n)
	if p.errWorkItem != nil {
		return providers.WorkItem{}, p.errWorkItem
	}
	return p.workItems[n], nil
}

func (p *selectionProvider) GetPullRequest(_ context.Context, _ providers.RepositoryRef, id string) (providers.PullRequestSummary, error) {
	n, _ := strconv.Atoi(id)
	return p.prs[n], nil
}

func (p *selectionProvider) ListComments(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.Comment, error) {
	if p.errComments != nil {
		return nil, p.errComments
	}
	n, _ := strconv.Atoi(id)
	return p.comments[n], nil
}

func (p *selectionProvider) BranchTipSHA(_ context.Context, _ providers.RepositoryRef, branch string) (string, error) {
	p.tipCalls = append(p.tipCalls, branch)
	if p.errTip != nil {
		return "", p.errTip
	}
	return p.tips[branch], nil
}

func (p *selectionProvider) CompareCommits(_ context.Context, _ providers.RepositoryRef, base, head string) (providers.CompareResult, error) {
	p.compareCalls = append(p.compareCalls, base+"..."+head)
	if p.errCompare != nil {
		return providers.CompareResult{}, p.errCompare
	}
	return providers.CompareResult{MergeBaseSHA: p.merges[base+"..."+head]}, nil
}

var selectionRepo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

func TestNamedBlockerResolutionIsDecidedOnProviderStateAndDeduplicated(t *testing.T) {
	p := &selectionProvider{workItems: map[int]providers.WorkItem{
		11: {State: "open"},
		12: {State: "CLOSED"},
		13: {State: "Open"},
	}}
	live, err := filterLiveBlockedOnSiblingBlockers(t.Context(), p, selectionRepo, []int{13, 11, 13, 12, 11})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, []int{13, 11}) {
		t.Fatalf("live blockers = %v, want [13 11]: input order, open only, state compared case-insensitively", live)
	}
	if !reflect.DeepEqual(p.workItemCalls, []int{13, 11, 12}) {
		t.Fatalf("provider lookups = %v, want one per distinct blocker in first-seen order", p.workItemCalls)
	}

	boom := errors.New("forge unavailable")
	p = &selectionProvider{errWorkItem: boom}
	if live, err := filterLiveBlockedOnSiblingBlockers(t.Context(), p, selectionRepo, []int{1, 2}); live != nil || !errors.Is(err, boom) || err.Error() != boom.Error() {
		t.Fatalf("lookup failure = %v, %v; want the provider's own error and no partial result", live, err)
	}
	if len(p.workItemCalls) != 1 {
		t.Fatalf("a failing lookup did not stop the scan: %v", p.workItemCalls)
	}
}

func TestBehindLiveBaseResolvesEachBaseTipOnceAndComparesMergeBaseToIt(t *testing.T) {
	p := &selectionProvider{
		tips:   map[string]string{"main": "tip-main", "release": "tip-rel"},
		merges: map[string]string{"tip-main...head-a": "tip-main", "tip-main...head-b": "older", "tip-rel...head-c": "tip-rel"},
	}
	tips := map[string]string{}
	for _, tc := range []struct {
		pr   providers.PullRequestSummary
		want bool
	}{
		{providers.PullRequestSummary{Number: 1, Base: "main", HeadSHA: "head-a"}, false},
		{providers.PullRequestSummary{Number: 2, Base: "main", HeadSHA: "head-b"}, true},
		{providers.PullRequestSummary{Number: 3, Base: "release", HeadSHA: "head-c"}, false},
	} {
		got, err := pullRequestBehindLiveBase(t.Context(), p, selectionRepo, tc.pr, tips)
		if err != nil || got != tc.want {
			t.Fatalf("PR #%d behind = %v (%v), want %v", tc.pr.Number, got, err, tc.want)
		}
	}
	if !reflect.DeepEqual(p.tipCalls, []string{"main", "release"}) {
		t.Fatalf("base tip lookups = %v, want one per distinct base", p.tipCalls)
	}
	if !reflect.DeepEqual(tips, map[string]string{"main": "tip-main", "release": "tip-rel"}) {
		t.Fatalf("tip cache = %v", tips)
	}
}

func TestBehindLiveBaseErrorTextIsStable(t *testing.T) {
	pr := providers.PullRequestSummary{Number: 7, Base: "main", HeadSHA: "head"}
	tipErr, cmpErr := errors.New("tip boom"), errors.New("compare boom")
	for name, tc := range map[string]struct {
		p       *selectionProvider
		want    string
		wrapped error
	}{
		"tip lookup fails":          {&selectionProvider{errTip: tipErr}, `resolve live base branch "main": tip boom`, tipErr},
		"compare fails":             {&selectionProvider{tips: map[string]string{"main": "t"}, errCompare: cmpErr}, "compare live base with PR #7 head: compare boom", cmpErr},
		"compare has no merge base": {&selectionProvider{tips: map[string]string{"main": "t"}}, "compare live base with PR #7 head returned no merge base", nil},
	} {
		tips := map[string]string{}
		_, err := pullRequestBehindLiveBase(t.Context(), tc.p, selectionRepo, pr, tips)
		if err == nil || err.Error() != tc.want || (tc.wrapped != nil && !errors.Is(err, tc.wrapped)) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if name == "tip lookup fails" && len(tips) != 0 {
			t.Errorf("a failed tip lookup was cached: %v", tips)
		}
	}
}

func TestRemediationFilterErrorsNameThePullRequestAndStopTheScan(t *testing.T) {
	boom := errors.New("comments boom")
	escalated := providers.PullRequestSummary{Number: 21, Labels: []string{remediationEscalatedLabel}}
	parked := providers.PullRequestSummary{Number: 22, Labels: []string{blockedOnSiblingLabel}}
	for name, tc := range map[string]struct {
		pr   providers.PullRequestSummary
		want string
	}{
		"escalation state":         {escalated, "check escalation state for PR #21: comments boom"},
		"blocked-on-sibling state": {parked, "check blocked-on-sibling state for PR #22: comments boom"},
	} {
		p := &selectionProvider{errComments: boom}
		eligible, dependents, err := filterRemediationPullRequests(t.Context(), p, selectionRepo, []providers.PullRequestSummary{tc.pr}, nil)
		if err == nil || err.Error() != tc.want || !errors.Is(err, boom) || eligible != nil || dependents != nil {
			t.Errorf("%s: %v %v %v, want %q and no result", name, eligible, dependents, err, tc.want)
		}
	}
}

func TestRemediationFilterKeepsOrderCountsLiveDependentsAndSkipsHeldBranches(t *testing.T) {
	record := func(blockers ...int) []providers.Comment {
		body, err := blockedOnSiblingComment(blockedOnSiblingState{Blockers: blockers, Reason: "waiting"})
		if err != nil {
			t.Fatal(err)
		}
		return []providers.Comment{{ID: "c", Body: body}}
	}
	p := &selectionProvider{
		workItems: map[int]providers.WorkItem{31: {State: "open"}, 32: {State: "closed"}},
		comments:  map[int][]providers.Comment{41: record(31, 32), 42: record(32)},
	}
	prs := []providers.PullRequestSummary{
		{Number: 40, Head: "team/a"},
		{Number: 41, Head: "team/b", Labels: []string{blockedOnSiblingLabel, needsRemediationLabel}},
		{Number: 42, Head: "team/c", Labels: []string{blockedOnSiblingLabel}},
		{Number: 43, Head: "team/held"},
		{Number: 44, Head: "team/d", Labels: []string{providers.LabelNeedsHuman}},
	}
	eligible, dependents, err := filterRemediationPullRequests(t.Context(), p, selectionRepo, prs, map[string]bool{"team/held": true})
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, pr := range eligible {
		numbers = append(numbers, pr.Number)
	}
	// 41 stays (parked behind live #31 but already labelled needs-remediation),
	// 42's only blocker is closed so it is not held, 43 is held, 44 needs a human.
	if !reflect.DeepEqual(numbers, []int{40, 41, 42}) {
		t.Fatalf("eligible = %v, want [40 41 42] in input order", numbers)
	}
	if !reflect.DeepEqual(dependents, map[int]int{31: 1}) {
		t.Fatalf("live dependents = %v, want only #31 counted once", dependents)
	}
}

func TestCandidateSelectionOrdersStrongSignalsFirstAndOnlyChecksBehindBaseWhenNothingIsStronger(t *testing.T) {
	failing := providers.PullRequestSummary{Number: 5, CheckState: providers.CheckStateFailing}
	labelled := providers.PullRequestSummary{Number: 3, Labels: []string{needsRemediationLabel}}
	plain := providers.PullRequestSummary{Number: 9}
	calls := 0
	behind := func(providers.PullRequestSummary) (bool, error) { calls++; return true, nil }

	got, best, err := selectRemediationCandidates([]providers.PullRequestSummary{plain, failing, labelled}, nil, behind)
	if err != nil || calls != 0 || best != remediationPriorityNeedsRemediation {
		t.Fatalf("strong tier: err=%v behind-base checks=%d best=%v", err, calls, best)
	}
	if numbers(got) != "3,5" {
		t.Fatalf("strong candidates = %s, want 3,5: one oldest-first order across both signals", numbers(got))
	}

	// Nothing strong: a lander is eligible only if crowned or alone.
	got, best, err = selectRemediationCandidates([]providers.PullRequestSummary{plain, {Number: 8}}, map[int]int{8: 2}, behind)
	if err != nil || best != remediationPriorityBehindBase || numbers(got) != "8" || calls != 1 {
		t.Fatalf("crowned lander: %s best=%v err=%v checks=%d, want only #8 after one check", numbers(got), best, err, calls)
	}
	calls = 0
	got, _, err = selectRemediationCandidates([]providers.PullRequestSummary{plain}, nil, behind)
	if err != nil || numbers(got) != "9" || calls != 1 {
		t.Fatalf("solitary PR: %s err=%v checks=%d, want #9 after one check", numbers(got), err, calls)
	}
	calls = 0
	got, best, err = selectRemediationCandidates([]providers.PullRequestSummary{plain, {Number: 10}}, nil, behind)
	if err != nil || len(got) != 0 || best != remediationPriorityNone || calls != 0 {
		t.Fatalf("uncrowned wave: %s best=%v err=%v checks=%d, want none and no behind-base check", numbers(got), best, err, calls)
	}

	boom := errors.New("behind boom")
	_, _, err = selectRemediationCandidates([]providers.PullRequestSummary{plain}, nil, func(providers.PullRequestSummary) (bool, error) { return false, boom })
	if !errors.Is(err, boom) || err.Error() != boom.Error() {
		t.Fatalf("behind-base failure = %v, want the callback's own error", err)
	}
}

func numbers(prs []providers.PullRequestSummary) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		parts = append(parts, strconv.Itoa(pr.Number))
	}
	return strings.Join(parts, ",")
}
