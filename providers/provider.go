package providers

import (
	"context"
	"fmt"
)

// Provider combines repo, backlog, and trigger operations for a backend.
type Provider interface {
	RepoProvider
	BacklogProvider
	TriggerProvider
	Kind() ProviderKind
}

// RepoProvider abstracts repository operations needed by Goobers runs.
type RepoProvider interface {
	CloneRepository(context.Context, CloneRequest) (CloneResult, error)
	CreateBranch(context.Context, BranchRequest) (BranchResult, error)
	DeleteBranch(context.Context, DeleteBranchRequest) (DeleteBranchResult, error)
	Commit(context.Context, CommitRequest) (CommitResult, error)
	OpenPullRequest(context.Context, PullRequestRequest) (PullRequestResult, error)
	RequestReview(context.Context, ReviewRequest) error
	// PollPullRequest reports review/CI state as a deterministic stage output so a
	// gate can drive the CI-poll/repass loop (BL-031, ARCHITECTURE.md §12).
	PollPullRequest(context.Context, PullRequestPollRequest) (PullRequestPollResult, error)
	// ClosePullRequest closes a pull request, detecting merged-vs-closed (BL-031).
	ClosePullRequest(context.Context, ClosePullRequestRequest) (ClosePullRequestResult, error)
	// MergePullRequest merges a pull request (issue #360) — the provider-level
	// primitive a conjunctive auto-merge action calls only after independently
	// verifying every merge conjunct; see MergePullRequestRequest's doc.
	MergePullRequest(context.Context, MergePullRequestRequest) (MergePullRequestResult, error)
	// ListPullRequests lists open pull requests matching req — merge-review's
	// selection stage and sibling-set context gathering (issue #359), and
	// #361's post-merge fan-out (find every other open PR targeting the
	// merged PR's base branch).
	ListPullRequests(context.Context, ListPullRequestsRequest) ([]PullRequestSummary, error)
	// PullRequestFiles lists the files a pull request touches — merge-review's
	// sibling-set context gathering (issue #359): what does the OTHER open PR
	// change, for cross-PR conflict/drift detection.
	PullRequestFiles(context.Context, RepositoryRef, string) ([]ChangedFile, error)
	// CompareCommits reports the common ancestor and file-level diff between
	// base and head (issue #718): merge-review's verdict-cache re-keying
	// uses this for the selected PR's own patch content (base=its baseSHA,
	// head=its headSHA) and for "what changed on base since this PR's
	// merge-base" (base=that merge-base, head=base's current tip) — both
	// needed to make the cache key and merge-pr's SHA-pin check delta-aware
	// instead of raw-SHA-equality.
	CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error)
	// DetectMergePolicy reports a repo's active merge policy for a branch
	// (issue #758), read from its live branch protection/ruleset state —
	// the detection half of the merge-policy abstraction internal/
	// mergepolicy's Land dispatches on.
	DetectMergePolicy(context.Context, RepoMergePolicyRequest) (RepoMergePolicyResult, error)
	// EnqueuePullRequest adds a pull request to its repo's merge queue
	// (issue #758) — the enqueue-policy counterpart to MergePullRequest;
	// see EnqueuePullRequestRequest's doc.
	EnqueuePullRequest(context.Context, EnqueuePullRequestRequest) (EnqueuePullRequestResult, error)
	// PollMergeQueueEntry reports whether the merge queue has since merged
	// or evicted a pull request previously enqueued via EnqueuePullRequest
	// (issue #758) — the eviction-as-first-class-outcome half of the
	// merge-policy abstraction.
	PollMergeQueueEntry(context.Context, PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error)
}

// BranchDeleter removes remote branch refs. It is separate from RepoProvider
// so V0's GitHub-only cleanup does not widen every provider implementation.
type BranchDeleter interface {
	DeleteBranch(context.Context, DeleteBranchRequest) (DeleteBranchResult, error)
}

// PullRequestReviewSubmitter publishes provider-native review verdicts. It is
// separate from RepoProvider because V1's native-review protocol is currently
// implemented only for GitHub.
type PullRequestReviewSubmitter interface {
	SubmitPullRequestReview(context.Context, PullRequestReviewRequest) (PullRequestReviewResult, error)
}

// BranchName returns the run-scoped branch-name convention the repo provider
// owns (BL-010/#13): the worktree manager pushes to it, the provider never does.
func BranchName(workflow, runID string) string {
	return fmt.Sprintf("goobers/%s/%s", workflow, runID)
}

// BacklogProvider abstracts backlog work item operations: query/read, create,
// general edit (title/body/label/close/comment), status mirroring, and claiming.
// The GitHub implementation is the V0 workload; ADO reaches parity in V1 (BL-033).
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
