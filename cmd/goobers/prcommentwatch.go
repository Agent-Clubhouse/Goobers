package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

const prCommentWatchResultName = "comment-watch-result.json"

// mergeReadyLabel mirrors verdictLabel's pass label (applyverdict.go:64, a bare
// string literal there). It is one of the PR-side lifecycle labels the watcher
// must treat as "already routed / parked / opted out": a PR carrying it is
// landing, so remediation would race the merge.
const mergeReadyLabel = "goobers:merge-ready"

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
// (needs-remediation) — closing the loop that would otherwise leave the PR
// deaf to follow-up comments. Deliberately only the two human-decision parks:
// merge-escalated (remediation gave up and asked for a human) and needs-human
// (a reviewer explicitly parked it). Ordering parks and operator opt-outs stay
// in the hard-exclude set above and are never un-parked by a comment.
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
	"and label any whose newest human comment is newer than Goobers' own\n" +
	"newest comment with goobers:needs-remediation, so pr-remediation updates\n" +
	"that PR in place. A PR parked for a human (needs-human / merge-escalated)\n" +
	"is un-parked when a fresh human comment lands: the park label is cleared\n" +
	"and needs-remediation added, since the human the PR was parked for has\n" +
	"now weighed in. Works on GitHub, Gitea and Azure DevOps. On Azure DevOps\n" +
	"general comments and review-thread replies both count; deleted comments\n" +
	"and system threads (votes, pushes, policy status) never do.\n\n" +
	"A comment is Goobers' own when it carries a Goobers marker (the\n" +
	"attribution footer or a review-thread response marker) on a line of its\n" +
	"own. With identityMode=dedicated, an unmarked comment by the credential's\n" +
	"own identity (its login; its identity id on Azure DevOps) is Goobers' own\n" +
	"too. With identityMode=shared, for Goobers running as a person's own\n" +
	"identity, such a comment is that person's and counts as human.\n\n" +
	"Remediation responses carry a feedback-ack frontier: the newest\n" +
	"timestamp in the feedback snapshot the run assessed, to the second. Such\n" +
	"a response acknowledges only human comments within that second or\n" +
	"earlier, so feedback that lands after the snapshot stays fresh until a\n" +
	"run assesses it; comments sharing the frontier's second count as\n" +
	"assessed, and edits never re-route. Azure DevOps verdict thread comments\n" +
	"carry an empty frontier and acknowledge nothing. A Goobers comment\n" +
	"without a frontier keeps the legacy rule (it acknowledges every earlier\n" +
	"human comment); PRs whose newest human comment only such a comment\n" +
	"acknowledges are listed as unboundAcknowledged in the result.\n\n" +
	"Inputs: maxPullRequests (default 20), headPrefixes (default the branch\n" +
	"namespace), base (default the gaggle base branch), excludeLabels (labels\n" +
	"that hard-exclude a PR from the scan), unparkLabels (park labels a fresh\n" +
	"human comment clears while routing, default needs-human,merge-escalated),\n" +
	"excludeAuthors (extra automation identities to ignore: logins, or\n" +
	"identity ids on Azure DevOps), identityMode (dedicated or shared; default\n" +
	"shared on Azure DevOps, dedicated elsewhere), resultFile (default\n" +
	prCommentWatchResultName + ").\n" +
	"Credentials: github:issues:write on GitHub and Gitea; github:pr:write on\n" +
	"Azure DevOps, where the routing labels are pull-request labels.\n" +
	"Exit codes: 0 = scanned (labeled zero or more), 1 = business error,\n" +
	"2 = usage/IO error.\n"

type prCommentWatchLabeled struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	CommentAuthor    string `json:"commentAuthor"`
	CommentAuthorID  string `json:"commentAuthorId,omitempty"`
	CommentURL       string `json:"commentUrl,omitempty"`
	CommentCreatedAt string `json:"commentCreatedAt,omitempty"`
	// Unparked is true when routing this PR also cleared a human-decision park
	// label (needs-human / merge-escalated); ClearedLabels lists what was stripped.
	Unparked      bool     `json:"unparked,omitempty"`
	ClearedLabels []string `json:"clearedLabels,omitempty"`
}

type prCommentWatchResult struct {
	Scanned   int    `json:"scanned"`
	Labeled   int    `json:"labeled"`
	Unparked  int    `json:"unparked"`
	Errors    int    `json:"errors"`
	Truncated bool   `json:"truncated"`
	BotLogin  string `json:"botLogin"`
	// BotID is the credential identity's stable id where the provider has one
	// distinct from its display login (Azure DevOps).
	BotID        string                  `json:"botId,omitempty"`
	IdentityMode string                  `json:"identityMode"`
	PRs          []prCommentWatchLabeled `json:"prs,omitempty"`
	// UnboundAcknowledged lists scanned PRs whose newest human comment counts
	// as addressed only through a Goobers comment with no feedback-ack
	// frontier (legacy rule), i.e. no assessed snapshot proves it was seen.
	UnboundAcknowledged []int  `json:"unboundAcknowledged,omitempty"`
	Integrity           string `json:"integrity"` // apiintegrity.Unapproved
}

