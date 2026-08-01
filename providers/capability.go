package providers

import (
	"context"
	"fmt"
)

// Capability names a single surface a provider may support (design doc
// docs/design/provider-contract-conformance.md §3.1). The set is closed and
// append-only: a new surface is a new constant here, never a repurposed one
// — existing declarations and the conformance corpus both key off the exact
// string value.
type Capability string

// repo.core: the git-level primitives every workflow assumes.
const (
	CapRepoClone  Capability = "repo.clone"
	CapRepoBranch Capability = "repo.branch"
	CapRepoCommit Capability = "repo.commit"
	CapRepoPush   Capability = "repo.push"
)

// pr.core: pull-request surfaces every workflow-required provider must
// carry, except pr.compare (§3.1: CompareCommits is gapped on ADO today, so
// it is declared per-provider rather than assumed).
const (
	CapPROpen    Capability = "pr.open"
	CapPRList    Capability = "pr.list"
	CapPRPoll    Capability = "pr.poll"
	CapPRClose   Capability = "pr.close"
	CapPRFiles   Capability = "pr.files"
	CapPRCompare Capability = "pr.compare"
)

// pr.review: native review protocol surfaces.
const (
	CapPRReviewRequest Capability = "pr.review.request"
	CapPRReviewSubmit  Capability = "pr.review.submit"
	CapPRReviewThreads Capability = "pr.review.threads"
)

// pr.landing: the landing contract (§4) — merge, queue, branch-delete,
// compare, and update-branch. This is the group ADO's declaration honestly
// excludes as of CONF-1; CONF-3 (#2076) flips the gaps to conformant.
const (
	CapPRMerge               Capability = "pr.merge"
	CapPRLandingDetectPolicy Capability = "pr.landing.detect-policy"
	CapPRLandingEnqueue      Capability = "pr.landing.enqueue"
	CapPRLandingPoll         Capability = "pr.landing.poll"
	CapPRUpdateBranch        Capability = "pr.update-branch"
	CapBranchDelete          Capability = "branch.delete"
)

// policy: forge-native policy read/write surfaces.
const (
	CapRepoPolicyRead  Capability = "repo.policy.read"
	CapPRStatusPublish Capability = "pr.status.publish"
)

// backlog: work-item surfaces. CapBacklogBlockers is declared per-provider
// rather than folded into mandatoryCapabilities(): today every provider
// implements HasOpenWorkItemBlocker (cmd/goobers/backlogquery.go's
// backlogIssueProvider already requires it structurally), but ADO's
// implementation is a known fail-open stub, not a real native-dependency
// read (#2059) — fixing that (deleting the stub, routing the call through
// Dispatcher/WorkItemBlockerChecker so an honest gap fails closed instead)
// is CONF-5 (#2078), not this issue.
const (
	CapBacklogList     Capability = "backlog.list"
	CapBacklogGet      Capability = "backlog.get"
	CapBacklogComments Capability = "backlog.comments"
	CapBacklogCreate   Capability = "backlog.create"
	CapBacklogUpdate   Capability = "backlog.update"
	CapBacklogStatus   Capability = "backlog.status"
	CapBacklogClaim    Capability = "backlog.claim"
	CapBacklogBlockers Capability = "backlog.blockers"
)

// trigger: backlog availability triggers.
const (
	CapTriggerSubscribe Capability = "trigger.subscribe"
)

// mandatoryCapabilities are the surfaces every RepoProvider/BacklogProvider/
// TriggerProvider implementation carries by Go-interface construction (the
// compiler already enforces the method exists), so every Provider declares
// them unconditionally. Capabilities absent from this set are optional —
// backed by one of the small interfaces in provider.go — and must be
// declared per-provider to match what that provider actually implements.
func mandatoryCapabilities() CapabilitySet {
	return NewCapabilitySet(
		CapRepoClone, CapRepoBranch, CapRepoCommit, CapRepoPush,
		CapPROpen, CapPRList, CapPRPoll, CapPRClose, CapPRFiles,
		CapPRReviewRequest,
		CapBacklogList, CapBacklogGet, CapBacklogComments, CapBacklogCreate,
		CapBacklogUpdate, CapBacklogStatus, CapBacklogClaim,
		CapTriggerSubscribe,
	)
}

// CapabilitySet is the closed set of capabilities one provider declares.
// Presence is the only signal: a capability's absence means the dispatch
// shim refuses calls into it before the provider is ever reached (§3.3).
type CapabilitySet map[Capability]struct{}

// NewCapabilitySet builds a CapabilitySet from caps.
func NewCapabilitySet(caps ...Capability) CapabilitySet {
	set := make(CapabilitySet, len(caps))
	for _, c := range caps {
		set[c] = struct{}{}
	}
	return set
}

