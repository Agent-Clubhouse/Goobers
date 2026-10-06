package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const (
	threadResponsesOutput             = "threadResponses"
	resolveReviewThreadsResultFile    = "review-thread-resolution.json"
	errorCodeThreadResponsesInvalid   = "thread_responses_invalid"
	errorCodePublishedHeadNotVisible  = "published_head_not_visible"
	unresolvedReviewThreadCountOutput = "unresolvedThreadCount"
)

const resolveReviewThreadsHelp = "Usage: goobers resolve-review-threads [path]\n\n" +
	"Validate the implementer's threadResponses against every gathered live review\n" +
	"thread, reply to each thread, resolve addressed threads after the reply is\n" +
	"visible, and re-query the published PR head. Before and during publication\n" +
	"the live feedback is compared with the run's recorded feedback snapshot:\n" +
	"new, changed or missing feedback, or a thread whose state someone else\n" +
	"changed, stops publication with a typed staleInput result the workflow\n" +
	"routes back to gather-review-threads; a head that moved off the published\n" +
	"SHA ends the run as no-work. The result file is a versioned publication\n" +
	"receipt (goobers.dev/review-thread-publication/v1), rewritten after every\n" +
	"verified reply and resolution; a retry reconciles the run's newest matching\n" +
	"receipt with re-read provider state and never publishes a mutation twice.\n" +
	"Exit codes: 0 = responses applied and verified, stale input reported, or\n" +
	"no-work; 1 = business/provider error; 2 = usage/IO error.\n"