// prCommentWatchSettings are the stage inputs, parsed and defaulted.
type prCommentWatchSettings struct {
	maxPRs         int
	base           string
	prefixes       []string
	exclude        map[string]bool
	unpark         map[string]bool
	excludeAuthors map[string]bool
	identityMode   string
	resultFile     string
}

func readPRCommentWatchSettings(provider providers.ProviderKind) (prCommentWatchSettings, error) {
	rawMax := providerInput("maxPullRequests", "20")
	maxPRs, err := strconv.Atoi(rawMax)
	if err != nil || maxPRs < 1 {
		return prCommentWatchSettings{}, fmt.Errorf("invalid maxPullRequests %q (want a positive integer)", rawMax)
	}
	mode := strings.ToLower(strings.TrimSpace(providerInput("identityMode", defaultPRCommentWatchIdentityMode(provider))))
	if mode != prCommentWatchIdentityDedicated && mode != prCommentWatchIdentityShared {
		return prCommentWatchSettings{}, fmt.Errorf("invalid identityMode %q (want %s or %s)", mode, prCommentWatchIdentityDedicated, prCommentWatchIdentityShared)
	}
	return prCommentWatchSettings{
		maxPRs:         maxPRs,
		base:           providerInput("base", providerBaseBranch()),
		prefixes:       splitLabelList(providerInput("headPrefixes", providerBranchNamespace())),
		exclude:        toLowerSet(splitLabelList(providerInput("excludeLabels", prCommentWatchDefaultExcludeLabels()))),
		unpark:         toLowerSet(splitLabelList(providerInput("unparkLabels", prCommentWatchDefaultUnparkLabels()))),
		excludeAuthors: toLowerSet(splitLabelList(providerInput("excludeAuthors", ""))),
		identityMode:   mode,
		resultFile:     providerInput("resultFile", prCommentWatchResultName),
	}, nil
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
	forge, err := newPRCommentWatchForge(root, repo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	settings, err := readPRCommentWatchSettings(repo.Provider)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()

	// Resolve the credential's own identity once, before the PR loop: a
	// dedicated-identity comparison is meaningless without it, and on Azure
	// DevOps an identity that cannot be read makes the scan incomplete, so a
	// failure here is stage-fatal.
	self, err := forge.identity(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve bot identity", err, prCommentWatchResultName)
	}
	prs, err := forge.listPullRequests(ctx, settings.base)
	if err != nil {
		return failProviderStage(stderr, "list open pull requests", err, prCommentWatchResultName)
	}

	result := prCommentWatchResult{BotLogin: self.login, IdentityMode: settings.identityMode, Integrity: "unapproved"}
	if self.byID {
		result.BotID = self.key
	}
	classifier := newPRCommentClassifier(self, settings)
	lastErr := scanPRComments(ctx, forge, prs, settings, classifier, &result, stdout, stderr)
	// Every scanned PR erroring is a systemic failure (bad token, forge down),
	// not per-PR noise — surface it as a stage failure so the retry policy sees it.
	if result.Scanned > 0 && result.Errors == result.Scanned {
		return failProviderStage(stderr, "watch pull-request comments", lastErr, prCommentWatchResultName)
	}
	if err := writeProviderStagePayload(settings.resultFile, result); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "scanned %d open PR(s): labeled %d (un-parked %d), errors %d\n", result.Scanned, result.Labeled, result.Unparked, result.Errors)
	return 0
}

// scanPRComments visits the eligible PRs, up to settings.maxPRs, and routes
// each whose newest human comment is unaddressed. It records outcomes on
// result and returns the last per-PR error.
func scanPRComments(ctx context.Context, forge prCommentWatchForge, prs []providers.PullRequestSummary, settings prCommentWatchSettings, classifier prCommentClassifier, result *prCommentWatchResult, stdout, stderr io.Writer) error {
	var lastErr error
	for _, pr := range prs {
		if pr.Draft || !hasAnyHeadPrefix(pr.Head, settings.prefixes) || hasAnyLowerLabel(pr.Labels, settings.exclude) {
			continue
		}
		if result.Scanned >= settings.maxPRs {
			result.Truncated = true
			break
		}
		result.Scanned++
		if err := watchPRComments(ctx, forge, pr, settings, classifier, result, stdout); err != nil {
			// Warn and continue rather than conservatively labeling: a transient
			// failure must not spam-route the PR. The schedule retries next tick.
			pf(stderr, "warning: PR #%d: %v\n", pr.Number, err)
			result.Errors++
			lastErr = err
		}
	}
	return lastErr
}

