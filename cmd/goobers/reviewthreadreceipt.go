package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// Review-thread publication receipts (#6131).
//
// resolve-review-threads' result file IS its receipt
// (goobers.dev/review-thread-publication/v1). It is written atomically before
// the first mutation and again after every verified reply and resolution, so
// whatever stops the stage — a provider error, stale input, a moved head, a
// killed process — leaves the newest durable account of exactly which
// mutations completed. The executor journals the result file as the stage's
// "<stage>/result" artifact on every exit, success or not, and that journal
// record is what survives a retry, a daemon restart and a crash-resume: the
// stage's scratch workspace does not.
//
// On the next attempt the stage loads the newest receipt its own run journal
// recorded for the same pull request, published head and feedback snapshot,
// and reconciles it against a fresh provider read before any mutation:
//
//   - provider state is authoritative. A receipt entry is never trusted on its
//     own; a verified entry the provider still shows is receipt_confirmed, and
//     a mutation the provider shows (this run's reply marker, the resolution)
//     that no receipt recorded — an attempt interrupted between mutation and
//     receipt write — is provider_adopted. Neither is published again.
//   - a verified entry the provider no longer shows (this run's reply deleted,
//     or a thread it resolved reopened) is someone else's mutation. It is not
//     silently redone: publication stops as stale input
//     (changed_thread_state), with the receipt as evidence, and the workflow
//     re-gathers.
//   - a receipt for a different head or feedback snapshot belongs to an
//     earlier pass and is not resumed.
//
// No restoration is attempted (restoration: unsupported): published replies
// are human-visible and are never deleted on a generic failure.

const (
	// defaultResolveReviewThreadsStage names the stage whose result artifacts
	// carry receipts when the runner did not say which task this is.
	defaultResolveReviewThreadsStage = "resolve-review-threads"
	// errorCodeReviewThreadUnverified is a mutation the provider accepted but
	// a read-after-write did not show. Non-retryable: the receipt records it
	// and a blind retry could not tell the stage anything new.
	errorCodeReviewThreadUnverified = "review_thread_mutation_unverified"
)

// reviewThreadReceiptStage is the task name this stage's result artifacts are
// journaled under: the runner's GOOBERS_TASK, which a workflow may name
// something other than the command.
func reviewThreadReceiptStage() string {
	if task := strings.TrimSpace(os.Getenv(executor.TaskEnvVar)); task != "" {
		return task
	}
	return defaultResolveReviewThreadsStage
}

// newReviewThreadReceipt is the receipt a fresh attempt starts from: every
// response pending, resolution applicable only to addressed threads.
func newReviewThreadReceipt(pullID, publishedHead string, snapshot *apiv1.PRFeedbackSnapshot, responses []reviewThreadDisposition) apiv1.ReviewThreadPublication {
	digests := map[string]string{}
	receipt := apiv1.ReviewThreadPublication{
		Schema:           apiv1.ReviewThreadPublicationVersion,
		Integrity:        apiv1.IntegrityUnapproved,
		PullRequest:      pullID,
		SelectedNumber:   pullID,
		PublishedHeadSHA: publishedHead,
		Status:           apiv1.ReviewThreadPublicationInProgress,
		Restoration:      apiv1.ReviewThreadRestorationUnsupported,
		Threads:          make([]apiv1.ReviewThreadReceipt, 0, len(responses)),
	}
	if snapshot != nil {
		receipt.FeedbackSnapshotDigest = snapshot.SnapshotDigest
		for _, thread := range snapshot.ReviewThreads {
			digests[thread.ThreadID] = thread.ContentDigest
		}
	}
	for _, response := range responses {
		resolution := apiv1.ReviewThreadMutationNotApplicable
		if response.Disposition == "addressed" {
			resolution = apiv1.ReviewThreadMutationPending
		}
		receipt.Threads = append(receipt.Threads, apiv1.ReviewThreadReceipt{
			ThreadID:        response.ThreadID,
			Disposition:     response.Disposition,
			ContentDigest:   digests[response.ThreadID],
			ReplyState:      apiv1.ReviewThreadMutationPending,
			ResolutionState: resolution,
		})
	}
	return receipt
}

