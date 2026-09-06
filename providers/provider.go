// Package providers defines repository and backlog provider integrations.
package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	errIssueIDRequired = errors.New("issue id is required")
	errPullIDRequired  = errors.New("pull id is required")
)

func validateCommitRequest(req CommitRequest) error {
	if req.Branch == "" {
		return errors.New("branch is required")
	}
	if req.Message == "" {
		return errors.New("message is required")
	}
	if len(req.Files) == 0 {
		return errors.New("at least one file is required")
	}
	return nil
}

func validateCommitFile(file CommitFile) error {
	if file.Path == "" {
		return errors.New("file path is required")
	}
	return nil
}

// Provider combines repo, backlog, and trigger operations for a backend.
type Provider interface {
	RepoProvider
	BacklogProvider
	TriggerProvider
	Kind() ProviderKind
	// Capabilities reports the surfaces this provider supports (design doc
	// docs/design/provider-contract-conformance.md §3.2). A capability
	// listed here MUST pass the conformance corpus for that capability;
	// one absent here MUST NOT be reachable at runtime — Dispatcher
	// refuses before the provider is called. This is the authority over
	// the optional interfaces below (BranchDeleter, PolicyProvider, …):
	// declaration and interface satisfaction are cross-checked by a
	// conformance test, so a provider cannot declare what it does not
	// implement, nor implement what it does not declare.
	Capabilities() CapabilitySet
}

// RepoProvider abstracts repository operations needed by Goobers runs. The
// landing surfaces (merge, enqueue/poll, compare, branch-delete) are NOT
// here: they gap on ADO today, so they live behind the optional interfaces
// below (PullRequestMerger, CommitComparer, MergePolicyDetector,
// PullRequestEnqueuer, MergeQueuePoller, BranchDeleter) and are reached only
// through Dispatcher, which refuses an undeclared capability before a
// provider ever sees the call (§3.2/§3.3 of the provider-contract design).
type RepoProvider interface {
	CloneRepository(context.Context, CloneRequest) (CloneResult, error)
	CreateBranch(context.Context, BranchRequest) (BranchResult, error)
	Commit(context.Context, CommitRequest) (CommitResult, error)
	OpenPullRequest(context.Context, PullRequestRequest) (PullRequestResult, error)
	RequestReview(context.Context, ReviewRequest) error
	// PollPullRequest reports review/CI state as a deterministic stage output so a
	// gate can drive the CI-poll/repass loop (BL-031, ARCHITECTURE.md §12).
	PollPullRequest(context.Context, PullRequestPollRequest) (PullRequestPollResult, error)
	// ClosePullRequest closes a pull request, detecting merged-vs-closed (BL-031).
	ClosePullRequest(context.Context, ClosePullRequestRequest) (ClosePullRequestResult, error)
	// ListPullRequests lists open pull requests matching req — merge-review's
	// selection stage and sibling-set context gathering (issue #359), and
	// #361's post-merge fan-out (find every other open PR targeting the
	// merged PR's base branch).
	ListPullRequests(context.Context, ListPullRequestsRequest) ([]PullRequestSummary, error)
	// PullRequestFiles lists the files a pull request touches — merge-review's
	// sibling-set context gathering (issue #359): what does the OTHER open PR
	// change, for cross-PR conflict/drift detection.
	PullRequestFiles(context.Context, RepositoryRef, string) ([]ChangedFile, error)
}

// PullRequestMerger merges a pull request (issue #360) — the provider-level
// primitive a conjunctive auto-merge action calls only after independently
// verifying every merge conjunct; see MergePullRequestRequest's doc. It is
// optional (pr.merge) because ADO's landing surfaces gap today (#2061).
type PullRequestMerger interface {
	MergePullRequest(context.Context, MergePullRequestRequest) (MergePullRequestResult, error)
}

// CommitComparer reports the common ancestor and file-level diff between
// base and head (issue #718): merge-review's verdict-cache re-keying uses
// this for the selected PR's own patch content (base=its baseSHA,
// head=its headSHA) and for "what changed on base since this PR's
// merge-base" (base=that merge-base, head=base's current tip) — both needed
// to make the cache key and merge-pr's SHA-pin check delta-aware instead of
// raw-SHA-equality. Optional (pr.compare); ADO gaps it today (#2061).
type CommitComparer interface {
	CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error)
}

