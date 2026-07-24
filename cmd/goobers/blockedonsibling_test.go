package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type deadlineRecordingClient struct {
	client  providers.HTTPClient
	mu      sync.Mutex
	paths   []string
	missing []string
}

func (c *deadlineRecordingClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.paths = append(c.paths, req.URL.Path)
	if _, ok := req.Context().Deadline(); !ok {
		c.missing = append(c.missing, req.URL.Path)
	}
	c.mu.Unlock()
	return c.client.Do(req)
}

func (c *deadlineRecordingClient) requests() (paths, missing []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...), append([]string(nil), c.missing...)
}

// blockedOnSiblingCommentFor is a test helper: the sticky comment apply-verdict
// would post for a PR blocked behind the given blocker PR numbers.
func blockedOnSiblingCommentFor(t *testing.T, blockers ...int) string {
	t.Helper()
	c, err := blockedOnSiblingComment(blockedOnSiblingState{
		Blockers: blockers, Reason: "waits behind sibling(s)", HeadSHA: "h", BaseSHA: "b",
	})
	if err != nil {
		t.Fatalf("blockedOnSiblingComment: %v", err)
	}
	return c
}

// TestBlockedOnSiblingStillBlocks exercises #748's blocker-aware self-heal in
// isolation: a PR not carrying the label is never blocked; a labeled PR with no
// recorded blocker set fails OPEN (a re-review re-establishes the record); a
// labeled PR with any still-open blocker stays blocked; and a labeled PR whose
// every named blocker has resolved (merged or closed — both leave the blocker
// no-longer-open) self-heals to unblocked.
func TestBlockedOnSiblingStillBlocks(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("no label, never blocked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "pr 1")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 1}

		blocked, err := blockedOnSiblingStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("blockedOnSiblingStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — PR carries no blocked-on-sibling label")
		}
	})

	t.Run("labeled but no recorded blocker set fails open", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(2, "pr 2")
		server.addComment(2, "waiting on something, unspecified") // no payload
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 2, Labels: []string{blockedOnSiblingLabel}}

		blocked, err := blockedOnSiblingStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("blockedOnSiblingStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — labeled with no recorded blocker set must fail open")
		}
	})

	t.Run("open blocker stays blocked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(3, "pr 3")
		server.addIssue(700, "blocker still open")
		server.addComment(3, blockedOnSiblingCommentFor(t, 700))
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 3, Labels: []string{blockedOnSiblingLabel}}

		blocked, err := blockedOnSiblingStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("blockedOnSiblingStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — named blocker #700 is still open")
		}
	})

	t.Run("all blockers resolved self-heals", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(4, "pr 4")
		server.addIssue(701, "merged blocker")
		server.closeIssue(701) // merged or closed -> not open -> resolved
		server.addComment(4, blockedOnSiblingCommentFor(t, 701))
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 4, Labels: []string{blockedOnSiblingLabel}}

		blocked, err := blockedOnSiblingStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("blockedOnSiblingStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — every named blocker has resolved")
		}
	})

	t.Run("mixed open and resolved stays blocked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(5, "pr 5")
		server.addIssue(702, "resolved blocker")
		server.closeIssue(702)
		server.addIssue(703, "still-open blocker")
		server.addComment(5, blockedOnSiblingCommentFor(t, 702, 703))
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 5, Labels: []string{blockedOnSiblingLabel}}

		blocked, err := blockedOnSiblingStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("blockedOnSiblingStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — one of two named blockers (#703) is still open")
		}
	})

	t.Run("actively demoted blocker no longer blocks", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(6, "pr 6")
		server.addIssue(704, "demoted blocker", mergeDemotedLabel)
		server.addOpenPR(704, "goobers/implementation/704", "main", "h704", "base", false, []string{mergeDemotedLabel}, nil)
		demotion, err := mergeDemotionComment(mergeDemotionState{Demoted: true, HeadSHA: "h704"})
		if err != nil {
			t.Fatal(err)
		}
		server.addComment(704, demotion)
		server.addComment(6, blockedOnSiblingCommentFor(t, 704))
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 6, Labels: []string{blockedOnSiblingLabel}}

		blocked, err := blockedOnSiblingStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("blockedOnSiblingStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — snapshot-valid demotion lets successors drain")
		}
	})
}

// TestUnparkResolvedSiblings is #748's post-merge push-half acceptance: after a
// merge, a blocked-on-sibling PR whose named blocker just resolved has its
// (now-stale) label cleared, while a PR with a still-open blocker and a PR
// carrying no such label are both left untouched. Also covers the #542 bypass
// path — the same live blocker-state check runs regardless of how the blocker
// closed, so an out-of-band merge is caught too.
func TestUnparkResolvedSiblings(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)

	// #700 is the blocker that just merged (closed).
	server.addIssue(700, "blocker that merged")
	server.closeIssue(700)

	// #810: parked behind the now-resolved #700 -> should be unparked.
	server.addIssue(810, "resolved-blocker pr")
	server.addOpenPR(810, "goobers/impl/a", "main", "h810", "b810", false, []string{blockedOnSiblingLabel}, nil)
	server.addComment(810, blockedOnSiblingCommentFor(t, 700))

	// #811: parked behind #700 AND still-open #712 -> should stay parked.
	server.addIssue(712, "still-open blocker")
	server.addIssue(811, "still-blocked pr")
	server.addOpenPR(811, "goobers/impl/b", "main", "h811", "b811", false, []string{blockedOnSiblingLabel}, nil)
	server.addComment(811, blockedOnSiblingCommentFor(t, 700, 712))

	// #812: not parked at all -> untouched.
	server.addIssue(812, "unrelated pr")
	server.addOpenPR(812, "goobers/impl/c", "main", "h812", "b812", false, nil, nil)

	provider := server.newGitHubProvider("token")
	unparked, errs := unparkResolvedSiblings(context.Background(), provider, repo, 700, "main", io.Discard)
	if len(errs) != 0 {
		t.Fatalf("unparkResolvedSiblings errs = %v, want none", errs)
	}
	if len(unparked) != 1 || unparked[0] != 810 {
		t.Fatalf("unparked = %v, want exactly [810]", unparked)
	}
}