// Has reports whether c is declared in the set.
func (s CapabilitySet) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}

// With returns a new CapabilitySet containing s plus caps — used to compose
// a provider's optional declarations onto mandatoryCapabilities() without
// mutating the shared baseline.
func (s CapabilitySet) With(caps ...Capability) CapabilitySet {
	out := make(CapabilitySet, len(s)+len(caps))
	for c := range s {
		out[c] = struct{}{}
	}
	for _, c := range caps {
		out[c] = struct{}{}
	}
	return out
}

// ErrUnsupported is returned by the dispatch shim when a caller invokes a
// capability its provider has not declared. It is a first-class, typed gap
// signal (§3.3) — never a zero-value result a caller could mistake for a
// real answer, which is what let the #2059 fail-open class happen.
type ErrUnsupported struct {
	Provider   ProviderKind
	Capability Capability
}

func (e ErrUnsupported) Error() string {
	return fmt.Sprintf("provider %q does not declare capability %q", e.Provider, e.Capability)
}

// Dispatcher wraps a Provider and is the sole authority (§3.2) for the
// capabilities that are optional per-provider: it checks Capabilities()
// before dispatching, so an undeclared capability returns ErrUnsupported
// without the provider ever seeing the call, and callers no longer probe
// optional interfaces by hand at each call site.
type Dispatcher struct {
	Provider
}

// NewDispatcher wraps p for capability-checked dispatch.
func NewDispatcher(p Provider) *Dispatcher {
	return &Dispatcher{Provider: p}
}

// MergePullRequest dispatches to the underlying provider's
// PullRequestMerger if pr.merge is declared, else returns ErrUnsupported.
func (d *Dispatcher) MergePullRequest(ctx context.Context, req MergePullRequestRequest) (MergePullRequestResult, error) {
	if !d.Capabilities().Has(CapPRMerge) {
		return MergePullRequestResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRMerge}
	}
	merger, ok := d.Provider.(PullRequestMerger)
	if !ok {
		return MergePullRequestResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRMerge}
	}
	return merger.MergePullRequest(ctx, req)
}

// CompareCommits dispatches to the underlying provider's CommitComparer if
// pr.compare is declared, else returns ErrUnsupported.
func (d *Dispatcher) CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error) {
	if !d.Capabilities().Has(CapPRCompare) {
		return CompareResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRCompare}
	}
	comparer, ok := d.Provider.(CommitComparer)
	if !ok {
		return CompareResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRCompare}
	}
	return comparer.CompareCommits(ctx, repo, base, head)
}

// DetectMergePolicy dispatches to the underlying provider's
// MergePolicyDetector if pr.landing.detect-policy is declared, else
// returns ErrUnsupported.
func (d *Dispatcher) DetectMergePolicy(ctx context.Context, req RepoMergePolicyRequest) (RepoMergePolicyResult, error) {
	if !d.Capabilities().Has(CapPRLandingDetectPolicy) {
		return RepoMergePolicyResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRLandingDetectPolicy}
	}
	detector, ok := d.Provider.(MergePolicyDetector)
	if !ok {
		return RepoMergePolicyResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRLandingDetectPolicy}
	}
	return detector.DetectMergePolicy(ctx, req)
}

// EnqueuePullRequest dispatches to the underlying provider's
// PullRequestEnqueuer if pr.landing.enqueue is declared, else returns
// ErrUnsupported.
func (d *Dispatcher) EnqueuePullRequest(ctx context.Context, req EnqueuePullRequestRequest) (EnqueuePullRequestResult, error) {
	if !d.Capabilities().Has(CapPRLandingEnqueue) {
		return EnqueuePullRequestResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRLandingEnqueue}
	}
	enqueuer, ok := d.Provider.(PullRequestEnqueuer)
	if !ok {
		return EnqueuePullRequestResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRLandingEnqueue}
	}
	return enqueuer.EnqueuePullRequest(ctx, req)
}

// PollMergeQueueEntry dispatches to the underlying provider's
// MergeQueuePoller if pr.landing.poll is declared, else returns
// ErrUnsupported.
func (d *Dispatcher) PollMergeQueueEntry(ctx context.Context, req PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error) {
	if !d.Capabilities().Has(CapPRLandingPoll) {
		return PollMergeQueueEntryResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRLandingPoll}
	}
	poller, ok := d.Provider.(MergeQueuePoller)
	if !ok {
		return PollMergeQueueEntryResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRLandingPoll}
	}
	return poller.PollMergeQueueEntry(ctx, req)
}

