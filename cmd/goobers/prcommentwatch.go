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
// string literal there): the PR passed review and its landing is in progress,
// possibly armed (an Azure DevOps auto-complete, a GitHub merge-queue entry)
// and waiting on a policy. Such a PR is still scanned (#6134): fresh human
// feedback disarms the landing first and only then routes the PR, so neither
// the armed landing nor remediation races the other.
const mergeReadyLabel = "goobers:merge-ready"

// prCommentWatchDefaultExcludeLabels are the lifecycle labels that HARD-exclude a
// PR from the scan: a fresh human comment must neither re-route it nor clear the
// label, because — unlike the human-decision parks below — a comment is not the
// signal that changes any of these states. needs-remediation is the re-fire
// guard (already routed); blocked-on-sibling is an ordering park, not a content
// decision; no-merge-review is an explicit operator opt-out we must respect.
// merge-ready is added to these unless blockUnansweredFeedback is "true"
// (#6134), which scans landing PRs so an armed landing does not complete over
// feedback that arrived after it was armed.
func prCommentWatchDefaultExcludeLabels() string {
	return strings.Join([]string{
		needsRemediationLabel,
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
	"now weighed in. With blockUnansweredFeedback \"true\" (declare policy\n" +
	"action disarm-landing) a PR that is landing (goobers:merge-ready) is\n" +
	"scanned too, by its open feedback: fresh human feedback first disarms\n" +
	"its armed landing — on Azure DevOps it cancels the auto-complete; on\n" +
	"GitHub queue-watch withdraws the queued PR once it is routed — then\n" +
	"removes merge-ready and routes it. Default false: landing PRs are\n" +
	"skipped, as before; set it false to merge over comments.\n" +
	"Works on GitHub, Gitea and Azure DevOps. On Azure DevOps\n" +
	"general comments and review-thread replies both count; deleted comments\n" +
	"and system threads (votes, pushes, policy status) never do.\n\n" +
	"A comment is Goobers' own when it carries a Goobers marker (the\n" +
	"attribution footer or a review-thread response marker) on a line of its\n" +
	"own. With identityMode=dedicated, an unmarked comment by the credential's\n" +
	"own identity (its login; its identity id on Azure DevOps) is Goobers' own\n" +
	"too. With identityMode=shared, for Goobers running as a person's own\n" +
	"identity, such a comment is that person's and counts as human.\n" +
	"rebase-pr and update-behind-pr classify comments by this same rule\n" +
	"before they clear needs-remediation; give them the same identityMode and\n" +
	"excludeAuthors.\n\n" +
	"With DSL 2.1, 3.0 or 3.1, shared mode permits three routing attempts\n" +
	"per PR head, recorded on the PR; declare label-issue and comment-on-issue.\n" +
	"Further attempts park it with needs-human and merge-escalated and a\n" +
	"visible note; feedback remains unaddressed. A new head resets the budget.\n" +
	"An explicit DSL 2.0 workflow keeps label-only routing without this budget\n" +
	"or its comment writes. Dedicated mode does not use the shared-mode cap.\n" +
	"Standalone calls without a DSL pin or stage context use the guarded default.\n" +
	"Stages must carry their pinned DSL version; a missing or unknown version\n" +
	"is refused before provider writes.\n\n" +
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
	// Disarmed is true when this stage cancelled the PR's armed landing (an
	// Azure DevOps auto-complete) before routing it. On GitHub a queued PR is
	// withdrawn by queue-watch once routed, so this stays false there.
	Disarmed bool `json:"disarmed,omitempty"`
}

type prCommentWatchResult struct {
	Scanned   int    `json:"scanned"`
	Parked    int    `json:"parked"`
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
	maxPRs     int
	base       string
	prefixes   []string
	exclude    map[string]bool
	unpark     map[string]bool
	identity   commentIdentitySettings
	resultFile string
	// disarmLandings scans landing (goobers:merge-ready) PRs and disarms
	// them on fresh human feedback (#6134). Opt-in, because it is a new
	// mutation the task must declare (policy action disarm-landing).
	disarmLandings bool
	routingBudget  bool
}

func readPRCommentWatchSettings(provider providers.ProviderKind) (prCommentWatchSettings, error) {
	routingBudget, err := commentWatchRoutingBudgetEnabled()
	if err != nil {
		return prCommentWatchSettings{}, err
	}
	rawMax := providerInput("maxPullRequests", "20")
	maxPRs, err := strconv.Atoi(rawMax)
	if err != nil || maxPRs < 1 {
		return prCommentWatchSettings{}, fmt.Errorf("invalid maxPullRequests %q (want a positive integer)", rawMax)
	}
	identity, err := readCommentIdentitySettings(provider)
	if err != nil {
		return prCommentWatchSettings{}, err
	}
	rawDisarm := providerInput("blockUnansweredFeedback", "false")
	disarm, err := strconv.ParseBool(strings.TrimSpace(rawDisarm))
	if err != nil {
		return prCommentWatchSettings{}, fmt.Errorf("invalid blockUnansweredFeedback %q (want true or false)", rawDisarm)
	}
	// An operator's own excludeLabels list is honoured as written; the
	// default skips landing PRs unless disarming them is enabled.
	defaultExclude := prCommentWatchDefaultExcludeLabels()
	if !disarm {
		defaultExclude += "," + mergeReadyLabel
	}
	exclude := toLowerSet(splitLabelList(providerInput("excludeLabels", defaultExclude)))
	return prCommentWatchSettings{
		maxPRs:         maxPRs,
		routingBudget:  routingBudget,
		base:           providerInput("base", providerBaseBranch()),
		prefixes:       splitLabelList(providerInput("headPrefixes", providerBranchNamespace())),
		exclude:        exclude,
		disarmLandings: disarm,
		unpark:         toLowerSet(splitLabelList(providerInput("unparkLabels", prCommentWatchDefaultUnparkLabels()))),
		identity:       identity,
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

	result := prCommentWatchResult{BotLogin: self.login, IdentityMode: settings.identity.mode, Integrity: "unapproved"}
	if self.byID {
		result.BotID = self.key
	}
	classifier := newCommentClassifier(self, settings.identity)
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
func scanPRComments(ctx context.Context, forge prCommentWatchForge, prs []providers.PullRequestSummary, settings prCommentWatchSettings, classifier commentClassifier, result *prCommentWatchResult, stdout, stderr io.Writer) error {
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
func watchPRComments(ctx context.Context, forge prCommentWatchForge, pr providers.PullRequestSummary, settings prCommentWatchSettings, classifier commentClassifier, result *prCommentWatchResult, stdout io.Writer) error {
	comments, err := forge.listComments(ctx, pr, classifier)
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
	landing := carriedLabels(pr.Labels, map[string]bool{mergeReadyLabel: true})
	disarmed := false
	if len(landing) > 0 {
		// #6134: disarm the landing BEFORE routing; a failed disarm leaves
		// the PR untouched and is retried next tick rather than routed while
		// the armed landing can still complete. A PR that landed first is
		// reported, never routed as withdrawn.
		var err error
		if disarmed, err = forge.disarm(ctx, pr); err != nil {
			return fmt.Errorf("disarm the armed landing: %w", err)
		}
	}
	exhausted := false
	if settings.routingBudget {
		var err error
		exhausted, err = reserveCommentWatchRoute(ctx, forge, pr, comments, classifier)
		if err != nil {
			return err
		}
	}
	if exhausted {
		remove := carriedLabels(pr.Labels, map[string]bool{mergeReadyLabel: true, needsRemediationLabel: true})
		if len(remove) > 0 || !hasAnyLabel(pr.Labels, []string{remediationEscalatedLabel}) || !hasAnyLabel(pr.Labels, []string{providers.LabelNeedsHuman}) {
			if err := forge.park(ctx, pr, remove); err != nil {
				return fmt.Errorf("park exhausted comment routing: %w", err)
			}
		}
		result.Parked++
		pf(stdout, "parked PR #%d: shared-identity routing budget exhausted for head %s; feedback requires human review\n", pr.Number, pr.HeadSHA)
		return nil
	}
	if err := forge.route(ctx, pr, append(append([]string(nil), cleared...), landing...)); err != nil {
		return fmt.Errorf("label %s: %w", needsRemediationLabel, err)
	}
	entry := prCommentWatchLabeled{
		Number: pr.Number, URL: pr.URL, CommentAuthor: triggering.Author, CommentAuthorID: triggering.AuthorID,
		CommentURL: triggering.URL, Unparked: len(cleared) > 0, ClearedLabels: cleared, Disarmed: disarmed,
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
