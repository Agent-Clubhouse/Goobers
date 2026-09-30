package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

const gatherReviewThreadsHelp = "Usage: goobers gather-review-threads [path]\n\n" +
	"Read this run's latest remediation brief and replace its gatherReviewThreads\n" +
	"section with native review bodies and inline review comments, refresh its\n" +
	"general PR comments from the same read, and pin all of it in an immutable\n" +
	"feedbackSnapshot (goobers.dev/pr-feedback-snapshot/v1) that later stages\n" +
	"compare against before acting on the feedback. File, line, side,\n" +
	"diff-hunk, resolved, and outdated metadata are preserved so the remediator\n" +
	"can distinguish live feedback from stale threads. A pull request that is\n" +
	"no longer at this run's selected head ends the run as a stale-selection\n" +
	"no-work. [path] defaults to GOOBERS_INSTANCE_ROOT. Exit codes: 0 = review\n" +
	"context gathered (possibly empty) or no-work, 1 = business/provider/journal\n" +
	"error, 2 = usage/IO error.\n"

func runGatherReviewThreads(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("gather-review-threads", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-review-threads")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	brief, err := readLatestRemediationBrief(root, runID)
	if err != nil {
		pf(stderr, "error: read remediation brief: %v\n", err)
		return 1
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, err := reviewThreadStageSurface[reviewThreadResolver](root, repo, true)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	src, err := newPRFeedbackSource(provider, repo.Provider)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := providerCommandContext()
	defer cancel()

	evidence, err := readFeedbackEvidence(ctx, src, repo, brief.SelectedNumber)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("gather feedback on PR #%s", brief.SelectedNumber), err, remediationBriefResultFile)
	}
	if code, stop := guardGatheredRevision(root, runID, repo, brief.SelectedNumber, evidence.pr, stdout, stderr); stop {
		return code
	}
	comments, err := briefGeneralComments(ctx, provider, repo, evidence.comments)
	if err != nil {
		return failProviderStage(stderr, "attribute PR comments", err, remediationBriefResultFile)
	}
	brief.GatherPRContext.Comments = comments
	brief.GatherReviewThreads = briefReviewThreads(evidence.threads)
	snapshot := buildFeedbackSnapshot(repo, brief.SelectedNumber, evidence, time.Now())
	brief.FeedbackSnapshot = &snapshot
	brief.Integrity = briefIntegrity(brief, evidence)
	if code := writeGatheredBrief(brief, stderr); code != 0 {
		return code
	}
	pf(stdout, "gathered %d native review(s), %d inline comment(s) and %d PR comment(s) for PR #%s at %s (feedback %s)\n",
		len(brief.GatherReviewThreads.Reviews), len(brief.GatherReviewThreads.InlineComments), len(comments),
		brief.SelectedNumber, snapshot.HeadSHA, snapshot.SnapshotDigest)
	return 0
}

// guardGatheredRevision pins the snapshot to this run's selected revision
// (#6128): feedback captured on a head the run never selected is feedback
// about someone else's code, so the run ends as a stale selection instead.
func guardGatheredRevision(root, runID string, repo providers.RepositoryRef, selected string, pr providers.PullRequestSummary, stdout, stderr io.Writer) (int, bool) {
	number, err := strconv.Atoi(selected)
	if err != nil {
		pf(stderr, "error: remediation brief has malformed selectedNumber %q\n", selected)
		return 1, true
	}
	expected, recorded, err := loadPRExpectedRevision(root, runID, repo, number)
	if err != nil {
		return failPRRevision(stderr, "load claimed pull request revision", err, remediationBriefResultFile), true
	}
	check, err := evaluatePRRevision(expected, recorded, pr)
	if err != nil {
		return failPRRevision(stderr, "verify gathered pull request revision", err, remediationBriefResultFile), true
	}
	if check.State != prRevisionTerminal && check.State != prRevisionStale {
		return 0, false
	}
	result := prRemediationLifecycleResult{
		SelectedNumber: selected, Revision: string(check.State), RevisionSource: expected.Source,
		ExpectedHeadSHA: check.Expected, LiveHeadSHA: check.Live,
	}
	return endClaimedPullRequest(root, result, check, stdout, stderr), true
}