type reviewThreadDisposition struct {
	ThreadID    string `json:"threadId"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

type gatheredReviewThread struct {
	ID        string
	CommentID int64
}

// reviewThreadPublication is one resolve-review-threads invocation: the
// validated responses, the provider it publishes through, what it compares
// live state against, and the durable receipt it keeps (#6131).
type reviewThreadPublication struct {
	root, runID   string
	repo          providers.RepositoryRef
	pullID        string
	publishedHead string
	// prePublishHead is the head this pass's push published over: the
	// snapshot's head, or the selected head for a run without a snapshot.
	prePublishHead string
	provider       reviewThreadResolver
	mutator        providers.PullRequestReviewThreadMutator
	source         prFeedbackSource
	snapshot       *apiv1.PRFeedbackSnapshot
	threads        map[string]gatheredReviewThread
	responses      []reviewThreadDisposition
	// receipt is this attempt's transaction record; prior is the newest
	// receipt an earlier attempt of the same transaction left in the journal.
	receipt apiv1.ReviewThreadPublication
	prior   *apiv1.ReviewThreadPublication
	// earlier holds receipts this run's earlier publication passes left at
	// the same head for other feedback snapshots, newest first; pass is this
	// pass's reply-marker key; reused maps a thread to the earlier pass
	// whose still-visible reply already answers this pass's response.
	earlier []apiv1.ReviewThreadPublication
	pass    string
	reused  map[string]string
	stdout  io.Writer
	stderr  io.Writer
}

func runResolveReviewThreads(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("resolve-review-threads", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "resolve-review-threads")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	p, code, ok := newReviewThreadPublication(root, stdout, stderr)
	if !ok {
		return code
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	return p.run(ctx)
}

func newReviewThreadPublication(root string, stdout, stderr io.Writer) (*reviewThreadPublication, int, bool) {
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return nil, 1, false
	}
	brief, rawResponses, publishedHead, published, err := readReviewThreadResolutionInputs(root, runID)
	if err != nil {
		pf(stderr, "error: read review-thread resolution inputs: %v\n", err)
		return nil, 1, false
	}
	if !published {
		pf(stderr, "error: remediated branch was not published; refusing to reply to review threads\n")
		return nil, 1, false
	}
	threads, err := gatheredLiveReviewThreads(brief.GatherReviewThreads)
	if err != nil {
		return nil, failThreadResponseValidation(err, stderr), false
	}
	responses, err := validateThreadResponses(threads, rawResponses)
	if err != nil {
		return nil, failThreadResponseValidation(err, stderr), false
	}
	p := &reviewThreadPublication{
		root: root, runID: runID, pullID: brief.SelectedNumber, publishedHead: publishedHead,
		snapshot: brief.FeedbackSnapshot, threads: threads, responses: responses, stdout: stdout, stderr: stderr,
		prePublishHead: strings.TrimSpace(brief.GatherPRContext.HeadSHA),
	}
	if brief.FeedbackSnapshot != nil {
		p.prePublishHead = brief.FeedbackSnapshot.HeadSHA
	}
	p.receipt = newReviewThreadReceipt(p.pullID, publishedHead, p.snapshot, responses)
	p.pass = reviewThreadPassKey(p.receipt.FeedbackSnapshotDigest)
	if p.prior, p.earlier, err = loadPriorReviewThreadReceipts(root, runID, reviewThreadReceiptStage(), p.receipt); err != nil {
		return nil, p.fail("read this run's review-thread publication receipt", err), false
	}
	if code, ok := p.connect(); !ok {
		return nil, code, false
	}
	return p, 0, true
}

func (p *reviewThreadPublication) connect() (int, bool) {
	repo, err := providerRepo(p.root)
	if err != nil {
		pf(p.stderr, "error: %v\n", err)
		return 1, false
	}
	provider, err := reviewThreadStageSurface[reviewThreadResolver](p.root, repo, false)
	if err != nil {
		pf(p.stderr, "error: construct remediation provider: %v\n", err)
		return 1, false
	}
	mutator, ok := provider.(providers.PullRequestReviewThreadMutator)
	if !ok {
		pf(p.stderr, "error: provider %q cannot reply to or resolve review threads\n", repo.Provider)
		return 1, false
	}
	p.repo, p.provider, p.mutator = repo, provider, mutator
	if p.snapshot != nil {
		source, err := newPRFeedbackSource(provider, repo.Provider)
		if err != nil {
			pf(p.stderr, "error: %v\n", err)
			return 1, false
		}
		p.source = source
	}
	return 0, true
}

func (p *reviewThreadPublication) run(ctx context.Context) int {
	if code, stop := p.checkBeforePublication(ctx); stop {
		return code
	}
	if code, stop := p.reconcile(ctx); stop {
		return code
	}
	for _, response := range p.responses {
		if code, stop := p.publish(ctx, response); stop {
			return code
		}
	}
	return p.finish(ctx)
}

// compareOptions names the thread-state changes this run makes itself.
func (p *reviewThreadPublication) compareOptions() feedbackCompareOptions {
	mayResolve := map[string]bool{}
	for _, response := range p.responses {
		if response.Disposition == "addressed" {
			mayResolve[response.ThreadID] = true
		}
	}
	// A resolution this run verified that reads unresolved again was
	// reopened by someone else: stale input, never a silent re-resolve.
	resolvedByRun := map[string]bool{}
	for _, entry := range p.receipt.Threads {
		if entry.ResolutionState == apiv1.ReviewThreadMutationVerified {
			resolvedByRun[entry.ThreadID] = true
		}
	}
	return feedbackCompareOptions{expectedHead: p.publishedHead, mayResolve: mayResolve, resolvedByRun: resolvedByRun}
}

// checkBeforePublication re-reads the head and, when the run recorded a
// feedback snapshot, every feedback source, before the first mutation.
func (p *reviewThreadPublication) checkBeforePublication(ctx context.Context) (int, bool) {
	if p.snapshot == nil {
		current, err := p.provider.GetPullRequest(ctx, p.repo, p.pullID)
		if err != nil {
			return p.fail("read published pull request head", err), true
		}
		return p.checkHead(current.HeadSHA, "before review threads could be reconciled")
	}
	check, err := checkLiveFeedback(ctx, p.source, p.repo, p.snapshot, p.compareOptions())
	if err != nil {
		return p.fail("re-read pull request feedback before publication", err), true
	}
	if check.stale() && check.reasons[0].Code == staleReasonHead {
		if code, stop := p.checkHead(check.evidence.pr.HeadSHA, "before review threads could be reconciled"); stop {
			return code, true
		}
	}
	if check.stale() {
		return p.reportStale(check.reasons, countLiveUnresolvedReviewThreads(check.evidence.threads)), true
	}
	return 0, false
}

// reconcile folds an earlier attempt's receipt and a fresh provider read into
// this attempt's receipt, then makes it durable before the first mutation.
// A verified mutation the provider no longer shows stops publication as
// stale input rather than being silently redone.
func (p *reviewThreadPublication) reconcile(ctx context.Context) (int, bool) {
	listing, code, stop := p.listThreads(ctx, "read review threads before publication")
	if stop {
		return code, true
	}
	if diverged := p.reconcileReceipt(listing); len(diverged) > 0 {
		return p.reportStale(diverged, countLiveUnresolvedReviewThreads(listing)), true
	}
	return p.checkpoint()
}

// checkHead enforces the published-head precondition. An empty head fails
// closed; a head that moved off this run's publication ends the run as
// no-work: whatever moved it, replies describing the published SHA would now
// describe a head the PR is no longer at.
func (p *reviewThreadPublication) checkHead(live, when string) (int, bool) {
	live = strings.TrimSpace(live)
	if live == "" {
		pf(p.stderr, "error: published PR #%s has no head SHA\n", p.pullID)
		return 1, true
	}
	if strings.EqualFold(live, p.publishedHead) {
		return 0, false
	}
	if p.prePublishHead != "" && strings.EqualFold(live, p.prePublishHead) {
		// The provider still reports the head this pass published over: its
		// view has not caught up with the push yet (Azure DevOps updates the
		// PR's source commit asynchronously). That is weather, not a moved
		// PR, so it takes the bounded infrastructure retry.
		return p.failTyped("read published pull request head",
			fmt.Errorf("PR #%s still reports pre-publication head %s instead of published %s", p.pullID, live, p.publishedHead),
			errorCodePublishedHeadNotVisible, true, ""), true
	}
	pf(p.stderr, "PR #%s head moved from published SHA %s to %s %s\n", p.pullID, p.publishedHead, live, when)
	return p.endStaleHead(live), true
}

func (p *reviewThreadPublication) endStaleHead(live string) int {
	if err := releasePRRemediationClaim(p.root); err != nil {
		return p.fail("release remediation PR claim", err)
	}
	reason := fmt.Sprintf("stale head: PR #%s moved from this run's published head %s to %s", p.pullID, p.publishedHead, live)
	pf(p.stdout, "no work: %s; no further review-thread replies published, claim released\n", reason)
	p.receipt.Status = apiv1.ReviewThreadPublicationStale
	p.receipt.StaleInput = staleReasonHead
	p.receipt.NoWork = true
	p.receipt.NoWorkReason = reason
	p.receipt.Outcome = staleReasonHead
	p.receipt.LiveHeadSHA = live
	if err := p.persistReceipt(); err != nil {
		pf(p.stderr, "error: %v\n", err)
		return 2
	}
	return 0
}

// reportStale records a typed stale-input result and stops publication. It
// exits 0 on purpose: the stage did its job — it refused to answer feedback
// that is no longer the feedback on the PR — and the workflow's gate routes
// the result back to gather-review-threads rather than treating it as a
// provider failure. The receipt records exactly which mutations had completed.
func (p *reviewThreadPublication) reportStale(reasons []feedbackStaleReason, unresolved int) int {
	pf(p.stdout, "PR #%s: feedback changed since it was gathered (%s); no further review-thread replies published\n",
		p.pullID, describeStaleReasons(reasons))
	p.receipt.Status = apiv1.ReviewThreadPublicationStale
	p.receipt.UnresolvedThreadCount = strconv.Itoa(unresolved)
	p.receipt.StaleInput = staleInputCode(reasons)
	p.receipt.StaleReasons = reasons
	if err := p.persistReceipt(); err != nil {
		pf(p.stderr, "error: %v\n", err)
		return 2
	}
	return 0
}

func describeStaleReasons(reasons []feedbackStaleReason) string {
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		part := reason.Code
		if reason.ID != "" {
			part += " " + reason.Kind + " " + reason.ID
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// publish replies to one thread and, for an addressed thread, resolves it,
// verifying each mutation by reading it back and checkpointing the receipt
// after each verified one. The thread listing it already re-reads around each
// mutation doubles as the per-mutation freshness check, and provider state —
// not the receipt — decides whether a mutation is still needed.
func (p *reviewThreadPublication) publish(ctx context.Context, response reviewThreadDisposition) (int, bool) {
	entry := p.receiptThread(response.ThreadID)
	listing, code, stop := p.listThreads(ctx, "read review threads before reply")
	if stop {
		return code, true
	}
	if replyID, ok := p.replyFor(listing, response.ThreadID); ok {
		if entry.ReplyState != apiv1.ReviewThreadMutationVerified {
			entry.ReplyState, entry.ProviderReplyID = apiv1.ReviewThreadMutationVerified, replyID
			entry.Recovery = apiv1.ReviewThreadRecoveryProviderAdopted
		}
	} else {
		if listing, code, stop = p.reply(ctx, response, entry); stop {
			return code, true
		}
	}
	if response.Disposition != "addressed" {
		return 0, false
	}
	if reviewThreadResolved(listing, response.ThreadID) {
		entry.ResolutionState = apiv1.ReviewThreadMutationVerified
		return p.checkpoint()
	}
	return p.resolve(ctx, entry)
}

// listThreads re-lists the PR's review threads and applies the per-mutation
// freshness check to the listing.
func (p *reviewThreadPublication) listThreads(ctx context.Context, what string) (providers.PullRequestReviewThreads, int, bool) {
	listing, err := p.provider.ListPullRequestReviewThreads(ctx, p.repo, p.pullID)
	if err != nil {
		return listing, p.fail(what, err), true
	}
	if reasons := checkThreadFeedback(p.snapshot, listing, p.compareOptions()); len(reasons) > 0 {
		return listing, p.reportStale(reasons, countLiveUnresolvedReviewThreads(listing)), true
	}
	return listing, 0, false
}

func (p *reviewThreadPublication) reply(ctx context.Context, response reviewThreadDisposition, entry *apiv1.ReviewThreadReceipt) (providers.PullRequestReviewThreads, int, bool) {
	thread := p.threads[response.ThreadID]
	body := renderReviewThreadReply(p.runID, p.pass, p.publishedHead, response)
	if _, err := p.mutator.ReplyPullRequestReviewThread(ctx, providers.PullRequestReviewThreadReply{
		Repository: p.repo, PullID: p.pullID, ThreadID: thread.ID, CommentID: thread.CommentID, Body: body,
	}); err != nil {
		// The provider may or may not have applied it; the next attempt's
		// read decides, by this run's marker, never by this error.
		entry.ReplyState, entry.LastError = apiv1.ReviewThreadMutationFailed, err.Error()
		return providers.PullRequestReviewThreads{}, p.fail(fmt.Sprintf("reply to review thread %s", response.ThreadID), err), true
	}
	listing, code, stop := p.listThreads(ctx, "verify review-thread reply")
	if stop {
		return listing, code, true
	}
	replyID, ok := reviewThreadReplyID(listing, p.runID, response.ThreadID, p.pass)
	if !ok {
		entry.ReplyState, entry.LastError = apiv1.ReviewThreadMutationFailed, "reply not visible after publication"
		return listing, p.failTyped("verify review-thread reply",
			fmt.Errorf("reply to review thread %s is not visible after publication", response.ThreadID),
			errorCodeReviewThreadUnverified, false, ""), true
	}
	entry.ReplyState, entry.ProviderReplyID, entry.LastError = apiv1.ReviewThreadMutationVerified, replyID, ""
	if code, stop := p.checkpoint(); stop {
		return listing, code, true
	}
	return listing, 0, false
}

func (p *reviewThreadPublication) resolve(ctx context.Context, entry *apiv1.ReviewThreadReceipt) (int, bool) {
	threadID := entry.ThreadID
	if err := p.mutator.ResolvePullRequestReviewThread(ctx, p.repo, threadID); err != nil {
		entry.ResolutionState, entry.LastError = apiv1.ReviewThreadMutationFailed, err.Error()
		return p.fail(fmt.Sprintf("resolve review thread %s", threadID), err), true
	}
	listing, code, stop := p.listThreads(ctx, "verify review-thread resolution")
	if stop {
		return code, true
	}
	if !reviewThreadResolved(listing, threadID) {
		entry.ResolutionState, entry.LastError = apiv1.ReviewThreadMutationFailed, "thread unresolved after resolution"
		return p.failTyped("verify review-thread resolution",
			fmt.Errorf("review thread %s remains unresolved after resolution", threadID),
			errorCodeReviewThreadUnverified, false, ""), true
	}
	entry.ResolutionState, entry.LastError = apiv1.ReviewThreadMutationVerified, ""
	return p.checkpoint()
}

// finish re-queries the threads and the head after the last mutation, proves
// every intended mutation from that final read, and writes the complete
// receipt.
func (p *reviewThreadPublication) finish(ctx context.Context) int {
	final, code, stop := p.listThreads(ctx, "re-query unresolved review threads")
	if stop {
		return code
	}
	unresolved := countLiveUnresolvedReviewThreads(final)
	p.receipt.UnresolvedThreadCount = strconv.Itoa(unresolved)
	if err := p.verifyReceiptComplete(final); err != nil {
		return p.failTyped("verify review-thread publication", err, errorCodeReviewThreadUnverified, false, "")
	}
	verifiedHead, err := p.provider.GetPullRequest(ctx, p.repo, p.pullID)
	if err != nil {
		return p.fail("verify published pull request head", err)
	}
	if code, stop := p.checkHead(verifiedHead.HeadSHA, "while review threads were reconciled"); stop {
		return code
	}
	p.receipt.Status = apiv1.ReviewThreadPublicationComplete
	if err := p.persistReceipt(); err != nil {
		pf(p.stderr, "error: %v\n", err)
		return 2
	}
	pf(p.stdout, "PR #%s: replied to %d review thread(s); %d unresolved live thread(s) remain\n", p.pullID, len(p.responses), unresolved)
	return 0
}

func readReviewThreadResolutionInputs(root, runID string) (apiv1.RemediationBrief, string, string, bool, error) {
	brief, err := readLatestRemediationBrief(root, runID)
	if err != nil {
		return apiv1.RemediationBrief{}, "", "", false, err
	}
	rd, err := stageRunJournal(root, runID)
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

// reviewThreadResponseMarker is the hidden line that identifies this run's
// reply to one review thread in one publication pass. The stage recognises
// its own reply by it, both before posting (idempotent re-runs) and after
// (the visibility check).
//
// pass names the feedback snapshot the reply answers (reviewThreadPassKey).
// A run can publish more than once at the same head — a no-change feedback
// repass after publication (#6126) runs resolve-review-threads again against
// a fresh snapshot — and a reply to the earlier snapshot must not pass for
// the later one's (#6131). A run without a snapshot uses the original
// run-and-thread marker.
func reviewThreadResponseMarker(runID, threadID, pass string) string {
	if pass == "" {
		return fmt.Sprintf("%s%s:%s -->", reviewThreadResponseMarkerPrefix, runID, threadID)
	}
	return fmt.Sprintf("%s%s:%s:%s -->", reviewThreadResponseMarkerPrefix, runID, threadID, pass)
}

// reviewThreadPassKey is the short, stable pass discriminator a reply marker
// carries: the leading hex of the feedback snapshot digest it answers.
func reviewThreadPassKey(snapshotDigest string) string {
	key := strings.TrimPrefix(strings.TrimSpace(snapshotDigest), "sha256:")
	if len(key) > 16 {
		key = key[:16]
	}
	return key
}

// reviewThreadResponseMarkerPrefix opens every reviewThreadResponseMarker.
// pr-comment-watch reads it to recognise a Goobers-written thread reply.
const reviewThreadResponseMarkerPrefix = "<!-- goobers:review-thread-response:"

func renderReviewThreadReply(runID, pass, headSHA string, response reviewThreadDisposition) string {
	marker := reviewThreadResponseMarker(runID, response.ThreadID, pass)
	switch response.Disposition {
	case "addressed":
		return fmt.Sprintf("Addressed in `%s`.\n\n%s\n\n%s", headSHA, response.Detail, marker)
	case "obsolete":
		return fmt.Sprintf("Leaving this thread unresolved because the finding is obsolete: %s\n\n%s", response.Detail, marker)
	default:
		return fmt.Sprintf("Leaving this thread unresolved because remediation is blocked: %s\n\n%s", response.Detail, marker)
	}
}

// reviewThreadReplyID returns the provider id of this run's reply on
// threadID for one publication pass, if the thread already carries one. It
// matches the response marker on a line of its own rather than the whole
// body: providers append run attribution to the text they post, so the body
// read back does not equal the body the stage rendered.
func reviewThreadReplyID(snapshot providers.PullRequestReviewThreads, runID, threadID, pass string) (string, bool) {
	marker := reviewThreadResponseMarker(runID, threadID, pass)
	for _, comment := range snapshot.InlineComments {
		if comment.ThreadID == threadID && bodyHasMarkerLine(comment.Body, marker) {
			return strconv.FormatInt(comment.ID, 10), true
		}
	}
	return "", false
}

func bodyHasMarkerLine(body, marker string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == marker {
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
