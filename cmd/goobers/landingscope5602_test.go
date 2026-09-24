package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// The #5602 shape: a managed PR overlaps a PR under the branch namespace that
// this instance cannot land — outside headPrefixes (left by another instance
// configuration), or opted out with goobers:no-merge-review.
const (
	scope5602Unlandable = 7                                       // goobers/tb-other-implementation/...
	scope5602OptedOut   = 8                                       // goobers/implementation/..., no-merge-review
	scope5602Selected   = 10                                      // the managed selection
	scope5602Managed    = 11                                      // a managed sibling that must still serialize
	scope5602Human      = 20                                      // outside the namespace entirely
	scope5602OtherHead  = "goobers/tb-other-implementation/run-7" // inside the namespace, outside headPrefixes
)

func addScope5602PRs(server *fakeGitHubServer) {
	shared := []fakePRFile{{path: "src/TransactionParser.cs", status: "modified", additions: 1}}
	server.addIssue(scope5602Unlandable, "out-of-scope PR")
	server.addOpenPR(scope5602Unlandable, scope5602OtherHead, "main", "sha7", "base", false, nil, shared)
	server.addIssue(scope5602OptedOut, "opted-out PR")
	server.addOpenPR(scope5602OptedOut, "goobers/implementation/run-8", "main", "sha8", "base", false,
		[]string{noMergeReviewLabel}, shared)
	server.addIssue(scope5602Selected, "selected PR")
	server.addOpenPR(scope5602Selected, "goobers/implementation/run-10", "main", "sha10", "base", false, nil, shared)
	server.addIssue(scope5602Managed, "managed sibling")
	server.addOpenPR(scope5602Managed, "goobers/implementation/run-11", "main", "sha11", "base", false, nil, shared)
	server.addIssue(scope5602Human, "human PR")
	server.addOpenPR(scope5602Human, "feature/human-change", "main", "sha20", "base", false, nil, shared)
}

type scope5602SiblingContext struct {
	Siblings          []siblingPR `json:"siblings"`
	Overlapping       []int       `json:"overlappingSiblings"`
	UnlandableCSV     string      `json:"unlandableSiblingsCsv"`
	HasSiblingOverlap string      `json:"hasSiblingOverlap"`
}

func runScope5602GatherSiblingContext(t *testing.T, root string, selected int, advisory bool) scope5602SiblingContext {
	t.Helper()
	t.Setenv(executor.InputEnvVar("selectedNumber"), strconv.Itoa(selected))
	t.Setenv(executor.InputEnvVar("advisoryMode"), map[bool]string{true: "true", false: "false"}[advisory])
	dir := t.TempDir()
	t.Chdir(dir)
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var got scope5602SiblingContext
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	return got
}

func siblingNumbers(siblings []siblingPR) []int {
	out := make([]int, 0, len(siblings))
	for _, s := range siblings {
		out = append(out, s.Number)
	}
	sort.Ints(out)
	return out
}

func sortedCSV(csv string) []string {
	if csv == "" {
		return nil
	}
	out := strings.Split(csv, ",")
	sort.Strings(out)
	return out
}

// TestGatherSiblingContextSequencesOnlyLandableSiblings is #5602's producer
// pin. A managed selection's sibling set is the set this instance can land:
// the out-of-scope and opted-out PRs overlap the selected PR's file but are
// neither siblings nor overlapping siblings, and they are published as
// unlandable so the election can drop a reviewer-named one too. The managed
// sibling still overlaps, so a genuine cluster still serializes.
func TestGatherSiblingContextSequencesOnlyLandableSiblings(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addScope5602PRs(server)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-5602")
	routeMergeReviewTestRepo(t)

	got := runScope5602GatherSiblingContext(t, root, scope5602Selected, false)
	if want := []int{scope5602Managed}; !reflect.DeepEqual(siblingNumbers(got.Siblings), want) {
		t.Fatalf("siblings = %v, want only the landable managed sibling %v", siblingNumbers(got.Siblings), want)
	}
	if want := []int{scope5602Managed}; !reflect.DeepEqual(got.Overlapping, want) {
		t.Fatalf("overlappingSiblings = %v, want %v: the managed cluster must still serialize", got.Overlapping, want)
	}
	if want := []string{"7", "8"}; !reflect.DeepEqual(sortedCSV(got.UnlandableCSV), want) {
		t.Fatalf("unlandableSiblingsCsv = %q, want the out-of-scope and opted-out PRs %v (the human PR is outside the namespace)",
			got.UnlandableCSV, want)
	}
}

