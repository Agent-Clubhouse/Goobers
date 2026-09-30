package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// prfeedbacksnapshot.go is the immutable pull-request feedback snapshot and
// its stale-input check (#6126).
//
// gather-review-threads captures the snapshot from the same provider reads it
// puts in the remediation brief, so the snapshot is exactly the feedback the
// agent was given. Every later stage that would act on that feedback —
// pr-claim --verify-feedback before the branch is published, and
// resolve-review-threads before and during its replies — re-reads the live
// feedback, canonicalizes it the same way, and compares. A difference is a
// typed stale-input result (never a provider failure): the workflow routes it
// back to gather-review-threads for a bounded repass, so the agent answers
// the feedback that is actually there.
//
// Canonicalization rules (documented and tested; the digest depends on them):
//
//   - Identity is the provider's stable id; order is by id (numeric where the
//     provider's ids are numeric), so retrieval order never matters. Ids are
//     unique, so no timestamp tie-break is ever needed.
//   - Bodies are compared by sha256 after normalizing CRLF to LF and trimming
//     surrounding whitespace; URLs, anchors, diff hunks and display names that
//     have a stable id beside them are excluded.
//   - Machine-authored content is excluded by an explicit rule: a body that
//     carries a Goobers hidden marker (a line-level "<!-- goobers" comment,
//     which every Goobers-published body and the provider attribution footer
//     carry), or a general comment the provider reports as authored by a bot
//     account. Nothing else is filtered, so a human edit is never invisible.
//   - Native review bodies count only when non-empty (an empty approval has no
//     feedback text; its inline comments are captured as threads). Review
//     state is excluded because a provider rewrites it on push (stale-review
//     dismissal) without any human acting.
//   - A thread's outdated flag is recorded but never compared: it is a
//     function of the head, which the snapshot pins separately.
//   - Collection completeness is part of the digest input; a read that fails
//     part-way yields no clean snapshot.

const (
	staleReasonHead        = "stale_head"
	staleReasonIncomplete  = "incomplete_collection"
	staleReasonNew         = "new_feedback"
	staleReasonChanged     = "changed_feedback"
	staleReasonMissing     = "missing_feedback"
	staleReasonThreadState = "changed_thread_state"

	// staleInputOutput is the scalar a workflow gate routes on: empty when
	// the live feedback matches the snapshot (or none was recorded), else the
	// highest-priority stale reason code.
	staleInputOutput             = "staleInput"
	staleReasonsOutput           = "staleReasons"
	feedbackSnapshotDigestOutput = "feedbackSnapshotDigest"
)

// staleReasonPriority orders reason codes for staleInput: the most
// fundamental mismatch names the result.
var staleReasonPriority = map[string]int{
	staleReasonHead:        0,
	staleReasonIncomplete:  1,
	staleReasonNew:         2,
	staleReasonChanged:     3,
	staleReasonMissing:     4,
	staleReasonThreadState: 5,
}

// feedbackStaleReason is one structured reason a live read differs from the
// recorded snapshot.
type feedbackStaleReason struct {
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// prFeedbackSource is the provider-neutral read surface a snapshot needs.
type prFeedbackSource interface {
	GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error)
	ListPullRequestReviewThreads(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestReviewThreads, error)
	ListGeneralComments(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error)
}

type issueCommentLister interface {
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
}

type pullRequestThreadCommentLister interface {
	ListPullRequestThreadComments(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error)
}

// stageFeedbackSource adapts a review-thread provider to prFeedbackSource.
// General PR comments are the PR conversation on GitHub and Gitea and the
// pull-request threads on Azure DevOps — never ADO's work-item comments,
// which ListComments would address by the PR's number (the wrong-object
// hazard).
type stageFeedbackSource struct {
	reviewThreadResolver
	general func(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error)
}

func (s stageFeedbackSource) ListGeneralComments(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error) {
	return s.general(ctx, repo, pullID)
}

func newPRFeedbackSource(provider reviewThreadResolver, kind providers.ProviderKind) (prFeedbackSource, error) {
	if kind == providers.ProviderADO {
		lister, ok := provider.(pullRequestThreadCommentLister)
		if !ok {
			return nil, fmt.Errorf("provider %q cannot list pull-request thread comments", kind)
		}
		return stageFeedbackSource{reviewThreadResolver: provider, general: lister.ListPullRequestThreadComments}, nil
	}
	lister, ok := provider.(issueCommentLister)
	if !ok {
		return nil, fmt.Errorf("provider %q cannot list pull-request comments", kind)
	}
	return stageFeedbackSource{reviewThreadResolver: provider, general: lister.ListComments}, nil
}

