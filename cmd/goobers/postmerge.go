package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// closingKeywordPattern matches GitHub's own issue-closing keyword grammar
// (close/closes/closed, fix/fixes/fixed, resolve/resolves/resolved,
// case-insensitive) followed by a same-repo "#N" reference — the exact
// convention `goobers open-pr` writes ("Fixes #<issueID>", openpr.go).
var closingKeywordPattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)`)

// closingIssueNumbers extracts every distinct issue number a PR body
// references via GitHub's closing-keyword grammar, in first-seen order.
func closingIssueNumbers(body string) []string {
	matches := closingKeywordPattern.FindAllStringSubmatch(body, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

const needsRemediationLabel = "goobers:needs-remediation"

// runPostMerge implements the `goobers post-merge` built-in stage kind
// (issue #361): the two actions that follow a successful merge-review merge.
//
//   - Post-merge fan-out (design doc §7 D7): label every OTHER open PR
//     targeting the same base branch needs-remediation, since the base just
//     moved out from under them — this is what feeds pr-remediation.
//   - Close-out on merge (#355): the merged PR's body is parsed for its
//     closing-keyword issue reference(s) (the same "Fixes #N" convention
//     `goobers open-pr` writes), and each referenced issue is marked done.
//     The work isn't done until the merge, so this replaces
//     `implementation`'s old PR-open-time close (which now only sets
//     status=in-review — cmd/goobers/issuecloseout.go).
//
// Meant to run as the merge-review workflow's stage immediately after a
// successful `goobers merge-pr` (gated on its merged=true output). A PR that
// references no issue, or has no other open PRs to label, is a normal
// outcome (exit 0), not an error — not every merged PR closes a backlog item.
func runPostMerge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("post-merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers post-merge [path]\n\n"+
			"Run the two actions that follow a successful merge: label every other\n"+
			"open PR targeting the same base branch goobers:needs-remediation (the\n"+
			"base just moved out from under them), and mark each issue the merged\n"+
			"PR's body references (Fixes/Closes/Resolves #N) done. Declared input:\n"+
			"pullNumber (required — the just-merged PR). Exit codes: 0 = done\n"+
			"(even if the PR body references no issue, or there are no other open\n"+
			"PRs to label — both are normal outcomes, not errors), 1 = business\n"+
			"error, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Two capabilities are actually used: github:pr:write (poll/list PRs)
	// and github:issues:write (label the other PRs — GitHub's issues API
	// also covers PR labels — and close the referenced issue). Both are
	// checked explicitly before any call is made, matching #360's
	// capability-absent-refuses-first contract; in V0 both resolve to the
	// identical repo credential (runnerwiring.go's credentialedCapabilities),
	// so only the first token is actually needed to construct the provider.
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(prToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))

	pullNumber := providerInput("pullNumber", "")
	if pullNumber == "" {
		pf(stderr, "error: pullNumber input is required\n")
		return 1
	}

	ctx := context.Background()
	poll, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: pullNumber})
	if err != nil {
		return failProviderStage(stderr, "poll merged pull request", err, "")
	}

	labeled, labelErrs := fanOutNeedsRemediation(ctx, provider, repo, poll.Number, poll.BaseBranch)
	for _, lerr := range labelErrs {
		pf(stderr, "warning: %v\n", lerr)
	}

	closed, closeErrs := closeReferencedIssues(ctx, provider, repo, poll.Body, pullNumber)
	for _, cerr := range closeErrs {
		pf(stderr, "warning: %v\n", cerr)
	}

	pf(stdout, "post-merge: labeled %d pr(s) %s, closed %d issue(s)\n", len(labeled), needsRemediationLabel, len(closed))
	return 0
}

// fanOutNeedsRemediation labels every OTHER open PR targeting base
// needs-remediation (design doc §7 D7). Best-effort per PR: one failed
// label-apply is collected as a warning, not fatal to the others or to the
// merge that already succeeded.
func fanOutNeedsRemediation(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, mergedNumber int, base string) (labeled []int, errs []error) {
	if base == "" {
		errs = append(errs, fmt.Errorf("merged PR has no recorded base branch, skipping fan-out"))
		return nil, errs
	}
	// HeadPrefix scopes the fan-out to goober-authored siblings (G1's
	// goober-authored-repo assumption, matching pr-select/gather-sibling-
	// context's own default) — a human/other-agent PR sharing the same base
	// isn't pr-remediation's to touch.
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: repo, Base: base, HeadPrefix: "goobers/"})
	if err != nil {
		errs = append(errs, fmt.Errorf("list open pull requests targeting %s: %w", base, err))
		return nil, errs
	}
	for _, pr := range others {
		if pr.Number == mergedNumber {
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: strconv.Itoa(pr.Number), AddLabels: []string{needsRemediationLabel},
		}); err != nil {
			errs = append(errs, fmt.Errorf("label pr #%d %s: %w", pr.Number, needsRemediationLabel, err))
			continue
		}
		labeled = append(labeled, pr.Number)
	}
	return labeled, errs
}

// closeReferencedIssues marks every issue the merged PR's body references via
// GitHub's closing-keyword grammar (Fixes/Closes/Resolves #N) done. A PR
// referencing no issue is a normal outcome (not every PR closes a backlog
// item), not an error.
func closeReferencedIssues(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, body, pullNumber string) (closed []string, errs []error) {
	for _, issueID := range closingIssueNumbers(body) {
		if _, err := provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
			Repository: repo, ID: issueID, Status: providers.WorkItemStatusDone,
			Comment: fmt.Sprintf("Merged in pull request #%s.", pullNumber),
		}); err != nil {
			errs = append(errs, fmt.Errorf("close issue #%s: %w", issueID, err))
			continue
		}
		closed = append(closed, issueID)
	}
	return closed, errs
}