// TestGatherSiblingContextAdvisorySelectionKeepsEveryOpenPR pins that #5602
// narrows only managed sequencing: an advisory selection still receives every
// open PR as review context and publishes no unlandable set.
func TestGatherSiblingContextAdvisorySelectionKeepsEveryOpenPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addScope5602PRs(server)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-5602-advisory")
	routeMergeReviewTestRepo(t)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)

	got := runScope5602GatherSiblingContext(t, root, scope5602Human, true)
	want := []int{scope5602Unlandable, scope5602OptedOut, scope5602Selected, scope5602Managed}
	if !reflect.DeepEqual(siblingNumbers(got.Siblings), want) {
		t.Fatalf("advisory siblings = %v, want every other open PR %v", siblingNumbers(got.Siblings), want)
	}
	if got.UnlandableCSV != "" {
		t.Fatalf("advisory unlandableSiblingsCsv = %q, want empty", got.UnlandableCSV)
	}
}

// TestElectionDropsUnlandableSiblings is #5602's election pin: the set
// gather-sibling-context publishes is excluded from candidacy and from every
// blocker set, so a PR this instance cannot land never becomes or precedes
// the FIFO lander — even when the reviewer names it — while a managed
// predecessor still holds its successors back.
func TestElectionDropsUnlandableSiblings(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	provider := server.newGitHubProvider("token")
	prs := []providers.PullRequestSummary{
		{Number: scope5602Unlandable, State: "open", Head: scope5602OtherHead},
		{Number: scope5602OptedOut, State: "open", Head: "goobers/implementation/run-8", Labels: []string{noMergeReviewLabel}},
		{Number: scope5602Selected, State: "open", Head: "goobers/implementation/run-10"},
		{Number: scope5602Managed, State: "open", Head: "goobers/implementation/run-11"},
	}
	var stderr bytes.Buffer
	excluded, err := electionExcludedSet(context.Background(), provider, repo, prs, "7,8", &stderr)
	if err != nil {
		t.Fatalf("electionExcludedSet: %v (stderr %q)", err, stderr.String())
	}
	if want := map[int]bool{scope5602Unlandable: true, scope5602OptedOut: true}; !reflect.DeepEqual(excluded, want) {
		t.Fatalf("excluded = %v, want %v", excluded, want)
	}

	fifo, _ := resolveElectionPolicy("fifo")
	named := func(blockers ...int) []apiv1.Finding {
		return []apiv1.Finding{{Severity: apiv1.SeverityWarning, Class: apiv1.FindingCrossPRBlocked, BlockingPRs: blockers}}
	}
	if !electionDecision(named(scope5602Unlandable, scope5602OptedOut), scope5602Selected, fifo, excluded) {
		t.Fatal("PR #10 not elected: lower-numbered PRs this instance cannot land must not precede the lander")
	}
	if electionDecision(named(scope5602Unlandable, scope5602OptedOut), scope5602Selected, fifo, nil) {
		t.Fatal("control: without the unlandable set PR #10 was elected; the pin above proves nothing")
	}
	if electionDecision(named(scope5602Unlandable, scope5602Selected), scope5602Managed, fifo, excluded) {
		t.Fatal("PR #11 elected ahead of managed PR #10: managed siblings must still serialize")
	}
	if got := predecessorBlockers(scope5602Managed, []int{scope5602Unlandable, scope5602OptedOut, scope5602Selected}, fifo, excluded); !reflect.DeepEqual(got, []int{scope5602Selected}) {
		t.Fatalf("recorded predecessors of #11 = %v, want only managed #10", got)
	}

	none, err := electionExcludedSet(context.Background(), provider, repo, prs, "", &stderr)
	if err != nil || len(none) != 0 {
		t.Fatalf("no unlandable input: excluded = %v, err = %v; want the pre-#5602 empty set", none, err)
	}
}

