package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const (
	threadResponsesOutput             = "threadResponses"
	resolveReviewThreadsResultFile    = "review-thread-resolution.json"
	errorCodeThreadResponsesInvalid   = "thread_responses_invalid"
	unresolvedReviewThreadCountOutput = "unresolvedThreadCount"
)

const resolveReviewThreadsHelp = "Usage: goobers resolve-review-threads [path]\n\n" +
	"Validate the implementer's threadResponses against every gathered live review\n" +
	"thread, reply to each thread, resolve addressed threads after the reply is\n" +
	"visible, and re-query the published PR head. Exit codes: 0 = responses\n" +
	"applied and verified, 1 = business/provider error, 2 = usage/IO error.\n"

type reviewThreadDisposition struct {
	ThreadID    string `json:"threadId"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

type gatheredReviewThread struct {
	ID        string
	CommentID int64
}

func runResolveReviewThreads(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("resolve-review-threads", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "resolve-review-threads")
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
	brief, rawResponses, publishedHead, published, err := readReviewThreadResolutionInputs(root, runID)
	if err != nil {
		pf(stderr, "error: read review-thread resolution inputs: %v\n", err)
		return 1
	}
	if !published {
		pf(stderr, "error: remediated branch was not published; refusing to reply to review threads\n")
		return 1
	}
	threads, err := gatheredLiveReviewThreads(brief.GatherReviewThreads)
	if err != nil {
		return failThreadResponseValidation(err, stderr)
	}
	responses, err := validateThreadResponses(threads, rawResponses)
	if err != nil {
		return failThreadResponseValidation(err, stderr)
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
	provider, err := remediationStageProvider(root, repo, token, false)
	if err != nil {
		pf(stderr, "error: construct remediation provider: %v\n", err)
		return 1
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	mutator, ok := provider.(providers.PullRequestReviewThreadMutator)
	if !ok {
		pf(stderr, "error: provider %q cannot reply to or resolve review threads\n", repo.Provider)
		return 1
	}
	current, err := provider.GetPullRequest(ctx, repo, brief.SelectedNumber)
	if err != nil {
		return failProviderStage(stderr, "read published pull request head", err, resolveReviewThreadsResultFile)
	}
	if current.HeadSHA == "" {
		pf(stderr, "error: published PR #%s has no head SHA\n", brief.SelectedNumber)
		return 1
	}
	if current.HeadSHA != publishedHead {
		pf(stderr, "error: PR #%s head moved from published SHA %s to %s before review threads could be reconciled\n",
			brief.SelectedNumber, publishedHead, current.HeadSHA)
		return 1
	}

	for _, response := range responses {
		thread := threads[response.ThreadID]
		body := renderReviewThreadReply(runID, publishedHead, response)
		snapshot, err := provider.ListPullRequestReviewThreads(ctx, repo, brief.SelectedNumber)
		if err != nil {
			return failProviderStage(stderr, "read review threads before reply", err, resolveReviewThreadsResultFile)
		}
		if !reviewThreadHasReply(snapshot, response.ThreadID, body) {
			if _, err := mutator.ReplyPullRequestReviewThread(ctx, providers.PullRequestReviewThreadReply{
				Repository: repo, PullID: brief.SelectedNumber, CommentID: thread.CommentID, Body: body,
			}); err != nil {
				return failProviderStage(stderr, fmt.Sprintf("reply to review thread %s", response.ThreadID), err, resolveReviewThreadsResultFile)
			}
			snapshot, err = provider.ListPullRequestReviewThreads(ctx, repo, brief.SelectedNumber)
			if err != nil {
				return failProviderStage(stderr, "verify review-thread reply", err, resolveReviewThreadsResultFile)
			}
			if !reviewThreadHasReply(snapshot, response.ThreadID, body) {
				pf(stderr, "error: reply to review thread %s is not visible after publication\n", response.ThreadID)
				return 1
			}
		}
		if response.Disposition != "addressed" {
			continue
		}
		if reviewThreadResolved(snapshot, response.ThreadID) {
			continue
		}
		if err := mutator.ResolvePullRequestReviewThread(ctx, repo, response.ThreadID); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("resolve review thread %s", response.ThreadID), err, resolveReviewThreadsResultFile)
		}
		snapshot, err = provider.ListPullRequestReviewThreads(ctx, repo, brief.SelectedNumber)
		if err != nil {
			return failProviderStage(stderr, "verify review-thread resolution", err, resolveReviewThreadsResultFile)
		}
		if !reviewThreadResolved(snapshot, response.ThreadID) {
			pf(stderr, "error: review thread %s remains unresolved after resolution\n", response.ThreadID)
			return 1
		}
	}

	final, err := provider.ListPullRequestReviewThreads(ctx, repo, brief.SelectedNumber)
	if err != nil {
		return failProviderStage(stderr, "re-query unresolved review threads", err, resolveReviewThreadsResultFile)
	}
	verifiedHead, err := provider.GetPullRequest(ctx, repo, brief.SelectedNumber)
	if err != nil {
		return failProviderStage(stderr, "verify published pull request head", err, resolveReviewThreadsResultFile)
	}
	if verifiedHead.HeadSHA != publishedHead {
		pf(stderr, "error: PR #%s head moved from published SHA %s to %s while review threads were reconciled\n",
			brief.SelectedNumber, publishedHead, verifiedHead.HeadSHA)
		return 1
	}
	unresolved := countLiveUnresolvedReviewThreads(final)
	if err := writeProviderStageResult(providerInput("resultFile", resolveReviewThreadsResultFile), map[string]interface{}{
		"selectedNumber":                  brief.SelectedNumber,
		"publishedHeadSha":                publishedHead,
		unresolvedReviewThreadCountOutput: strconv.Itoa(unresolved),
	}); err != nil {
		pf(stderr, "error: write review-thread resolution result: %v\n", err)
		return 2
	}
	pf(stdout, "PR #%s: replied to %d review thread(s); %d unresolved live thread(s) remain\n", brief.SelectedNumber, len(responses), unresolved)
	return 0
}

func readReviewThreadResolutionInputs(root, runID string) (apiv1.RemediationBrief, string, string, bool, error) {
	brief, err := readLatestRemediationBrief(root, runID)
	if err != nil {
		return apiv1.RemediationBrief{}, "", "", false, err
	}
	runDir, err := runDirFor(layoutFor(root), runID)
	if err != nil {
		return apiv1.RemediationBrief{}, "", "", false, err
	}
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		return apiv1.RemediationBrief{}, "", "", false, err
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.RemediationBrief{}, "", "", false, err
	}
	var raw string
	var implementFound, pushFound bool
	var publishedValue, publishedHead string
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "implement" {
			implementFound = true
			raw, _ = event.Outputs[threadResponsesOutput].(string)
		}
		if event.Type == journal.EventStageFinished && event.Stage == "push-remediated" {
			pushFound = true
			publishedValue, _ = event.Outputs[pushRemediatedPublishedOutput].(string)
			publishedHead, _ = event.Outputs[pushRemediatedLocalHeadOutput].(string)
		}
	}
	if !implementFound {
		return apiv1.RemediationBrief{}, "", "", false, fmt.Errorf("no implement stage result found")
	}
	if !pushFound || (publishedValue != "true" && publishedValue != "false") {
		return apiv1.RemediationBrief{}, "", "", false, fmt.Errorf("push-remediated produced no valid published output")
	}
	if publishedValue == "true" && strings.TrimSpace(publishedHead) == "" {
		return apiv1.RemediationBrief{}, "", "", false, fmt.Errorf("push-remediated produced no published local head SHA")
	}
	return brief, raw, publishedHead, publishedValue == "true", nil
}

func gatheredLiveReviewThreads(section *apiv1.RemediationReviewThreads) (map[string]gatheredReviewThread, error) {
	if section == nil {
		return nil, fmt.Errorf("remediation brief has no gathered review threads")
	}
	threads := make(map[string]gatheredReviewThread)
	for _, comment := range section.InlineComments {
		if comment.IsResolved || comment.IsOutdated {
			continue
		}
		if comment.ID < 1 || strings.TrimSpace(comment.ThreadID) == "" {
			return nil, fmt.Errorf("gathered live review comment has no stable comment/thread id")
		}
		thread := threads[comment.ThreadID]
		thread.ID = comment.ThreadID
		if thread.CommentID == 0 || comment.InReplyTo == 0 {
			thread.CommentID = comment.ID
		}
		threads[comment.ThreadID] = thread
	}
	return threads, nil
}

func validateThreadResponses(threads map[string]gatheredReviewThread, raw string) ([]reviewThreadDisposition, error) {
	if strings.TrimSpace(raw) == "" {
		if len(threads) == 0 {
			return []reviewThreadDisposition{}, nil
		}
		return nil, fmt.Errorf("latest implement result omitted %s for %d live thread(s)", threadResponsesOutput, len(threads))
	}
	var responses []reviewThreadDisposition
	if err := json.Unmarshal([]byte(raw), &responses); err != nil {
		return nil, fmt.Errorf("decode %s JSON array: %w", threadResponsesOutput, err)
	}
	seen := make(map[string]bool, len(responses))
	for i := range responses {
		response := &responses[i]
		response.ThreadID = strings.TrimSpace(response.ThreadID)
		response.Disposition = strings.ToLower(strings.TrimSpace(response.Disposition))
		response.Detail = strings.TrimSpace(response.Detail)
		if _, ok := threads[response.ThreadID]; !ok {
			return nil, fmt.Errorf("response names unknown or non-live review thread %q", response.ThreadID)
		}
		if seen[response.ThreadID] {
			return nil, fmt.Errorf("review thread %q is accounted for more than once", response.ThreadID)
		}
		seen[response.ThreadID] = true
		if response.Disposition != "addressed" && response.Disposition != "obsolete" && response.Disposition != "blocked" {
			return nil, fmt.Errorf("review thread %q disposition is %q, want addressed, obsolete, or blocked", response.ThreadID, response.Disposition)
		}
		if response.Detail == "" {
			return nil, fmt.Errorf("review thread %q has no explanatory detail", response.ThreadID)
		}
	}
	for id := range threads {
		if !seen[id] {
			return nil, fmt.Errorf("%s has no response for review thread %q", threadResponsesOutput, id)
		}
	}
	return responses, nil
}

func failThreadResponseValidation(validationErr error, stderr io.Writer) int {
	message := fmt.Sprintf("validate %s: %v", threadResponsesOutput, validationErr)
	pf(stderr, "error: %s\n", message)
	if err := writeProviderStageResult(providerInput("resultFile", resolveReviewThreadsResultFile), map[string]interface{}{
		executor.OutputErrorCode:      errorCodeThreadResponsesInvalid,
		executor.OutputErrorMessage:   message,
		executor.OutputErrorRetryable: false,
	}); err != nil {
		pf(stderr, "warning: write thread-response validation result: %v\n", err)
	}
	return 1
}

func renderReviewThreadReply(runID, headSHA string, response reviewThreadDisposition) string {
	marker := fmt.Sprintf("<!-- goobers:review-thread-response:%s:%s -->", runID, response.ThreadID)
	switch response.Disposition {
	case "addressed":
		return fmt.Sprintf("Addressed in `%s`.\n\n%s\n\n%s", headSHA, response.Detail, marker)
	case "obsolete":
		return fmt.Sprintf("Leaving this thread unresolved because the finding is obsolete: %s\n\n%s", response.Detail, marker)
	default:
		return fmt.Sprintf("Leaving this thread unresolved because remediation is blocked: %s\n\n%s", response.Detail, marker)
	}
}

func reviewThreadHasReply(snapshot providers.PullRequestReviewThreads, threadID, body string) bool {
	for _, comment := range snapshot.InlineComments {
		if comment.ThreadID == threadID && comment.Body == body {
			return true
		}
	}
	return false
}

func reviewThreadResolved(snapshot providers.PullRequestReviewThreads, threadID string) bool {
	for _, comment := range snapshot.InlineComments {
		if comment.ThreadID == threadID {
			return comment.IsResolved
		}
	}
	return false
}

func countLiveUnresolvedReviewThreads(snapshot providers.PullRequestReviewThreads) int {
	ids := make(map[string]bool)
	for _, comment := range snapshot.InlineComments {
		if !comment.IsResolved && !comment.IsOutdated && comment.ThreadID != "" {
			ids[comment.ThreadID] = true
		}
	}
	return len(ids)
}
