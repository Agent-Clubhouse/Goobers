package main

import (
	"context"
	"flag"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

const prCommentWatchResultName = "comment-watch-result.json"

// mergeReadyLabel mirrors verdictLabel's pass label (applyverdict.go:64, a bare
// string literal there). It is one of the PR-side lifecycle labels the watcher
// must treat as "already routed / parked / opted out": a PR carrying it is
// landing, so remediation would race the merge.
const mergeReadyLabel = "goobers:merge-ready"

// prCommentWatchProvider is the narrow surface pr-comment-watch needs: list the
// open PRs (labels ride the summary), read a PR's issue-comment thread, learn
// the token's own login (the exclude-author — the token IS the bot), and add
// the routing label. All four exist on both the GitHub and Gitea providers;
// ADO has no AuthenticatedLogin, so it takes the default-error arm.
type prCommentWatchProvider interface {
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
	AuthenticatedLogin(context.Context) (string, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

// prCommentWatchDefaultExcludeLabels are the lifecycle labels that HARD-exclude a
// PR from the scan: a fresh human comment must neither re-route it nor clear the
// label, because — unlike the human-decision parks below — a comment is not the
// signal that changes any of these states. needs-remediation is the re-fire
// guard (already routed); merge-ready is landing in progress (self-heals on
// demotion, and remediation must not race the merge); blocked-on-sibling is an
// ordering park, not a content decision; no-merge-review is an explicit operator
// opt-out we must respect.
func prCommentWatchDefaultExcludeLabels() string {
	return strings.Join([]string{
		needsRemediationLabel,
		mergeReadyLabel,
		blockedOnSiblingLabel,
		noMergeReviewLabel,
	}, ",")
}

// prCommentWatchDefaultUnparkLabels are the "parked for a human" labels a fresh
// human comment SHOULD clear. The whole point of these parks is to wait for a
// human's judgement; once the human comments, they have weighed in, so the
// watcher strips the park label and routes the PR back through remediation
// (needs-remediation) in the same mutation — closing the loop that would
// otherwise leave the PR deaf to follow-up comments. Deliberately only the two
// human-decision parks: merge-escalated (remediation gave up and asked for a
// human) and needs-human (a reviewer explicitly parked it). Ordering parks and
// operator opt-outs stay in the hard-exclude set above and are never un-parked
// by a comment.
func prCommentWatchDefaultUnparkLabels() string {
	return strings.Join([]string{
		remediationEscalatedLabel,
		providers.LabelNeedsHuman,
	}, ",")
}

// carriedLabels returns the PR's own labels (original casing) whose lowercase is
// in set, preserving the exact strings the forge needs for a label remove.
func carriedLabels(prLabels []string, set map[string]bool) []string {
	var out []string
	for _, l := range prLabels {
		if set[strings.ToLower(l)] {
			out = append(out, l)
		}
	}
	return out
}

const prCommentWatchHelp = "Usage: goobers pr-comment-watch [path]\n\n" +
	"Scan open goober-authored PRs (head under the gaggle branch namespace)\n" +
	"and label any whose newest human comment is newer than the bot's own\n" +
	"newest comment with goobers:needs-remediation, so pr-remediation updates\n" +
	"that PR in place. A PR parked for a human (needs-human / merge-escalated)\n" +
	"is un-parked when a fresh human comment lands: the park label is cleared\n" +
	"and needs-remediation added in one mutation, since the human the PR was\n" +
	"parked for has now weighed in. The bot identity is the token's own login\n" +
	"(AuthenticatedLogin) — a dedicated bot account is required for signal;\n" +
	"with a shared human identity the stage never fires. Comments landing\n" +
	"mid-remediation after the brief snapshot can be masked by the bot's\n" +
	"response comment until the human comments again (accepted v1 limit).\n\n" +
	"Inputs: maxPullRequests (default 20), headPrefixes (default the branch\n" +
	"namespace), base (default the gaggle base branch), excludeLabels (labels\n" +
	"that hard-exclude a PR from the scan), unparkLabels (park labels a fresh\n" +
	"human comment clears while routing, default needs-human,merge-escalated),\n" +
	"excludeAuthors (extra bot logins to ignore, e.g. Gitea CI bots),\n" +
	"resultFile (default " + prCommentWatchResultName + ").\n" +
	"Exit codes: 0 = scanned (labeled zero or more), 1 = business error,\n" +
	"2 = usage/IO error.\n"

type prCommentWatchLabeled struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	CommentAuthor    string `json:"commentAuthor"`
	CommentURL       string `json:"commentUrl,omitempty"`
	CommentCreatedAt string `json:"commentCreatedAt,omitempty"`
	// Unparked is true when routing this PR also cleared a human-decision park
	// label (needs-human / merge-escalated); ClearedLabels lists what was stripped.
	Unparked      bool     `json:"unparked,omitempty"`
	ClearedLabels []string `json:"clearedLabels,omitempty"`
}

type prCommentWatchResult struct {
	Scanned   int                     `json:"scanned"`
	Labeled   int                     `json:"labeled"`
	Unparked  int                     `json:"unparked"`
	Errors    int                     `json:"errors"`
	Truncated bool                    `json:"truncated"`
	BotLogin  string                  `json:"botLogin"`
	PRs       []prCommentWatchLabeled `json:"prs,omitempty"`
	Integrity string                  `json:"integrity"` // apiintegrity.Unapproved
}

func runPRCommentWatch(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("pr-comment-watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "pr-comment-watch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	// Explicit per-kind dispatch (github | gitea | default-error): the single
	// mutation is an ordinary issues-API label add, so both forges take the
	// GitHubIssuesWrite credential seam. ADO has no AuthenticatedLogin, so it
	// cannot distinguish the bot's own comments and takes the error arm.
	var provider prCommentWatchProvider
	switch repo.Provider {
	case providers.ProviderGitea:
		token, terr := providerToken(capability.GitHubIssuesWrite)
		if terr != nil {
			pf(stderr, "error: %v\n", terr)
			return 1
		}
		giteaProvider, gerr := newGiteaProviderForStage(root, repo, token, providers.WithGiteaMutationRecorder(sidecarMutationRecorder{kind: "pr"}))
		if gerr != nil {
			pf(stderr, "error: %v\n", gerr)
			return 1
		}
		provider = giteaProvider
	case providers.ProviderGitHub:
		token, terr := providerToken(capability.GitHubIssuesWrite)
		if terr != nil {
			pf(stderr, "error: %v\n", terr)
			return 1
		}
		provider = newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))
	default:
		pf(stderr, "error: pr-comment-watch does not support repository provider %q\n", repo.Provider)
		return 1
	}

	rawMax := providerInput("maxPullRequests", "20")
	maxPRs, err := strconv.Atoi(rawMax)
	if err != nil || maxPRs < 1 {
		pf(stderr, "error: invalid maxPullRequests %q (want a positive integer)\n", rawMax)
		return 1
	}
	prefixes := splitLabelList(providerInput("headPrefixes", providerBranchNamespace()))
	exclude := toLowerSet(splitLabelList(providerInput("excludeLabels", prCommentWatchDefaultExcludeLabels())))
	unpark := toLowerSet(splitLabelList(providerInput("unparkLabels", prCommentWatchDefaultUnparkLabels())))
	excludeAuthors := toLowerSet(splitLabelList(providerInput("excludeAuthors", "")))
	resultFile := providerInput("resultFile", prCommentWatchResultName)

	ctx, cancel := providerCommandContext()
	defer cancel()

	// The token IS the bot, so its own login is the watermark author. Resolve it
	// once, before the PR loop: without it every human-vs-bot comparison is
	// meaningless, so a failure here is stage-fatal.
	botLogin, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve bot login", err, prCommentWatchResultName)
	}

	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     repo,
		Base:           providerInput("base", providerBaseBranch()),
		SkipCheckState: true, // we never gate on CI; skipping it saves 2 API calls/PR
	})
	if err != nil {
		return failProviderStage(stderr, "list open pull requests", err, prCommentWatchResultName)
	}

	result := prCommentWatchResult{BotLogin: botLogin, Integrity: "unapproved"}
	var lastErr error
	for _, pr := range prs {
		if pr.Draft || !hasAnyHeadPrefix(pr.Head, prefixes) || hasAnyLowerLabel(pr.Labels, exclude) {
			continue
		}
		if result.Scanned >= maxPRs {
			result.Truncated = true
			break
		}
		result.Scanned++

		comments, cerr := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
		if cerr != nil {
			// Warn and continue rather than conservatively labeling: a transient
			// failure must not spam-route the PR. The schedule retries next tick.
			pf(stderr, "warning: list comments for PR #%d: %v\n", pr.Number, cerr)
			result.Errors++
			lastErr = cerr
			continue
		}
		triggering, fresh := latestUnaddressedHumanComment(comments, botLogin, excludeAuthors)
		if !fresh {
			continue
		}

		// Route to remediation, and if the PR was parked for a human who has now
		// commented, clear that park in the same mutation so the lane can pick it
		// up (un-park). AddLabels leaves every other label untouched and re-adding
		// an existing label is a no-op; RemoveLabels only names park labels the PR
		// actually carries, so an un-parked PR is atomically re-routed. A normal
		// (non-parked) PR strips nothing and behaves exactly as before.
		cleared := carriedLabels(pr.Labels, unpark)
		if _, uerr := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   repo,
			ID:           strconv.Itoa(pr.Number), // PR number as issue id (applyverdict.go precedent)
			AddLabels:    []string{needsRemediationLabel},
			RemoveLabels: cleared,
		}); uerr != nil {
			pf(stderr, "warning: label PR #%d %s: %v\n", pr.Number, needsRemediationLabel, uerr)
			result.Errors++
			lastErr = uerr
			continue
		}
		entry := prCommentWatchLabeled{Number: pr.Number, URL: pr.URL, CommentAuthor: triggering.Author, CommentURL: triggering.URL}
		if triggering.CreatedAt != nil {
			entry.CommentCreatedAt = triggering.CreatedAt.UTC().Format(time.RFC3339)
		}
		entry.Unparked = len(cleared) > 0
		entry.ClearedLabels = cleared
		result.PRs = append(result.PRs, entry)
		result.Labeled++
		if entry.Unparked {
			result.Unparked++
			pf(stdout, "un-parked PR #%d (cleared %s, added %s): unaddressed comment by %s\n", pr.Number, strings.Join(cleared, ","), needsRemediationLabel, triggering.Author)
		} else {
			pf(stdout, "labeled PR #%d %s: unaddressed comment by %s\n", pr.Number, needsRemediationLabel, triggering.Author)
		}
	}
	// Every scanned PR erroring is a systemic failure (bad token, forge down),
	// not per-PR noise — surface it as a stage failure so the retry policy sees it.
	if result.Scanned > 0 && result.Errors == result.Scanned {
		return failProviderStage(stderr, "watch pull-request comments", lastErr, prCommentWatchResultName)
	}
	if err := writeProviderStagePayload(resultFile, result); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "scanned %d open PR(s): labeled %d (un-parked %d), errors %d\n", result.Scanned, result.Labeled, result.Unparked, result.Errors)
	return 0
}