// feedbackEvidence is one live read of every feedback source plus the head.
type feedbackEvidence struct {
	pr       providers.PullRequestSummary
	threads  providers.PullRequestReviewThreads
	comments []providers.Comment
}

// readFeedbackEvidence reads the head, the review threads and the general
// comments. The head is read first and re-read last: a head that moved while
// the feedback was being read would pair feedback with the wrong revision, so
// the read reports it rather than returning a torn snapshot.
func readFeedbackEvidence(ctx context.Context, src prFeedbackSource, repo providers.RepositoryRef, pullID string) (feedbackEvidence, error) {
	var ev feedbackEvidence
	var err error
	if ev.pr, err = src.GetPullRequest(ctx, repo, pullID); err != nil {
		return ev, fmt.Errorf("read pull request #%s: %w", pullID, err)
	}
	if ev.threads, err = src.ListPullRequestReviewThreads(ctx, repo, pullID); err != nil {
		return ev, fmt.Errorf("list review threads on PR #%s: %w", pullID, err)
	}
	if ev.comments, err = src.ListGeneralComments(ctx, repo, pullID); err != nil {
		return ev, fmt.Errorf("list comments on PR #%s: %w", pullID, err)
	}
	after, err := src.GetPullRequest(ctx, repo, pullID)
	if err != nil {
		return ev, fmt.Errorf("re-read pull request #%s: %w", pullID, err)
	}
	before := ev.pr.HeadSHA
	ev.pr = after
	if !strings.EqualFold(strings.TrimSpace(after.HeadSHA), strings.TrimSpace(before)) {
		return ev, fmt.Errorf("%w: PR #%s head moved from %s to %s while its feedback was read",
			errFeedbackTorn, pullID, before, after.HeadSHA)
	}
	return ev, nil
}

// errFeedbackTorn marks a feedback read that raced a head move.
var errFeedbackTorn = errors.New("feedback read raced a head change")

// buildFeedbackSnapshot canonicalizes one complete evidence read.
func buildFeedbackSnapshot(repo providers.RepositoryRef, pullID string, ev feedbackEvidence, now time.Time) apiv1.PRFeedbackSnapshot {
	snapshot := apiv1.PRFeedbackSnapshot{
		Schema:          apiv1.PRFeedbackSnapshotVersion,
		Provider:        string(repo.Provider),
		Repository:      repo.CanonicalKey(),
		PullRequest:     pullID,
		HeadSHA:         strings.TrimSpace(ev.pr.HeadSHA),
		CapturedAt:      now.UTC().Format(time.RFC3339),
		Complete:        true,
		GeneralComments: canonicalGeneralComments(ev.comments),
		Reviews:         canonicalReviews(ev.threads.Reviews),
		ReviewThreads:   canonicalThreads(ev.threads.InlineComments),
	}
	snapshot.SnapshotDigest = feedbackSnapshotDigest(snapshot)
	return snapshot
}

// feedbackSnapshotDigest is sha256 over the canonical JSON of every field but
// capturedAt and the digest itself. encoding/json marshals struct fields in
// declaration order, so the encoding is deterministic.
func feedbackSnapshotDigest(s apiv1.PRFeedbackSnapshot) string {
	s.CapturedAt = ""
	s.SnapshotDigest = ""
	return sha256JSON(s)
}

func sha256JSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		// Every value passed here is a plain struct of strings, bools and
		// slices of the same; marshal cannot fail.
		panic(fmt.Sprintf("marshal canonical feedback: %v", err))
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func feedbackBodyDigest(body string) string {
	normalized := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// isMachineFeedback is the documented machine-content rule: a body carrying a
// Goobers hidden marker on a line of its own.
func isMachineFeedback(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "<!-- goobers") {
			return true
		}
	}
	return false
}

func canonicalGeneralComments(comments []providers.Comment) []apiv1.PRFeedbackComment {
	out := make([]apiv1.PRFeedbackComment, 0, len(comments))
	for _, c := range comments {
		if isMachineFeedback(c.Body) || strings.EqualFold(c.AuthorType, "bot") {
			continue
		}
		author := c.AuthorID
		if author == "" {
			author = c.Author
		}
		out = append(out, apiv1.PRFeedbackComment{ID: c.ID, Author: author, BodySHA256: feedbackBodyDigest(c.Body)})
	}
	sort.Slice(out, func(i, j int) bool { return lessFeedbackID(out[i].ID, out[j].ID) })
	return out
}