// MergePolicyDetector reports a repo's active merge policy for a branch
// (issue #758), read from its live branch protection/ruleset state — the
// detection half of the merge-policy abstraction internal/mergepolicy's
// Land dispatches on. Optional (pr.landing.detect-policy); ADO gaps it
// today (#2061).
type MergePolicyDetector interface {
	DetectMergePolicy(context.Context, RepoMergePolicyRequest) (RepoMergePolicyResult, error)
}

// PullRequestEnqueuer adds a pull request to its repo's merge queue (issue
// #758) — the enqueue-policy counterpart to PullRequestMerger; see
// EnqueuePullRequestRequest's doc. Optional (pr.landing.enqueue); ADO gaps
// it today (#2061).
type PullRequestEnqueuer interface {
	EnqueuePullRequest(context.Context, EnqueuePullRequestRequest) (EnqueuePullRequestResult, error)
}

// MergeQueuePoller reports whether the merge queue has since merged or
// evicted a pull request previously enqueued via PullRequestEnqueuer (issue
// #758) — the eviction-as-first-class-outcome half of the merge-policy
// abstraction. Optional (pr.landing.poll); ADO gaps it today (#2061).
type MergeQueuePoller interface {
	PollMergeQueueEntry(context.Context, PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error)
}

// WorkItemBlockerChecker reports whether a work item has an unresolved
// native blocker. Optional (backlog.blockers). ADO does not implement or
// declare it — dependency-link modeling reaches parity under #2061 — so the call
// site (cmd/goobers/backlogquery.go) goes through Dispatcher: an ADO item
// with a nonzero BlockedByCount fails closed (excluded with a warning)
// instead of the fail-open stub #2059 used to return (CONF-5, #2078).
type WorkItemBlockerChecker interface {
	HasOpenWorkItemBlocker(context.Context, RepositoryRef, string) (bool, error)
}

// BranchDeleter removes remote branch refs. It is separate from RepoProvider
// so V0's GitHub-only cleanup does not widen every provider implementation.
type BranchDeleter interface {
	DeleteBranch(context.Context, DeleteBranchRequest) (DeleteBranchResult, error)
}

// BranchReconciliationProvider exposes the bounded reads and mutation needed
// to reconcile stale run branches without widening every RepoProvider backend.
type BranchReconciliationProvider interface {
	ListBranches(context.Context, ListBranchesRequest) ([]BranchSummary, error)
	GetBranch(context.Context, RepositoryRef, string) (BranchSummary, bool, error)
	FindPullRequestByBranch(context.Context, RepositoryRef, string, string) (PullRequestResult, bool, error)
	DeleteBranch(context.Context, DeleteBranchRequest) (DeleteBranchResult, error)
}

// PullRequestStatusPublisher publishes a provider-native pull-request status a
// status-check branch policy can gate on (#772). It is separate from
// RepoProvider because publishing PR statuses as evidence for a policy-based
// validation loop is currently an Azure DevOps parity capability and must not
// widen every backend.
type PullRequestStatusPublisher interface {
	PublishPullRequestStatus(context.Context, PullRequestStatusRequest) (PullRequestStatusResult, error)
}

// PendingCheckCanceler cancels provider CI still running for one exact PR head.
// It is optional because not every forge exposes a safe cancellation surface.
type PendingCheckCanceler interface {
	CancelPendingChecks(context.Context, CancelPendingChecksRequest) (CancelPendingChecksResult, error)
}

// RepositoryWritePreflighter is documented alongside its dispatch method in
// repository_write_preflight.go and its interface declaration there — kept
// out of RepoProvider so the GitHub-only V1 scope (#4414) does not widen
// every RepoProvider backend, like BranchDeleter and PolicyProvider.

// PolicyProvider reports a repo's live forge-conformance settings (issue
// #916, Tier 4 of #903) for `goobers doctor --repo` to diff against a
// declared instance-config manifest. It is separate from RepoProvider, like
// BranchDeleter/BranchReconciliationProvider, so the GitHub-only V1 scope
// does not widen every RepoProvider backend (e.g. ADO).
type PolicyProvider interface {
	GetRepoPolicy(context.Context, RepoPolicyRequest) (RepoPolicyResult, error)
}

// PullRequestReviewSubmitter publishes provider-native review verdicts. It is
// separate from RepoProvider because V1's native-review protocol is currently
// implemented only for GitHub.
type PullRequestReviewSubmitter interface {
	SubmitPullRequestReview(context.Context, PullRequestReviewRequest) (PullRequestReviewResult, error)
}