func briefReviewThreads(evidence providers.PullRequestReviewThreads) *apiv1.RemediationReviewThreads {
	reviews := make([]apiv1.RemediationNativeReview, 0, len(evidence.Reviews))
	for _, review := range evidence.Reviews {
		submittedAt := ""
		if review.SubmittedAt != nil {
			submittedAt = review.SubmittedAt.Format(time.RFC3339)
		}
		reviews = append(reviews, apiv1.RemediationNativeReview{
			Author: review.Author, State: review.State, Body: review.Body, CommitSHA: review.CommitSHA,
			SubmittedAt: submittedAt, URL: review.URL, Integrity: review.Integrity,
		})
	}
	comments := make([]apiv1.RemediationInlineComment, 0, len(evidence.InlineComments))
	for _, comment := range evidence.InlineComments {
		comments = append(comments, briefInlineComment(comment))
	}
	return &apiv1.RemediationReviewThreads{Reviews: reviews, InlineComments: comments}
}

func briefInlineComment(comment providers.PullRequestInlineComment) apiv1.RemediationInlineComment {
	createdAt := ""
	if comment.CreatedAt != nil {
		createdAt = comment.CreatedAt.Format(time.RFC3339)
	}
	return apiv1.RemediationInlineComment{
		ID: comment.ID, ThreadID: comment.ThreadID, Author: comment.Author, Body: comment.Body,
		Path: comment.Path, Line: comment.Line, OriginalLine: comment.OriginalLine, Side: comment.Side,
		StartLine: comment.StartLine, OriginalStartLine: comment.OriginalStartLine, StartSide: comment.StartSide,
		DiffHunk: comment.DiffHunk, InReplyTo: comment.InReplyTo, IsResolved: comment.IsResolved,
		IsOutdated: comment.IsOutdated, CreatedAt: createdAt, URL: comment.URL, Integrity: comment.Integrity,
	}
}

// briefGeneralComments refreshes the brief's general PR comments from the
// read the snapshot pins, so the agent is handed exactly the general feedback
// the snapshot identifies — including any posted since gather-pr-context.
// On Azure DevOps authors are attributed by identity GUID exactly as
// gather-pr-context attributes them (ADO-N5).
func briefGeneralComments(ctx context.Context, provider reviewThreadResolver, repo providers.RepositoryRef, raw []providers.Comment) ([]apiv1.RemediationThreadComment, error) {
	if repo.Provider == providers.ProviderADO {
		if identity, ok := provider.(adoIdentityReader); ok {
			self, err := identity.AuthenticatedIdentity(ctx)
			if err != nil {
				return nil, err
			}
			raw = adoAttributeCommentsByID(raw, self)
		}
	}
	comments := make([]apiv1.RemediationThreadComment, 0, len(raw))
	for _, c := range raw {
		createdAt := ""
		if c.CreatedAt != nil {
			createdAt = c.CreatedAt.Format(time.RFC3339)
		}
		comments = append(comments, apiv1.RemediationThreadComment{
			Author: c.Author, Body: c.Body, CreatedAt: createdAt, URL: c.URL, Integrity: c.Integrity,
		})
	}
	return comments, nil
}

func briefIntegrity(brief apiv1.RemediationBrief, evidence feedbackEvidence) apiv1.Integrity {
	integrities := []apiv1.Integrity{brief.Integrity}
	for _, review := range evidence.threads.Reviews {
		integrities = append(integrities, review.Integrity)
	}
	for _, comment := range evidence.threads.InlineComments {
		integrities = append(integrities, comment.Integrity)
	}
	for _, comment := range evidence.comments {
		integrities = append(integrities, comment.Integrity)
	}
	return apiv1.WeakestIntegrity(integrities...)
}

func writeGatheredBrief(brief apiv1.RemediationBrief, stderr io.Writer) int {
	resultFile := providerInput("resultFile", remediationBriefResultFile)
	data, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal remediation brief: %v\n", err)
		return 1
	}
	if err := validateRemediationBriefJSON(data); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	return 0
}