// loadPriorReviewThreadReceipt returns the newest receipt this run's journal
// recorded for stage that describes the same transaction: the same pull
// request, published head and feedback snapshot. Anything else is an earlier
// pass's history and is not resumed.
func loadPriorReviewThreadReceipt(root, runID, stage string, current apiv1.ReviewThreadPublication) (*apiv1.ReviewThreadPublication, error) {
	rd, err := stageRunJournal(root, runID)
	if err != nil {
		return nil, err
	}
	events, err := rd.Events()
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil ||
			stageArtifactName(runID, event.Name) != stage+"/result" {
			continue
		}
		data, err := rd.ArtifactBytes(*event.Ref)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", event.Name, err)
		}
		var prior apiv1.ReviewThreadPublication
		if json.Unmarshal(data, &prior) != nil || prior.Schema != apiv1.ReviewThreadPublicationVersion {
			// A result from before receipts existed, or a typed failure
			// written before publication began: no transaction history.
			continue
		}
		if prior.PullRequest == current.PullRequest &&
			strings.EqualFold(prior.PublishedHeadSHA, current.PublishedHeadSHA) &&
			prior.FeedbackSnapshotDigest == current.FeedbackSnapshotDigest {
			return &prior, nil
		}
	}
	return nil, nil
}

// receiptThread returns the receipt entry for threadID.
func (p *reviewThreadPublication) receiptThread(threadID string) *apiv1.ReviewThreadReceipt {
	for i := range p.receipt.Threads {
		if p.receipt.Threads[i].ThreadID == threadID {
			return &p.receipt.Threads[i]
		}
	}
	return nil
}

// reconcileReceipt folds the prior attempt's receipt and the live listing
// into this attempt's receipt before any mutation. It returns stale reasons
// for every verified mutation the provider no longer shows.
func (p *reviewThreadPublication) reconcileReceipt(listing providers.PullRequestReviewThreads) []feedbackStaleReason {
	prior := map[string]apiv1.ReviewThreadReceipt{}
	if p.prior != nil {
		p.receipt.ResumedFromReceipt = true
		for _, entry := range p.prior.Threads {
			prior[entry.ThreadID] = entry
		}
	}
	var diverged []feedbackStaleReason
	for i := range p.receipt.Threads {
		entry := &p.receipt.Threads[i]
		before, recorded := prior[entry.ThreadID]
		if reason, ok := reconcileReply(entry, before, recorded, listing, p.runID); !ok {
			diverged = append(diverged, reason)
			continue
		}
		if entry.ResolutionState == apiv1.ReviewThreadMutationNotApplicable {
			continue
		}
		if reason, ok := reconcileResolution(entry, before, recorded, listing); !ok {
			diverged = append(diverged, reason)
		}
	}
	return diverged
}

func reconcileReply(entry *apiv1.ReviewThreadReceipt, before apiv1.ReviewThreadReceipt, recorded bool, listing providers.PullRequestReviewThreads, runID string) (feedbackStaleReason, bool) {
	wasVerified := recorded && before.ReplyState == apiv1.ReviewThreadMutationVerified
	if replyID, ok := reviewThreadReplyID(listing, runID, entry.ThreadID); ok {
		entry.ReplyState = apiv1.ReviewThreadMutationVerified
		entry.ProviderReplyID = replyID
		entry.Recovery = apiv1.ReviewThreadRecoveryProviderAdopted
		if wasVerified {
			entry.Recovery = apiv1.ReviewThreadRecoveryReceiptConfirmed
		}
		return feedbackStaleReason{}, true
	}
	if wasVerified {
		entry.ReplyState = apiv1.ReviewThreadMutationFailed
		entry.ProviderReplyID = before.ProviderReplyID
		entry.LastError = "the receipt records a verified reply the provider no longer shows; it is not published again"
		return feedbackStaleReason{Code: staleReasonThreadState, Kind: "reviewThread", ID: entry.ThreadID,
			Detail: "this run's verified reply is no longer on the thread"}, false
	}
	if recorded && before.LastError != "" {
		entry.LastError = "previous attempt: " + before.LastError
	}
	return feedbackStaleReason{}, true
}