// DeleteBranch dispatches to the underlying provider's BranchDeleter if
// branch.delete is declared, else returns ErrUnsupported.
func (d *Dispatcher) DeleteBranch(ctx context.Context, req DeleteBranchRequest) (DeleteBranchResult, error) {
	if !d.Capabilities().Has(CapBranchDelete) {
		return DeleteBranchResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapBranchDelete}
	}
	deleter, ok := d.Provider.(BranchDeleter)
	if !ok {
		return DeleteBranchResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapBranchDelete}
	}
	return deleter.DeleteBranch(ctx, req)
}

// UpdateBranch dispatches to the underlying provider's
// PullRequestBranchUpdater if pr.update-branch is declared, else returns
// ErrUnsupported.
func (d *Dispatcher) UpdateBranch(ctx context.Context, req UpdateBranchRequest) (UpdateBranchResult, error) {
	if !d.Capabilities().Has(CapPRUpdateBranch) {
		return UpdateBranchResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRUpdateBranch}
	}
	updater, ok := d.Provider.(PullRequestBranchUpdater)
	if !ok {
		return UpdateBranchResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRUpdateBranch}
	}
	return updater.UpdateBranch(ctx, req)
}

// SubmitPullRequestReview dispatches to the underlying provider's
// PullRequestReviewSubmitter if pr.review.submit is declared, else returns
// ErrUnsupported.
func (d *Dispatcher) SubmitPullRequestReview(ctx context.Context, req PullRequestReviewRequest) (PullRequestReviewResult, error) {
	if !d.Capabilities().Has(CapPRReviewSubmit) {
		return PullRequestReviewResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRReviewSubmit}
	}
	submitter, ok := d.Provider.(PullRequestReviewSubmitter)
	if !ok {
		return PullRequestReviewResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRReviewSubmit}
	}
	return submitter.SubmitPullRequestReview(ctx, req)
}

// ListPullRequestReviewThreads dispatches to the underlying provider's
// PullRequestReviewThreadProvider if pr.review.threads is declared, else
// returns ErrUnsupported.
func (d *Dispatcher) ListPullRequestReviewThreads(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestReviewThreads, error) {
	if !d.Capabilities().Has(CapPRReviewThreads) {
		return PullRequestReviewThreads{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRReviewThreads}
	}
	reader, ok := d.Provider.(PullRequestReviewThreadProvider)
	if !ok {
		return PullRequestReviewThreads{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRReviewThreads}
	}
	return reader.ListPullRequestReviewThreads(ctx, repo, pullID)
}

// PublishPullRequestStatus dispatches to the underlying provider's
// PullRequestStatusPublisher if pr.status.publish is declared, else
// returns ErrUnsupported.
func (d *Dispatcher) PublishPullRequestStatus(ctx context.Context, req PullRequestStatusRequest) (PullRequestStatusResult, error) {
	if !d.Capabilities().Has(CapPRStatusPublish) {
		return PullRequestStatusResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRStatusPublish}
	}
	publisher, ok := d.Provider.(PullRequestStatusPublisher)
	if !ok {
		return PullRequestStatusResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapPRStatusPublish}
	}
	return publisher.PublishPullRequestStatus(ctx, req)
}

// GetRepoPolicy dispatches to the underlying provider's PolicyProvider if
// repo.policy.read is declared, else returns ErrUnsupported.
func (d *Dispatcher) GetRepoPolicy(ctx context.Context, req RepoPolicyRequest) (RepoPolicyResult, error) {
	if !d.Capabilities().Has(CapRepoPolicyRead) {
		return RepoPolicyResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapRepoPolicyRead}
	}
	reader, ok := d.Provider.(PolicyProvider)
	if !ok {
		return RepoPolicyResult{}, ErrUnsupported{Provider: d.Kind(), Capability: CapRepoPolicyRead}
	}
	return reader.GetRepoPolicy(ctx, req)
}

// HasOpenWorkItemBlocker dispatches to the underlying provider's
// WorkItemBlockerChecker if backlog.blockers is declared, else returns
// ErrUnsupported.
func (d *Dispatcher) HasOpenWorkItemBlocker(ctx context.Context, repo RepositoryRef, id string) (bool, error) {
	if !d.Capabilities().Has(CapBacklogBlockers) {
		return false, ErrUnsupported{Provider: d.Kind(), Capability: CapBacklogBlockers}
	}
	checker, ok := d.Provider.(WorkItemBlockerChecker)
	if !ok {
		return false, ErrUnsupported{Provider: d.Kind(), Capability: CapBacklogBlockers}
	}
	return checker.HasOpenWorkItemBlocker(ctx, repo, id)
}