// TestUnlandableSiblingSetMatchesSelectionScope pins the ownership test the
// sibling set now shares with pr-select: in the namespace and outside
// headPrefixes, or opted out, is unlandable; a managed PR is not; a PR
// outside the namespace keeps its old treatment and is not named.
func TestUnlandableSiblingSetMatchesSelectionScope(t *testing.T) {
	prs := []providers.PullRequestSummary{
		{Number: scope5602Unlandable, Head: scope5602OtherHead},
		{Number: scope5602OptedOut, Head: "goobers/implementation/run-8", Labels: []string{noMergeReviewLabel}},
		{Number: scope5602Managed, Head: "goobers/implementation/run-11"},
		{Number: scope5602Human, Head: "feature/human-change"},
	}
	got := unlandableSiblingSet(prs, []string{"goobers/implementation/"}, "")
	if want := map[int]bool{scope5602Unlandable: true, scope5602OptedOut: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unlandableSiblingSet = %v, want %v", got, want)
	}
	// Ownership by identity (#1780) is the same test pr-select applies.
	prs[0].Author = "goobers-daemon"
	if got := unlandableSiblingSet(prs, []string{"goobers/implementation/"}, "goobers-daemon"); got[scope5602Unlandable] {
		t.Fatalf("unlandableSiblingSet = %v: a daemon-authored PR is selectable under identity ownership, so it is landable", got)
	}
}

// TestBlockedOnSiblingSelectionHoldIgnoresUnlandableBlocker covers a park
// record written before #5602 that names a PR this instance cannot land.
func TestBlockedOnSiblingSelectionHoldIgnoresUnlandableBlocker(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(scope5602Unlandable, "out-of-scope blocker, still open")
	server.addIssue(scope5602Selected, "parked PR")
	server.addComment(scope5602Selected, blockedOnSiblingCommentFor(t, scope5602Unlandable))
	provider := server.newGitHubProvider("token")
	parked := providers.PullRequestSummary{
		Number: scope5602Selected, State: "open", Base: "main",
		Head: "goobers/implementation/run-10", Labels: []string{blockedOnSiblingLabel},
	}
	ctx := context.Background()

	held, reason, err := blockedOnSiblingSelectionHold(ctx, provider, repo, parked, nil)
	if err != nil || !held {
		t.Fatalf("control: held = %t (%q), err = %v; want held while the named blocker is open and in scope", held, reason, err)
	}
	held, reason, err = blockedOnSiblingSelectionHold(ctx, provider, repo, parked, map[int]bool{scope5602Unlandable: true})
	if err != nil || held {
		t.Fatalf("held = %t (%q), err = %v; want not held: its only blocker is one this instance cannot land", held, reason, err)
	}
}

// TestPRSelectSelectsPRParkedBehindUnlandableSibling is the observed shape end
// to end: the managed PR has been parked since before #5602 behind an open PR
// outside headPrefixes. Selection must take it rather than report the queue
// parked until a human closes the other PR.
func TestPRSelectSelectsPRParkedBehindUnlandableSibling(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(scope5602Unlandable, "out-of-scope PR")
	server.addOpenPR(scope5602Unlandable, scope5602OtherHead, "main", "sha7", "base", false, nil, nil)
	server.addIssue(scope5602Selected, "parked managed PR")
	server.addOpenPR(scope5602Selected, "goobers/implementation/run-10", "main", "sha10", "base", false,
		[]string{blockedOnSiblingLabel}, nil)
	server.addComment(scope5602Selected, blockedOnSiblingCommentFor(t, scope5602Unlandable))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-5602-select")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(workDir, "selected-pr.json"))

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #10") {
		t.Fatalf("stdout = %q, want PR #10 selected: its only recorded blocker is outside headPrefixes", stdout)
	}
}