func reconcileResolution(entry *apiv1.ReviewThreadReceipt, before apiv1.ReviewThreadReceipt, recorded bool, listing providers.PullRequestReviewThreads) (feedbackStaleReason, bool) {
	wasVerified := recorded && before.ResolutionState == apiv1.ReviewThreadMutationVerified
	if reviewThreadResolved(listing, entry.ThreadID) {
		entry.ResolutionState = apiv1.ReviewThreadMutationVerified
		// provider_adopted wins: one mutation no receipt recorded is the
		// fact worth surfacing for the thread.
		if !wasVerified {
			entry.Recovery = apiv1.ReviewThreadRecoveryProviderAdopted
		} else if entry.Recovery == "" {
			entry.Recovery = apiv1.ReviewThreadRecoveryReceiptConfirmed
		}
		return feedbackStaleReason{}, true
	}
	if wasVerified {
		entry.ResolutionState = apiv1.ReviewThreadMutationFailed
		entry.LastError = "the receipt records a verified resolution but the thread is unresolved again; it is not resolved again"
		return feedbackStaleReason{Code: staleReasonThreadState, Kind: "reviewThread", ID: entry.ThreadID,
			Detail: "reopened after this run resolved it"}, false
	}
	return feedbackStaleReason{}, true
}

// verifyReceiptComplete checks every intended mutation against the final
// listing: a complete receipt is proven by read-after-write, never by the
// receipt's own say-so.
func (p *reviewThreadPublication) verifyReceiptComplete(listing providers.PullRequestReviewThreads) error {
	for i := range p.receipt.Threads {
		entry := &p.receipt.Threads[i]
		if _, ok := reviewThreadReplyID(listing, p.runID, entry.ThreadID); !ok {
			entry.ReplyState = apiv1.ReviewThreadMutationFailed
			entry.LastError = "reply not visible in the final read"
			return fmt.Errorf("reply to review thread %s is not visible in the final read", entry.ThreadID)
		}
		if entry.ResolutionState != apiv1.ReviewThreadMutationNotApplicable && !reviewThreadResolved(listing, entry.ThreadID) {
			entry.ResolutionState = apiv1.ReviewThreadMutationFailed
			entry.LastError = "thread not resolved in the final read"
			return fmt.Errorf("review thread %s is not resolved in the final read", entry.ThreadID)
		}
	}
	return nil
}

// stoppedStatus is the receipt status for publication an error stopped:
// partial when any mutation was verified, failed otherwise.
func (p *reviewThreadPublication) stoppedStatus() string {
	for _, entry := range p.receipt.Threads {
		if entry.ReplyState == apiv1.ReviewThreadMutationVerified || entry.ResolutionState == apiv1.ReviewThreadMutationVerified {
			return apiv1.ReviewThreadPublicationPartial
		}
	}
	return apiv1.ReviewThreadPublicationFailed
}

// persistReceipt makes the receipt durable in the stage's result file. The
// write is a same-directory rename, so a process killed mid-write leaves the
// previous complete receipt, never a torn one.
func (p *reviewThreadPublication) persistReceipt() error {
	path := providerInput("resultFile", resolveReviewThreadsResultFile)
	data, err := json.Marshal(p.receipt)
	if err != nil {
		return fmt.Errorf("marshal review-thread publication receipt: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write review-thread publication receipt: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write review-thread publication receipt: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync review-thread publication receipt: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write review-thread publication receipt: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("write review-thread publication receipt: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish review-thread publication receipt: %w", err)
	}
	return nil
}

// checkpoint persists the receipt after a verified mutation. A receipt that
// cannot be made durable stops publication before the next mutation.
func (p *reviewThreadPublication) checkpoint() (int, bool) {
	if err := p.persistReceipt(); err != nil {
		pf(p.stderr, "error: %v\n", err)
		return 2, true
	}
	return 0, false
}

// fail stops publication on a provider error: the classified error and the
// partial/failed status are recorded in the receipt, so the failure result
// still says exactly which mutations completed.
func (p *reviewThreadPublication) fail(what string, err error) int {
	code, retryable, extra := classifyProviderError(err)
	reset, _ := extra["rateLimitReset"].(string)
	return p.failTyped(what, err, code, retryable, reset)
}

func (p *reviewThreadPublication) failTyped(what string, err error, code string, retryable bool, rateLimitReset string) int {
	pf(p.stderr, "error: %s: %v\n", what, err)
	p.receipt.Status = p.stoppedStatus()
	p.receipt.ErrorCode = code
	p.receipt.ErrorMessage = fmt.Sprintf("%s: %v", what, err)
	p.receipt.ErrorRetryable = retryable
	p.receipt.RateLimitReset = rateLimitReset
	if werr := p.persistReceipt(); werr != nil {
		pf(p.stderr, "warning: write review-thread publication receipt: %v\n", werr)
	}
	return 1
}
