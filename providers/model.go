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
	// WorkItemStatusInReview means a PR implementing this item is open and
	// cycling through merge-review — the item is neither still being
	// implemented (in-progress) nor actually done (issue #361/#355): the
	// work isn't done until the PR merges. `implementation`'s close-out
	// stage sets this at PR-open time instead of WorkItemStatusDone; only
	// the merge event (`goobers post-merge`) advances it to done.
	WorkItemStatusInReview WorkItemStatus = "in-review"
	WorkItemStatusDone     WorkItemStatus = "done"
	WorkItemStatusClosed   WorkItemStatus = "closed"
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
	// RunID, if set, is stamped as a breadcrumb footer on the PR body so the run
	// journal (once #8 lands) can link the PR URL back to the run (bidirectional trace).
	RunID string `json:"runId,omitempty"`
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

// ReviewDecision normalizes provider-native review verdicts (BL-031).
type ReviewDecision string

// Review decisions a PR poll can report.
const (
	ReviewDecisionPending          ReviewDecision = "pending"
	ReviewDecisionApproved         ReviewDecision = "approved"
	ReviewDecisionChangesRequested ReviewDecision = "changes_requested"
)

// CheckState normalizes combined CI/check status across providers (BL-031).
type CheckState string

// Check states a PR poll can report, driving the CI-poll/repass loop.
const (
	CheckStatePending CheckState = "pending"
	CheckStatePassing CheckState = "passing"
	CheckStateFailing CheckState = "failing"
)

// CheckDetail references a single check/status run for repass-gate drill-down.
type CheckDetail struct {
	Name    string     `json:"name"`
	State   CheckState `json:"state"`
	URL     string     `json:"url,omitempty"`
	Summary string     `json:"summary,omitempty"`
}

// PullRequestComment is a normalized issue-thread comment on a pull request.
type PullRequestComment struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body,omitempty"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// PullRequestPollRequest describes a PR poll operation.
type PullRequestPollRequest struct {
	Repository    RepositoryRef `json:"repository"`
	PullID        string        `json:"pullId"`
	CommentsSince *time.Time    `json:"commentsSince,omitempty"`
}