func canonicalReviews(reviews []providers.PullRequestNativeReview) []apiv1.PRFeedbackReview {
	out := make([]apiv1.PRFeedbackReview, 0, len(reviews))
	for _, r := range reviews {
		if strings.TrimSpace(r.Body) == "" || isMachineFeedback(r.Body) {
			continue
		}
		out = append(out, apiv1.PRFeedbackReview{ID: strconv.FormatInt(r.ID, 10), Author: r.Author, BodySHA256: feedbackBodyDigest(r.Body)})
	}
	sort.Slice(out, func(i, j int) bool { return lessFeedbackID(out[i].ID, out[j].ID) })
	return out
}

func canonicalThreads(comments []providers.PullRequestInlineComment) []apiv1.PRFeedbackThread {
	byID := map[string]*apiv1.PRFeedbackThread{}
	var ids []string
	for _, c := range comments {
		if strings.TrimSpace(c.ThreadID) == "" {
			continue
		}
		thread, ok := byID[c.ThreadID]
		if !ok {
			thread = &apiv1.PRFeedbackThread{ThreadID: c.ThreadID, Comments: []apiv1.PRFeedbackComment{}}
			byID[c.ThreadID] = thread
			ids = append(ids, c.ThreadID)
		}
		thread.Resolved = thread.Resolved || c.IsResolved
		thread.Outdated = thread.Outdated || c.IsOutdated
		if isMachineFeedback(c.Body) {
			continue
		}
		item := apiv1.PRFeedbackComment{ID: strconv.FormatInt(c.ID, 10), Author: c.Author, BodySHA256: feedbackBodyDigest(c.Body)}
		if c.InReplyTo != 0 {
			item.InReplyTo = strconv.FormatInt(c.InReplyTo, 10)
		}
		thread.Comments = append(thread.Comments, item)
	}
	sort.Slice(ids, func(i, j int) bool { return lessFeedbackID(ids[i], ids[j]) })
	out := make([]apiv1.PRFeedbackThread, 0, len(ids))
	for _, id := range ids {
		thread := byID[id]
		sort.Slice(thread.Comments, func(i, j int) bool { return lessFeedbackID(thread.Comments[i].ID, thread.Comments[j].ID) })
		thread.ContentDigest = sha256JSON(thread.Comments)
		out = append(out, *thread)
	}
	return out
}

// lessFeedbackID orders provider ids: numerically when both are decimal
// integers (GitHub and Gitea ids), lexically otherwise (ADO's composite and
// GitHub's GraphQL node ids).
func lessFeedbackID(a, b string) bool {
	ai, aerr := strconv.ParseInt(a, 10, 64)
	bi, berr := strconv.ParseInt(b, 10, 64)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a < b
}

// feedbackCompareOptions carries what a comparing stage legitimately changed
// itself, so its own mutations are never mistaken for someone else's.
type feedbackCompareOptions struct {
	// expectedHead is the head the live PR must be at: the snapshot's own
	// head before publication, this run's published head after it.
	expectedHead string
	// mayResolve names threads this run answers "addressed" and therefore
	// resolves: live resolution of one of them is this run's intended effect
	// (or a human agreeing with it), never staleness.
	mayResolve map[string]bool
	// resolvedByRun names threads whose resolution this run already verified;
	// finding one unresolved again means someone reopened it.
	resolvedByRun map[string]bool
}

// compareFeedbackSnapshot lists every way live differs from recorded, in a
// deterministic order led by the highest-priority reason.
func compareFeedbackSnapshot(recorded, live apiv1.PRFeedbackSnapshot, opts feedbackCompareOptions) []feedbackStaleReason {
	var reasons []feedbackStaleReason
	expected := opts.expectedHead
	if expected == "" {
		expected = recorded.HeadSHA
	}
	if !strings.EqualFold(strings.TrimSpace(live.HeadSHA), strings.TrimSpace(expected)) {
		reasons = append(reasons, feedbackStaleReason{Code: staleReasonHead, Kind: "head", ID: live.HeadSHA,
			Detail: fmt.Sprintf("head is %s, expected %s", live.HeadSHA, expected)})
	}
	if !live.Complete {
		reasons = append(reasons, feedbackStaleReason{Code: staleReasonIncomplete, Kind: "collection"})
	}
	reasons = append(reasons, compareFeedbackComments("generalComment", "", recorded.GeneralComments, live.GeneralComments)...)
	reasons = append(reasons, compareFeedbackReviews(recorded.Reviews, live.Reviews)...)
	reasons = append(reasons, compareFeedbackThreads(recorded.ReviewThreads, live.ReviewThreads, opts)...)
	sort.SliceStable(reasons, func(i, j int) bool {
		return staleReasonPriority[reasons[i].Code] < staleReasonPriority[reasons[j].Code]
	})
	return reasons
}