// watchPRComments reads one PR's comments and, when its newest human comment
// is newer than Goobers' newest, routes it to remediation. If the PR was
// parked for a human who has now commented, the route also clears that park so
// the lane can pick it up (un-park). Only park labels the PR actually carries
// are cleared, so a normal (non-parked) PR strips nothing.
func watchPRComments(ctx context.Context, forge prCommentWatchForge, pr providers.PullRequestSummary, settings prCommentWatchSettings, classifier prCommentClassifier, result *prCommentWatchResult, stdout io.Writer) error {
	comments, err := forge.listComments(ctx, pr)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	triggering, fresh, unbound := latestUnaddressedHumanComment(comments, classifier)
	if unbound {
		result.UnboundAcknowledged = append(result.UnboundAcknowledged, pr.Number)
	}
	if !fresh {
		return nil
	}
	cleared := carriedLabels(pr.Labels, settings.unpark)
	if err := forge.route(ctx, pr, cleared); err != nil {
		return fmt.Errorf("label %s: %w", needsRemediationLabel, err)
	}
	entry := prCommentWatchLabeled{
		Number: pr.Number, URL: pr.URL, CommentAuthor: triggering.Author, CommentAuthorID: triggering.AuthorID,
		CommentURL: triggering.URL, Unparked: len(cleared) > 0, ClearedLabels: cleared,
	}
	if triggering.CreatedAt != nil {
		entry.CommentCreatedAt = triggering.CreatedAt.UTC().Format(time.RFC3339)
	}
	result.PRs = append(result.PRs, entry)
	result.Labeled++
	if entry.Unparked {
		result.Unparked++
		pf(stdout, "un-parked PR #%d (cleared %s, added %s): unaddressed comment by %s\n", pr.Number, strings.Join(cleared, ","), needsRemediationLabel, triggering.Author)
		return nil
	}
	pf(stdout, "labeled PR #%d %s: unaddressed comment by %s\n", pr.Number, needsRemediationLabel, triggering.Author)
	return nil
}

// prCommentMark is a position in the watermark order: timestamp, then list
// position. Providers return comments oldest-first (Azure DevOps by publish
// time, thread id, then comment id), so position breaks timestamp ties and
// orders comments with no timestamp.
type prCommentMark struct {
	at  time.Time
	idx int
}

func (m prCommentMark) after(o prCommentMark) bool {
	return m.at.After(o.at) || (m.at.Equal(o.at) && m.idx > o.idx)
}

// prCommentAckMark is how far one Goobers comment acknowledges human feedback,
// and whether a feedback-ack frontier bounds it (#6918). A bound comment
// covers every human comment within the frontier's second, wherever it sits
// in the list; a "none" or unreadable frontier covers nothing. An unbound
// comment keeps the legacy rule and covers everything before itself.
func prCommentAckMark(c providers.Comment, at time.Time, idx, n int) (prCommentMark, bool) {
	frontier, bound := parseFeedbackAck(c.Body)
	switch {
	case !bound:
		return prCommentMark{at: at, idx: idx}, false
	case frontier.IsZero():
		return prCommentMark{idx: -1}, true
	default:
		return prCommentMark{at: frontier.Add(time.Second - time.Nanosecond), idx: n}, true
	}
}

// latestUnaddressedHumanComment reports the newest human comment and whether
// it is fresh: past every Goobers acknowledgement (a PR with none compares as
// zero time). classifier decides each comment's origin; automation comments
// take part in neither watermark. unbound reports a newest human comment that
// only an unbound (legacy, frontier-less) Goobers comment acknowledges, so the
// result can say the acknowledgement rests on no assessed snapshot.
func latestUnaddressedHumanComment(comments []providers.Comment, classifier prCommentClassifier) (human providers.Comment, fresh, unbound bool) {
	humanMark, ack, boundAck := prCommentMark{idx: -1}, prCommentMark{idx: -1}, prCommentMark{idx: -1}
	for i, c := range comments {
		mark := prCommentMark{idx: i}
		if c.CreatedAt != nil {
			mark.at = *c.CreatedAt
		}
		switch classifier.origin(c) {
		case prCommentFromGoobers:
			covers, bound := prCommentAckMark(c, mark.at, i, len(comments))
			if covers.after(ack) {
				ack = covers
			}
			if bound && covers.after(boundAck) {
				boundAck = covers
			}
		case prCommentFromHuman:
			if mark.after(humanMark) {
				human, humanMark = c, mark
			}
		}
	}
	if humanMark.idx == -1 {
		return providers.Comment{}, false, false
	}
	fresh = humanMark.after(ack)
	return human, fresh, !fresh && humanMark.after(boundAck)
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
