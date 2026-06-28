package providers

import "time"

// ProviderKind identifies a concrete provider backend.
type ProviderKind string

// Provider kinds supported by the provider abstraction.
const (
	ProviderGitHub ProviderKind = "github"
	ProviderADO    ProviderKind = "ado"
)

// WorkItemStatus is the Goobers processing status mirrored to backlog items.
type WorkItemStatus string

// Work item statuses used for human-visible processing state.
const (
	WorkItemStatusOpen       WorkItemStatus = "open"
	WorkItemStatusClaimed    WorkItemStatus = "claimed"
	WorkItemStatusInProgress WorkItemStatus = "in-progress"
	WorkItemStatusDone       WorkItemStatus = "done"
	WorkItemStatusClosed     WorkItemStatus = "closed"
)

// WorkItemRef identifies a related work item without normalizing provider hierarchy.
type WorkItemRef struct {
	Provider ProviderKind `json:"provider"`
	ID       string       `json:"id"`
	URL      string       `json:"url,omitempty"`
	Type     string       `json:"type,omitempty"`
}

// Link preserves provider-native relationship links.
type Link struct {
	Rel string `json:"rel"`
	URL string `json:"url"`
}

// WorkItem is the flat scheduler-facing backlog model shared across providers.
type WorkItem struct {
	Provider   ProviderKind           `json:"provider"`
	ID         string                 `json:"id"`
	ExternalID string                 `json:"externalId,omitempty"`
	Type       string                 `json:"type,omitempty"`
	Title      string                 `json:"title"`
	Body       string                 `json:"body,omitempty"`
	Labels     []string               `json:"labels,omitempty"`
	State      string                 `json:"state,omitempty"`
	Status     WorkItemStatus         `json:"status,omitempty"`
	Assignee   string                 `json:"assignee,omitempty"`
	Links      []Link                 `json:"links,omitempty"`
	Parent     *WorkItemRef           `json:"parent,omitempty"`
	Hierarchy  map[string]interface{} `json:"hierarchy,omitempty"`
	URL        string                 `json:"url,omitempty"`
	UpdatedAt  *time.Time             `json:"updatedAt,omitempty"`
	Raw        interface{}            `json:"raw,omitempty"`
}

// HasLabel reports whether the work item has a scheduler routing label.
func (w WorkItem) HasLabel(label string) bool {
	for _, itemLabel := range w.Labels {
		if itemLabel == label {
			return true
		}
	}
	return false
}

// RepositoryRef identifies a repository in a provider backend.
type RepositoryRef struct {
	Provider ProviderKind `json:"provider"`
	Owner    string       `json:"owner,omitempty"`
	Project  string       `json:"project,omitempty"`
	Name     string       `json:"name"`
	ID       string       `json:"id,omitempty"`
	URL      string       `json:"url,omitempty"`
}

// CloneRequest describes a repository clone operation.
type CloneRequest struct {
	Repository  RepositoryRef `json:"repository"`
	Destination string        `json:"destination"`
	Branch      string        `json:"branch,omitempty"`
}

// CloneResult describes the local clone path and source URL.
type CloneResult struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

// BranchRequest describes a branch creation operation.
type BranchRequest struct {
	Repository RepositoryRef `json:"repository"`
	BaseBranch string        `json:"baseBranch,omitempty"`
	BaseSHA    string        `json:"baseSha,omitempty"`
	Name       string        `json:"name"`
}

// BranchResult describes a created branch.
type BranchResult struct {
	Name string `json:"name"`
	SHA  string `json:"sha,omitempty"`
	URL  string `json:"url,omitempty"`
}

// CommitChangeType identifies how a file changes in a commit.
type CommitChangeType string

// Commit change types supported by repo providers.
const (
	CommitChangeAdd    CommitChangeType = "add"
	CommitChangeEdit   CommitChangeType = "edit"
	CommitChangeDelete CommitChangeType = "delete"
)

// CommitFile describes a single file mutation in a provider commit.
type CommitFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	ChangeType string `json:"changeType,omitempty"`
}

// CommitRequest describes file changes to commit to a branch.
type CommitRequest struct {
	Repository RepositoryRef `json:"repository"`
	Branch     string        `json:"branch"`
	BaseSHA    string        `json:"baseSha,omitempty"`
	Message    string        `json:"message"`
	Files      []CommitFile  `json:"files"`
}

// CommitResult describes the provider commit produced by a commit operation.
type CommitResult struct {
	SHA string `json:"sha"`
	URL string `json:"url,omitempty"`
}

// PullRequestRequest describes a pull request to open.
type PullRequestRequest struct {
	Repository RepositoryRef `json:"repository"`
	Title      string        `json:"title"`
	Body       string        `json:"body,omitempty"`
	Head       string        `json:"head"`
	Base       string        `json:"base"`
	Draft      bool          `json:"draft,omitempty"`
}

// PullRequestResult describes an opened pull request.
type PullRequestResult struct {
	ID     string `json:"id"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url"`
}

// ReviewRequest describes reviewers to request on a pull request.
type ReviewRequest struct {
	Repository RepositoryRef `json:"repository"`
	PullID     string        `json:"pullId"`
	Reviewers  []string      `json:"reviewers"`
}

// ListWorkItemsRequest filters backlog items for scheduler admission.
type ListWorkItemsRequest struct {
	Repository RepositoryRef `json:"repository"`
	Labels     []string      `json:"labels,omitempty"`
	State      string        `json:"state,omitempty"`
	Limit      int           `json:"limit,omitempty"`
}

// CreateWorkItemRequest describes a backlog item to create.
type CreateWorkItemRequest struct {
	Repository RepositoryRef  `json:"repository"`
	Type       string         `json:"type,omitempty"`
	Title      string         `json:"title"`
	Body       string         `json:"body,omitempty"`
	Labels     []string       `json:"labels,omitempty"`
	Assignee   string         `json:"assignee,omitempty"`
	Parent     *WorkItemRef   `json:"parent,omitempty"`
	Links      []Link         `json:"links,omitempty"`
	Status     WorkItemStatus `json:"status,omitempty"`
}

// UpdateWorkItemStatusRequest describes a processing-status mirror update.
type UpdateWorkItemStatusRequest struct {
	Repository RepositoryRef  `json:"repository"`
	ID         string         `json:"id"`
	Status     WorkItemStatus `json:"status"`
	Comment    string         `json:"comment,omitempty"`
}

// TriggerKind identifies how backlog availability events are delivered.
type TriggerKind string

// Trigger kinds supported by provider subscriptions.
const (
	TriggerWebhook TriggerKind = "webhook"
	TriggerPolling TriggerKind = "polling"
)

// TriggerSubscription describes a backlog item availability subscription.
type TriggerSubscription struct {
	Provider     ProviderKind  `json:"provider"`
	Kind         TriggerKind   `json:"kind"`
	Repository   RepositoryRef `json:"repository"`
	Secret       string        `json:"secret,omitempty"`
	PollInterval time.Duration `json:"-"`
}

// WorkItemEvent is emitted when a backlog item is available or changed.
type WorkItemEvent struct {
	Provider ProviderKind `json:"provider"`
	Kind     TriggerKind  `json:"kind"`
	Item     WorkItem     `json:"item"`
	Action   string       `json:"action,omitempty"`
}