// TestPRSelectSkipsBlockedOnSibling is #748 AC1's merge-review-side acceptance:
// a PR parked blocked-on-sibling with a still-open blocker is not selected.
func TestPRSelectSkipsBlockedOnSibling(t *testing.T) {
	const prNumber = 810
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "blocked pr")
	server.addIssue(700, "open blocker")
	server.addOpenPR(prNumber, "goobers/implementation/blocked", "main", "h810", "b810", false, []string{blockedOnSiblingLabel}, nil)
	server.addComment(prNumber, blockedOnSiblingCommentFor(t, 700))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-810")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work — a blocked-on-sibling PR with an open blocker must not be selected", stdout)
	}
}

// TestPRSelectSelectsSelfHealedSibling is #748 AC2's merge-review-side
// acceptance: once every named blocker resolves, the parked PR re-enters
// selection automatically — no human clears the label.
func TestPRSelectSelectsSelfHealedSibling(t *testing.T) {
	const prNumber = 811
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "self-healed pr")
	server.addIssue(700, "blocker that merged")
	server.closeIssue(700) // the blocker merged since the PR was parked
	server.addOpenPR(prNumber, "goobers/implementation/healed", "main", "h811", "b811", false, []string{blockedOnSiblingLabel}, nil)
	server.addComment(prNumber, blockedOnSiblingCommentFor(t, 700))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-811")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "811") {
		t.Fatalf("stdout = %q, want PR #811 selected — its blocker resolved, self-healing the block", stdout)
	}
}

func TestPRSelectPrefersPRWithMostBlockedDependents(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	for _, number := range []int{101, 102, 103} {
		server.addIssue(number, "eligible pr")
		server.addOpenPR(number, "goobers/implementation/"+strconv.Itoa(number), "main", "head", "base", false, nil, nil)
	}
	for number, blockers := range map[int][]int{
		201: {103},
		202: {103},
		203: {102},
	} {
		server.addIssue(number, "blocked pr")
		server.addOpenPR(number, "goobers/implementation/"+strconv.Itoa(number), "main", "head", "base", false, []string{blockedOnSiblingLabel}, nil)
		server.addComment(number, blockedOnSiblingCommentFor(t, blockers...))
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-priority")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())
	recorder := &deadlineRecordingClient{}
	baseConstructor := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := baseConstructor(token, opts...)
		recorder.client = provider.Client
		provider.Client = recorder
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = baseConstructor })

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
	if result["number"] != "103" {
		t.Fatalf("selected PR = %q, want #103 with two blocked dependents", result["number"])
	}
	paths, missing := recorder.requests()
	for _, path := range missing {
		if strings.Contains(path, "/issues/") {
			t.Fatalf("blocker scan request without a stage deadline = %s", path)
		}
	}
	var sawComments, sawBlocker bool
	for _, path := range paths {
		sawComments = sawComments || strings.HasSuffix(path, "/issues/201/comments")
		sawBlocker = sawBlocker || strings.HasSuffix(path, "/issues/103")
	}
	if !sawComments || !sawBlocker {
		t.Fatalf("provider requests = %v, want blocker comment and live-blocker lookups", paths)
	}
}

func TestPRSelectCrownedLanderOutranksAgedUnrelatedPR(t *testing.T) {
	const (
		ordinaryNumber  = 1
		landerNumber    = 103
		dependentNumber = 201
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for _, number := range []int{ordinaryNumber, landerNumber} {
		server.addIssue(number, "eligible pr")
		server.addOpenPR(number, "goobers/implementation/"+strconv.Itoa(number), "main", "head-"+strconv.Itoa(number), "base", false, nil, nil)
	}
	server.addIssue(dependentNumber, "parked cluster member")
	server.addOpenPR(dependentNumber, "goobers/implementation/"+strconv.Itoa(dependentNumber), "main", "head-201", "base", false, []string{blockedOnSiblingLabel}, nil)
	server.addComment(dependentNumber, blockedOnSiblingCommentFor(t, landerNumber))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-crowned-lander")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")

	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	ordinary := providers.PullRequestSummary{Number: ordinaryNumber, HeadSHA: "head-1"}
	if _, err := observePRSelectEligibility(
		root,
		repo,
		[]providers.PullRequestSummary{ordinary},
		[]providers.PullRequestSummary{ordinary},
		prSelectCompleteSnapshot,
		time.Now().UTC().Add(-2*prSelectAgingInterval),
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
	if result["number"] != strconv.Itoa(landerNumber) {
		t.Fatalf("selected PR = %q, want crowned lander #%d ahead of 30-minute-old unrelated PR #%d", result["number"], landerNumber, ordinaryNumber)
	}
}

func TestPRSelectWithoutBlockedDependentsPreservesNumberOrder(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for _, number := range []int{112, 111} {
		server.addIssue(number, "eligible pr")
		server.addOpenPR(number, "goobers/implementation/"+strconv.Itoa(number), "main", "head", "base", false, nil, nil)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-no-priority")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
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
	if result["number"] != "111" {
		t.Fatalf("selected PR = %q, want existing lowest-number ordering (#111)", result["number"])
	}
}
