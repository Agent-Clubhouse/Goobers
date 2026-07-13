package providers

import "context"

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
