package providers

import "time"

// ProviderKind identifies a concrete provider backend.
type ProviderKind string

// Provider kinds supported by the provider abstraction.
const (
	ProviderGitHub ProviderKind = "github"
	ProviderADO    ProviderKind = "ado"
)

// Goobers marker labels applied to backlog items. The claim label mirrors the
// runner's lease for human visibility (BL-032); the ready label is the curated
// marker meaning an item is scoped and eligible for implementation.
const (
	LabelClaimed = "goobers:claimed"
	LabelReady   = "goobers:ready"
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

// Comment is a comment on a backlog work item (a GitHub issue comment).
type Comment struct {
	ID        string     `json:"id"`
	Author    string     `json:"author,omitempty"`
	Body      string     `json:"body"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	URL       string     `json:"url,omitempty"`
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
	Assignee   string        `json:"assignee,omitempty"`
	// UpdatedSince, when set, restricts results to items updated at or after it.
	UpdatedSince *time.Time `json:"updatedSince,omitempty"`
	Limit        int        `json:"limit,omitempty"`
	// Page selects a 1-based page for stable pagination; 0 means the first page.
	Page int `json:"page,omitempty"`
}

// UpdateWorkItemRequest is a general backlog item edit: title/body edits, label
// add/remove, open/close, and an optional comment. Fields left nil/empty are
// unchanged, so callers touch only what they intend to.
type UpdateWorkItemRequest struct {
	Repository   RepositoryRef `json:"repository"`
	ID           string        `json:"id"`
	Title        *string       `json:"title,omitempty"`
	Body         *string       `json:"body,omitempty"`
	AddLabels    []string      `json:"addLabels,omitempty"`
	RemoveLabels []string      `json:"removeLabels,omitempty"`
	// State, when set, opens or closes the item ("open" or "closed").
	State   string `json:"state,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ClaimWorkItemRequest requests a best-effort claiming marker on an item so
// concurrent runs observing the backlog do not double-process it (WF-031, BL-032).
// The runner's lease ledger remains the claim source of truth (BL-005); this marker
// mirrors it for human visibility and cross-run signaling.
type ClaimWorkItemRequest struct {
	Repository RepositoryRef `json:"repository"`
	ID         string        `json:"id"`
	// RunID identifies the claiming run; it is written into the claim breadcrumb so
	// exactly one run is recognized as the winner under a race.
	RunID string `json:"runId"`
	// ClaimLabel overrides the claiming label; defaults to LabelClaimed.
	ClaimLabel string `json:"claimLabel,omitempty"`
}

// ClaimResult reports the outcome of a claim attempt.
type ClaimResult struct {
	// Claimed is true when RunID is the recognized winner of the claim.
	Claimed bool `json:"claimed"`
	// ClaimedBy is the run id of the recognized winner (may differ from RunID when
	// another run claimed first).
	ClaimedBy string   `json:"claimedBy,omitempty"`
	Item      WorkItem `json:"item"`
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