// latestUnaddressedHumanComment mirrors calculateBacklogStaleness's author
// filter (backlogstaleness.go:118-131): the bot's own login and Bot-typed
// authors never count as human. Trigger iff the newest human comment is newer
// than the newest own-login comment (a missing own-login comment compares as
// zero time). GitHub populates AuthorType; Gitea does not, so excludeAuthors
// catches Gitea-side CI bots. Both providers return comments oldest-first, so
// list position breaks timestamp ties and orders comments with no timestamp.
func latestUnaddressedHumanComment(comments []providers.Comment, botLogin string, excludeAuthors map[string]bool) (providers.Comment, bool) {
	var human providers.Comment
	humanAt, ownAt := time.Time{}, time.Time{}
	humanIdx, ownIdx := -1, -1
	for i, c := range comments {
		at := time.Time{}
		if c.CreatedAt != nil {
			at = *c.CreatedAt
		}
		switch {
		case strings.EqualFold(c.Author, botLogin):
			if at.After(ownAt) || (at.Equal(ownAt) && i > ownIdx) {
				ownAt, ownIdx = at, i
			}
		case strings.EqualFold(c.AuthorType, "bot") || excludeAuthors[strings.ToLower(c.Author)]:
			// third-party automation — never a trigger
		default:
			if at.After(humanAt) || (at.Equal(humanAt) && i > humanIdx) {
				human, humanAt, humanIdx = c, at, i
			}
		}
	}
	if humanIdx == -1 {
		return providers.Comment{}, false
	}
	if humanAt.Equal(ownAt) {
		return human, humanIdx > ownIdx
	}
	return human, humanAt.After(ownAt)
}

// toLowerSet lowercases a slice into a membership set for case-insensitive
// label/author matching.
func toLowerSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[strings.ToLower(item)] = true
	}
	return set
}

// hasAnyLowerLabel reports whether labels intersects the lowercased want set.
func hasAnyLowerLabel(labels []string, want map[string]bool) bool {
	for _, l := range labels {
		if want[strings.ToLower(l)] {
			return true
		}
	}
	return false
}