// PullRequestReviewThreadProvider reads provider-native reviews and inline
// review threads without widening every RepoProvider backend.
type PullRequestReviewThreadProvider interface {
	ListPullRequestReviewThreads(context.Context, RepositoryRef, string) (PullRequestReviewThreads, error)
}

// PullRequestReviewThreadMutator replies to and resolves provider-native review
// threads without widening every RepoProvider backend.
type PullRequestReviewThreadMutator interface {
	ReplyPullRequestReviewThread(context.Context, PullRequestReviewThreadReply) (PullRequestInlineComment, error)
	ResolvePullRequestReviewThread(context.Context, RepositoryRef, string) error
}

// PullRequestBranchUpdater incorporates a pull request's base branch through
// the provider API. It is separate from RepoProvider because Azure DevOps does
// not yet expose the V0 GitHub update-branch primitive.
type PullRequestBranchUpdater interface {
	UpdateBranch(context.Context, UpdateBranchRequest) (UpdateBranchResult, error)
}

// DefaultBranchNamespace is the refs/heads/ root every run branch lives under
// by default: BranchName produces "<namespace><workflow>/<run-id>". It is the
// single seam of truth three otherwise-independent consumers must agree on
// (#965): the producer (BranchName here), the mirror-fetch exclusion that
// preserves a run's unpushed branch across stages (internal/worktree), and the
// PR-selector headPrefix defaults (cmd/goobers). If any one of them restates
// the literal and the others move, a run's rebased-but-unpushed branch is
// silently force-reset back to origin — the #965 / #133 silent-revert class.
// A gaggle may retune the namespace (GaggleSpec.BranchNamespace), but the value
// then flows to all three together rather than being hardcoded in each.
const DefaultBranchNamespace = "goobers/"

// NormalizeBranchNamespace returns ns as a usable refs/heads/ prefix: the
// DefaultBranchNamespace when ns is empty, and otherwise ns with a single
// trailing "/" guaranteed so callers can concatenate "<namespace><workflow>/…"
// uniformly regardless of whether the configured value included it.
func NormalizeBranchNamespace(ns string) string {
	if ns == "" {
		return DefaultBranchNamespace
	}
	if !strings.HasSuffix(ns, "/") {
		return ns + "/"
	}
	return ns
}

// BranchName returns the run-scoped branch-name convention the repo provider
// owns (BL-010/#13) under the DefaultBranchNamespace: the worktree manager
// pushes to it, the provider never does. Use BranchNameIn when a gaggle has
// retuned its branch namespace.
func BranchName(workflow, runID string) string {
	return BranchNameIn(DefaultBranchNamespace, workflow, runID)
}

// BranchNameIn is BranchName under an explicit branch namespace (a gaggle's
// configured GaggleSpec.BranchNamespace). An empty namespace falls back to the
// DefaultBranchNamespace, so callers with no configured value get identical
// output to BranchName.
func BranchNameIn(namespace, workflow, runID string) string {
	return fmt.Sprintf("%s%s/%s", NormalizeBranchNamespace(namespace), workflow, runID)
}

// BacklogProvider abstracts backlog work item operations: query/read, create,
// general edit (title/body/label/milestone/close/comment), status mirroring, and
// claiming. The GitHub implementation is the V0 workload; ADO reaches parity in
// V1 (BL-033).
type BacklogProvider interface {
	ListWorkItems(context.Context, ListWorkItemsRequest) ([]WorkItem, error)
	GetWorkItem(context.Context, RepositoryRef, string) (WorkItem, error)
	ListComments(context.Context, RepositoryRef, string) ([]Comment, error)
	CreateWorkItem(context.Context, CreateWorkItemRequest) (WorkItem, error)
	UpdateWorkItem(context.Context, UpdateWorkItemRequest) (WorkItem, error)
	UpdateWorkItemStatus(context.Context, UpdateWorkItemStatusRequest) (WorkItem, error)
	ClaimWorkItem(context.Context, ClaimWorkItemRequest) (ClaimResult, error)
}

// TriggerProvider abstracts backlog item availability triggers.
type TriggerProvider interface {
	Subscribe(context.Context, TriggerSubscription) (<-chan WorkItemEvent, error)
}