// PullRequestPollResult is the deterministic stage-output envelope a repass gate
// branches on: mergeability, review decision, combined check state + failure
// detail refs, and comments since the last poll (BL-031). Draft/HeadSHA/BaseSHA
// (issue #360) are the live signal a conjunctive auto-merge action re-checks
// against a previously-computed verdict's SHA-pin before acting on it — never
// trust a caller-supplied "still valid" claim, always re-poll (design doc D6).
type PullRequestPollResult struct {
	Number    int    `json:"number"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Mergeable *bool  `json:"mergeable,omitempty"`
	Draft     bool   `json:"draft"`
	HeadSHA   string `json:"headSha,omitempty"`
	BaseSHA   string `json:"baseSha,omitempty"`
	// BaseBranch is the target branch name (e.g. "main") — distinct from
	// BaseSHA (a pinned commit): issue #361's post-merge fan-out needs the
	// branch name to find OTHER open PRs targeting the same base, not the
	// commit the SHA-pin checks against.
	BaseBranch string `json:"baseBranch,omitempty"`
	// Body is the PR's description text — issue #361's post-merge close-out
	// parses it for a GitHub closing keyword ("Fixes #N"/"Closes #N"/
	// "Resolves #N", the same convention `goobers open-pr` writes) to find
	// the backlog issue a merged PR belongs to.
	Body             string               `json:"body,omitempty"`
	ReviewDecision   ReviewDecision       `json:"reviewDecision"`
	RequestedChanges int                  `json:"requestedChanges"`
	CheckState       CheckState           `json:"checkState"`
	Checks           []CheckDetail        `json:"checks,omitempty"`
	CommentsSince    []PullRequestComment `json:"commentsSince,omitempty"`
	URL              string               `json:"url,omitempty"`
}

// ClosePullRequestRequest describes closing a pull request, optionally leaving a comment.
type ClosePullRequestRequest struct {
	Repository RepositoryRef `json:"repository"`
	PullID     string        `json:"pullId"`
	Comment    string        `json:"comment,omitempty"`
}

// ClosePullRequestResult reports whether the PR was merged or closed unmerged
// (merged-vs-closed detection, BL-031).
type ClosePullRequestResult struct {
	Number int    `json:"number"`
	Merged bool   `json:"merged"`
	State  string `json:"state"`
}

// MergePullRequestRequest describes merging a pull request (issue #360). The
// caller (a conjunctive auto-merge action) is responsible for verifying every
// merge conjunct BEFORE calling this — MergePullRequest itself performs no
// policy check; it is the provider-level primitive the policy sits in front
// of, mirroring how CreateBranch/Commit are pure git-remote operations with
// no workflow-level judgment of their own.
type MergePullRequestRequest struct {
	Repository RepositoryRef `json:"repository"`
	PullID     string        `json:"pullId"`
	// ExpectedHeadSHA, if set, is passed to GitHub's merge API as its own
	// optimistic-concurrency guard (the `sha` merge-body field): the merge is
	// refused server-side if the PR's actual current head has moved past it
	// since the caller last checked — belt-and-suspenders alongside the
	// caller's own SHA-pin re-check (D6).
	ExpectedHeadSHA string `json:"expectedHeadSha,omitempty"`
	// CommitMessage, if set, overrides GitHub's default merge-commit message.
	CommitMessage string `json:"commitMessage,omitempty"`
}

// MergePullRequestResult reports the outcome of a merge attempt. Merged=false
// with a nil error never happens for GitHub (a refused merge — sha mismatch,
// not mergeable, merge blocked by branch protection — is always a non-2xx
// response, surfaced as an error) but the field exists for provider-neutral
// callers that might report a soft refusal instead.
type MergePullRequestResult struct {
	Number   int    `json:"number"`
	Merged   bool   `json:"merged"`
	MergeSHA string `json:"mergeSha,omitempty"`
	Message  string `json:"message,omitempty"`
}

// ListPullRequestsRequest filters open pull requests for merge-review's
// selection stage (issue #359) — the same declarative-selection model
// backlog-query already uses for issues, applied to PRs. Reused as-is by
// #361's post-merge fan-out (find every other open PR targeting the merged
// PR's base branch, to label it needs-remediation).
type ListPullRequestsRequest struct {
	Repository RepositoryRef `json:"repository"`
	// Base restricts to PRs targeting this branch (e.g. "main"); empty
	// means unfiltered.
	Base string `json:"base,omitempty"`
	// HeadPrefix restricts to PRs whose head branch starts with this
	// prefix (e.g. "goobers/", G1's goober-authored-repo assumption) —
	// applied client-side: GitHub's pulls-list API has no server-side
	// prefix match on head, only an exact head=owner:branch filter.
	HeadPrefix string `json:"headPrefix,omitempty"`
}

// PullRequestSummary is one open PR as merge-review's selection stage sees
// it — enough to filter eligibility (draft, labels, CI) without a second
// round-trip per candidate. Reused as-is by #361's post-merge fan-out, which
// only reads Number.
type PullRequestSummary struct {
	ID         string     `json:"id"`
	Number     int        `json:"number"`
	URL        string     `json:"url"`
	Head       string     `json:"head"`
	Base       string     `json:"base"`
	HeadSHA    string     `json:"headSha"`
	BaseSHA    string     `json:"baseSha"`
	Draft      bool       `json:"draft"`
	Labels     []string   `json:"labels,omitempty"`
	CheckState CheckState `json:"checkState"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// ChangedFile is one file a pull request touches (issue #359's sibling-set
// context: what does the OTHER open PR touch, for cross-PR conflict/drift
// detection).
type ChangedFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"` // added|modified|removed|renamed
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
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
	// RunID, when set, makes creation idempotent: a run-id footer is written
	// into the item body and CreateWorkItem searches for an existing item with
	// that marker before POSTing, so a policy retry after a timed-out-but-
	// committed create returns the original rather than filing a duplicate
	// (#140). Optional — empty keeps the plain, non-idempotent create.
	RunID string `json:"runId,omitempty"`
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