func compareFeedbackComments(kind, thread string, recorded, live []apiv1.PRFeedbackComment) []feedbackStaleReason {
	var reasons []feedbackStaleReason
	was := make(map[string]apiv1.PRFeedbackComment, len(recorded))
	for _, c := range recorded {
		was[c.ID] = c
	}
	seen := make(map[string]bool, len(live))
	for _, c := range live {
		seen[c.ID] = true
		prior, ok := was[c.ID]
		switch {
		case !ok:
			reasons = append(reasons, feedbackStaleReason{Code: staleReasonNew, Kind: kind, ID: c.ID, Detail: thread})
		case prior.BodySHA256 != c.BodySHA256 || prior.Author != c.Author || prior.InReplyTo != c.InReplyTo:
			reasons = append(reasons, feedbackStaleReason{Code: staleReasonChanged, Kind: kind, ID: c.ID, Detail: thread})
		}
	}
	for _, c := range recorded {
		if !seen[c.ID] {
			reasons = append(reasons, feedbackStaleReason{Code: staleReasonMissing, Kind: kind, ID: c.ID, Detail: thread})
		}
	}
	return reasons
}

func compareFeedbackReviews(recorded, live []apiv1.PRFeedbackReview) []feedbackStaleReason {
	toComments := func(reviews []apiv1.PRFeedbackReview) []apiv1.PRFeedbackComment {
		out := make([]apiv1.PRFeedbackComment, 0, len(reviews))
		for _, r := range reviews {
			out = append(out, apiv1.PRFeedbackComment{ID: r.ID, Author: r.Author, BodySHA256: r.BodySHA256})
		}
		return out
	}
	return compareFeedbackComments("review", "", toComments(recorded), toComments(live))
}

func compareFeedbackThreads(recorded, live []apiv1.PRFeedbackThread, opts feedbackCompareOptions) []feedbackStaleReason {
	var reasons []feedbackStaleReason
	now := make(map[string]apiv1.PRFeedbackThread, len(live))
	for _, t := range live {
		now[t.ThreadID] = t
	}
	was := make(map[string]bool, len(recorded))
	for _, prior := range recorded {
		was[prior.ThreadID] = true
		current, ok := now[prior.ThreadID]
		if !ok {
			reasons = append(reasons, feedbackStaleReason{Code: staleReasonMissing, Kind: "reviewThread", ID: prior.ThreadID})
			continue
		}
		reasons = append(reasons, compareFeedbackComments("reviewThreadComment", prior.ThreadID, prior.Comments, current.Comments)...)
		if reason, stale := threadStateChange(prior, current, opts); stale {
			reasons = append(reasons, reason)
		}
	}
	for _, t := range live {
		if !was[t.ThreadID] && len(t.Comments) > 0 {
			reasons = append(reasons, feedbackStaleReason{Code: staleReasonNew, Kind: "reviewThread", ID: t.ThreadID})
		}
	}
	return reasons
}

// threadStateChange applies the resolution rules. Unresolved→resolved is
// allowed only on a thread this run itself resolves; a thread this run
// verified as resolved that reads unresolved again was reopened; any other
// flip is someone else changing the thread's state.
func threadStateChange(prior, current apiv1.PRFeedbackThread, opts feedbackCompareOptions) (feedbackStaleReason, bool) {
	reason := feedbackStaleReason{Code: staleReasonThreadState, Kind: "reviewThread", ID: prior.ThreadID}
	switch {
	case prior.Resolved == current.Resolved:
		if !current.Resolved && opts.resolvedByRun[prior.ThreadID] {
			reason.Detail = "reopened after this run resolved it"
			return reason, true
		}
		return reason, false
	case current.Resolved && opts.mayResolve[prior.ThreadID]:
		return reason, false
	case current.Resolved:
		reason.Detail = "resolved outside this run"
	default:
		reason.Detail = "reopened"
	}
	return reason, true
}

func staleInputCode(reasons []feedbackStaleReason) string {
	if len(reasons) == 0 {
		return ""
	}
	return reasons[0].Code
}
