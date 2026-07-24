package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
)

const gatherReviewThreadsHelp = "Usage: goobers gather-review-threads [path]\n\n" +
	"Read this run's latest remediation brief and replace only its\n" +
	"gatherReviewThreads section with native review bodies and inline review\n" +
	"comments. File, line, side, diff-hunk, resolved, and outdated metadata\n" +
	"are preserved so the remediator can distinguish live feedback from stale\n" +
	"threads. [path] defaults to GOOBERS_INSTANCE_ROOT. Exit codes: 0 = review\n" +
	"context gathered (possibly empty), 1 = business/provider/journal error,\n" +
	"2 = usage/IO error.\n"

func runGatherReviewThreads(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gather-review-threads", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-review-threads")
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
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newCachedGitHubProvider(root, token)
	ctx, cancel := providerCommandContext()
	defer cancel()

	evidence, err := provider.ListPullRequestReviewThreads(ctx, repo, brief.SelectedNumber)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("list review threads on PR #%s", brief.SelectedNumber), err, remediationBriefResultFile)
	}
	reviews := make([]apiv1.RemediationNativeReview, 0, len(evidence.Reviews))
	for _, review := range evidence.Reviews {
		submittedAt := ""
		if review.SubmittedAt != nil {
			submittedAt = review.SubmittedAt.Format(time.RFC3339)
		}
		reviews = append(reviews, apiv1.RemediationNativeReview{
			Author:      review.Author,
			State:       review.State,
			Body:        review.Body,
			CommitSHA:   review.CommitSHA,
			SubmittedAt: submittedAt,
			URL:         review.URL,
		})
	}
	comments := make([]apiv1.RemediationInlineComment, 0, len(evidence.InlineComments))
	for _, comment := range evidence.InlineComments {
		createdAt := ""
		if comment.CreatedAt != nil {
			createdAt = comment.CreatedAt.Format(time.RFC3339)
		}
		comments = append(comments, apiv1.RemediationInlineComment{
			Author:       comment.Author,
			Body:         comment.Body,
			Path:         comment.Path,
			Line:         comment.Line,
			OriginalLine: comment.OriginalLine,
			Side:         comment.Side,
			DiffHunk:     comment.DiffHunk,
			InReplyTo:    comment.InReplyTo,
			IsResolved:   comment.IsResolved,
			IsOutdated:   comment.IsOutdated,
			CreatedAt:    createdAt,
			URL:          comment.URL,
		})
	}
	brief.GatherReviewThreads = &apiv1.RemediationReviewThreads{
		Reviews: reviews, InlineComments: comments,
	}

	resultFile := providerInput("resultFile", remediationBriefResultFile)
	data, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal remediation brief: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	pf(stdout, "gathered %d native review(s) and %d inline comment(s) for PR #%s\n", len(reviews), len(comments), brief.SelectedNumber)
	return 0
}

func readLatestRemediationBrief(root, runID string) (apiv1.RemediationBrief, error) {
	runDir, err := runDirFor(layoutFor(root), runID)
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}

	var latest apiv1.RemediationBrief
	found := false
	prefix := runID + ":gather-"
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil ||
			!strings.HasPrefix(event.Name, prefix) || !strings.HasSuffix(event.Name, "/result") {
			continue
		}
		data, readErr := rd.ArtifactBytes(*event.Ref)
		if readErr != nil {
			return apiv1.RemediationBrief{}, fmt.Errorf("read %s: %w", event.Name, readErr)
		}
		var header struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(data, &header) != nil || header.Schema == "" {
			continue
		}
		if !strings.HasPrefix(header.Schema, "goobers.dev/remediation-brief/") {
			continue
		}
		if header.Schema != apiv1.RemediationBriefVersion {
			return apiv1.RemediationBrief{}, fmt.Errorf(
				"%s schema is %q, want %q",
				event.Name, header.Schema, apiv1.RemediationBriefVersion,
			)
		}
		if err := json.Unmarshal(data, &latest); err != nil {
			return apiv1.RemediationBrief{}, fmt.Errorf("unmarshal %s: %w", event.Name, err)
		}
		found = true
	}
	if !found {
		return apiv1.RemediationBrief{}, fmt.Errorf("no remediation-brief artifact found")
	}
	if latest.SelectedNumber == "" {
		return apiv1.RemediationBrief{}, fmt.Errorf("latest remediation brief has no selectedNumber")
	}
	return latest, nil
}
