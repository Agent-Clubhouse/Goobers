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
