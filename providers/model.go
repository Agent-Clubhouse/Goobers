package providers

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/labelpredicate"
)

// ProviderKind identifies a concrete provider backend.
type ProviderKind string

// Provider kinds supported by the provider abstraction.
const (
	ProviderGitHub ProviderKind = "github"
	ProviderADO    ProviderKind = "ado"
	ProviderGitea  ProviderKind = "gitea"
)

// Goobers marker labels applied to backlog items. The claim label mirrors the
// runner's lease for human visibility (BL-032); the ready label is the curated
// marker meaning an item is scoped and eligible for implementation; the
// needs-human label parks an item pending a human decision (the curator's
// existing vocabulary — #539's convention; also applied when a stage reports
// blocked, #544).
const (
	LabelApproved   = "goobers:approved"
	LabelClaimed    = "goobers:claimed"
	LabelReady      = "goobers:ready"
	LabelCritical   = "goobers:critical"
	LabelNeedsHuman = "goobers:needs-human"
	LabelNominated  = "goobers:nominated"
	LabelAutoClose  = "goobers:auto-close"
	LabelStale      = "stale"
	LabelTracking   = "tracking"
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
	Provider       ProviderKind           `json:"provider"`
	ID             string                 `json:"id"`
	ExternalID     string                 `json:"externalId,omitempty"`
	Revision       string                 `json:"revision,omitempty"`
	Type           string                 `json:"type,omitempty"`
	Title          string                 `json:"title"`
	Body           string                 `json:"body,omitempty"`
	Labels         []string               `json:"labels,omitempty"`
	State          string                 `json:"state,omitempty"`
	Status         WorkItemStatus         `json:"status,omitempty"`
	Assignee       string                 `json:"assignee,omitempty"`
	Links          []Link                 `json:"links,omitempty"`
	Parent         *WorkItemRef           `json:"parent,omitempty"`
	Hierarchy      map[string]interface{} `json:"hierarchy,omitempty"`
	URL            string                 `json:"url,omitempty"`
	CreatedAt      *time.Time             `json:"createdAt,omitempty"`
	UpdatedAt      *time.Time             `json:"updatedAt,omitempty"`
	ReadyAt        *time.Time             `json:"readyAt,omitempty"`
	Fields         fieldpredicate.Fields  `json:"fields,omitempty"`
	BlockedByCount int                    `json:"-"`
	Raw            interface{}            `json:"raw,omitempty"`
	Integrity      apiintegrity.Grade     `json:"integrity,omitempty"`
}

// WorkItemLabel describes a provider-native issue label.
type WorkItemLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description,omitempty"`
}

// EnsureWorkItemLabelsResult reports which labels were created or already present.
type EnsureWorkItemLabelsResult struct {
	Created []string `json:"created"`
	Skipped []string `json:"skipped"`
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

// IntegrityForLabels classifies task text at the SEC-047 admission boundary
// using the trust label configured by the consuming workflow.
func IntegrityForLabels(labels []string, trustLabel string) apiintegrity.Grade {
	if trustLabel == "" {
		return apiintegrity.Unapproved
	}
	for _, label := range labels {
		if label == trustLabel {
			return apiintegrity.Maintainer
		}
	}
	return apiintegrity.Unapproved
}

// WorkItemLabelTransition is one provider-issued label add/remove event.
type WorkItemLabelTransition struct {
	EventID    int64     `json:"eventId"`
	ItemID     string    `json:"itemId"`
	Label      string    `json:"label"`
	Added      bool      `json:"added"`
	OccurredAt time.Time `json:"occurredAt"`
}

// Comment is a comment on a backlog work item (a GitHub issue comment).
type Comment struct {
	ID         string             `json:"id"`
	Author     string             `json:"author,omitempty"`
	AuthorType string             `json:"authorType,omitempty"`
	Body       string             `json:"body"`
	CreatedAt  *time.Time         `json:"createdAt,omitempty"`
	URL        string             `json:"url,omitempty"`
	Integrity  apiintegrity.Grade `json:"integrity,omitempty"`
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

// CanonicalKey returns the provider-complete identity of a repository, for use
// as the key of durable records. Owner and name alone are not an identity:
// they repeat across providers, across Azure DevOps projects, and across
// self-hosted service URLs, so keying durable state on them alone lets one
// repository suppress or satisfy another's records (#3649). Every field that
// distinguishes one provider repository from another therefore participates,
// with the service URL reduced to its host so that equivalent spellings of the
// same service agree.
func (r RepositoryRef) CanonicalKey() string {
	return strings.Join([]string{
		normalizeRepositoryIdentity(string(r.Provider)),
		repositoryServiceHost(r.URL),
		normalizeRepositoryIdentity(r.Project),
		normalizeRepositoryIdentity(r.Owner),
		normalizeRepositoryIdentity(r.Name),
		normalizeRepositoryIdentity(r.ID),
	}, "|")
}

// SameRepository reports whether two references identify the same provider
// repository under the canonical identity.
func SameRepository(left, right RepositoryRef) bool {
	return left.CanonicalKey() == right.CanonicalKey()
}

func normalizeRepositoryIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func repositoryServiceHost(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return strings.ToLower(parsed.Host)
	}
	return strings.ToLower(raw)
}

// CreateRepositoryRequest describes a new provider repository.
type CreateRepositoryRequest struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// CreateRepositoryResult identifies a newly created repository.
type CreateRepositoryResult struct {
	Repository RepositoryRef `json:"repository"`
	Visibility string        `json:"visibility"`
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

// ListBranchesRequest selects a bounded, stable page of remote branches.
type ListBranchesRequest struct {
	Repository RepositoryRef `json:"repository"`
	Prefix     string        `json:"prefix"`
	After      string        `json:"after,omitempty"`
	Limit      int           `json:"limit"`
}

// BranchSummary is the remote ref identity and activity needed for branch reconciliation.
type BranchSummary struct {
	Name           string     `json:"name"`
	SHA            string     `json:"sha"`
	URL            string     `json:"url,omitempty"`
	LastActivityAt *time.Time `json:"lastActivityAt,omitempty"`
}

// DeleteBranchRequest identifies a remote branch ref to remove.
type DeleteBranchRequest struct {
	Repository  RepositoryRef `json:"repository"`
	Name        string        `json:"name"`
	ExpectedSHA string        `json:"expectedSha,omitempty"`
}

// DeleteBranchResult reports whether the branch existed and was deleted.
type DeleteBranchResult struct {
	Deleted bool `json:"deleted"`
}

// BranchTipChangedError reports that a conditional branch deletion lost its
// lease because the remote ref no longer points at the expected commit.
type BranchTipChangedError struct {
	Name        string
	ExpectedSHA string
}

func (e *BranchTipChangedError) Error() string {
	return fmt.Sprintf("branch %q no longer points at expected SHA %s", e.Name, e.ExpectedSHA)
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

// PullRequestReviewRequest describes a provider-native review verdict. CommitSHA
// is required so the review is attached to exactly the head the reviewer saw;
// branch-protection stale-dismissal can then invalidate it after a new push.
type PullRequestReviewRequest struct {
	Repository RepositoryRef  `json:"repository"`
	PullID     string         `json:"pullId"`
	CommitSHA  string         `json:"commitSha"`
	Decision   ReviewDecision `json:"decision"`
	Body       string         `json:"body"`
}

// PullRequestReviewResult describes the submitted provider-native review.
type PullRequestReviewResult struct {
	ID        int64          `json:"id"`
	URL       string         `json:"url,omitempty"`
	CommitSHA string         `json:"commitSha"`
	Decision  ReviewDecision `json:"decision"`
}

// PullRequestNativeReview is one provider-native review submitted on a pull request.
type PullRequestNativeReview struct {
	ID          int64              `json:"id"`
	Author      string             `json:"author,omitempty"`
	State       string             `json:"state"`
	Body        string             `json:"body,omitempty"`
	CommitSHA   string             `json:"commitSha,omitempty"`
	SubmittedAt *time.Time         `json:"submittedAt,omitempty"`
	URL         string             `json:"url,omitempty"`
	Integrity   apiintegrity.Grade `json:"integrity,omitempty"`
}

// PullRequestInlineComment is one inline review comment with its thread state
// and file/diff anchor preserved.
type PullRequestInlineComment struct {
	ID                int64              `json:"id"`
	ThreadID          string             `json:"threadId"`
	Author            string             `json:"author,omitempty"`
	Body              string             `json:"body"`
	Path              string             `json:"path"`
	Line              int                `json:"line,omitempty"`
	OriginalLine      int                `json:"originalLine,omitempty"`
	Side              string             `json:"side,omitempty"`
	StartLine         int                `json:"startLine,omitempty"`
	OriginalStartLine int                `json:"originalStartLine,omitempty"`
	StartSide         string             `json:"startSide,omitempty"`
	DiffHunk          string             `json:"diffHunk,omitempty"`
	InReplyTo         int64              `json:"inReplyTo,omitempty"`
	IsResolved        bool               `json:"isResolved"`
	IsOutdated        bool               `json:"isOutdated"`
	CreatedAt         *time.Time         `json:"createdAt,omitempty"`
	URL               string             `json:"url,omitempty"`
	Integrity         apiintegrity.Grade `json:"integrity,omitempty"`
}

// PullRequestReviewThreadReply requests a reply to one native review thread.
type PullRequestReviewThreadReply struct {
	Repository RepositoryRef
	PullID     string
	CommentID  int64
	Body       string
}

// PullRequestReviewThreads is the review evidence attached to a pull request.
type PullRequestReviewThreads struct {
	Reviews        []PullRequestNativeReview  `json:"reviews"`
	InlineComments []PullRequestInlineComment `json:"inlineComments"`
	Integrity      apiintegrity.Grade         `json:"integrity"`
}

// CheckState normalizes combined CI/check status across providers (BL-031).
type CheckState string

// Check states a PR poll can report, driving the CI-poll/repass loop.
const (
	CheckStatePending CheckState = "pending"
	CheckStatePassing CheckState = "passing"
	CheckStateFailing CheckState = "failing"
)

// MergeableStateUnstable is GitHub's mergeable_state value meaning the PR is
// mergeable and the only failing or pending checks are NON-required (advisory /
// continue-on-error). The merge gate treats it as CI-ready so a red advisory
// check never blocks a merge (#961); every other state falls through to the
// conservative check-state gate.
const MergeableStateUnstable = "unstable"

// CheckDetail references a single check/status run for repass-gate drill-down.
type CheckDetail struct {
	Name       string     `json:"name"`
	State      CheckState `json:"state"`
	Conclusion string     `json:"conclusion,omitempty"`
	URL        string     `json:"url"`
	Summary    string     `json:"summary"`
}

// CheckAnnotation is one provider-native diagnostic attached to a check run.
type CheckAnnotation struct {
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	Level     string `json:"level,omitempty"`
	Title     string `json:"title,omitempty"`
	Message   string `json:"message"`
}

// CIFailureDetail carries the bounded evidence available for one failing
// status or check run. Legacy commit statuses do not support annotations.
type CIFailureDetail struct {
	CheckDetail
	Annotations []CheckAnnotation  `json:"annotations"`
	Integrity   apiintegrity.Grade `json:"integrity,omitempty"`
}

// PullRequestComment is a normalized issue-thread comment on a pull request.
type PullRequestComment struct {
	ID        int64              `json:"id"`
	Author    string             `json:"author,omitempty"`
	Body      string             `json:"body,omitempty"`
	URL       string             `json:"url,omitempty"`
	CreatedAt time.Time          `json:"createdAt"`
	Integrity apiintegrity.Grade `json:"integrity,omitempty"`
}

// PullRequestPollRequest describes a PR poll operation.
type PullRequestPollRequest struct {
	Repository    RepositoryRef `json:"repository"`
	PullID        string        `json:"pullId"`
	CommentsSince *time.Time    `json:"commentsSince,omitempty"`
	// HumanPolicyConfigurationIDs names the branch/required-check policies the
	// agent loop cannot fix by re-implementing — human/merge-time policies such
	// as merge strategy, required reviewers, comment resolution, or
	// proof-of-presence. A provider excludes these from the gating set it
	// reduces to CheckState, so a rejection there does NOT drive the fail branch
	// (which would loop forever on a policy only a human can satisfy). The
	// values are provider-interpreted opaque identities: the Azure DevOps
	// provider matches them against branch-policy *configuration* ids. Empty
	// means every required blocking policy gates (fail-closed default); loops
	// with human-only policies declare them here as configuration.
	HumanPolicyConfigurationIDs []string `json:"humanPolicyConfigurationIds,omitempty"`
}

// PullRequestPollResult is the deterministic stage-output envelope a repass gate
// branches on: mergeability, review decision, combined check state + failure
// detail refs, and comments since the last poll (BL-031). Draft, labels,
// HeadSHA, and BaseSHA are live signals a conjunctive auto-merge action
// re-checks before acting on a previously computed verdict — never trust a
// caller-supplied "still valid" claim, always re-poll (design doc D6).
type PullRequestPollResult struct {
	Number             int        `json:"number"`
	Title              string     `json:"title,omitempty"`
	Author             string     `json:"author,omitempty"`
	Assignees          []string   `json:"assignees,omitempty"`
	RequestedReviewers []string   `json:"requestedReviewers,omitempty"`
	State              string     `json:"state"`
	Merged             bool       `json:"merged"`
	MergedAt           *time.Time `json:"mergedAt,omitempty"`
	Mergeable          *bool      `json:"mergeable,omitempty"`
	Draft              bool       `json:"draft"`
	Labels             []string   `json:"labels,omitempty"`
	// HeadBranch and HeadRepository identify the PR's head branch and where
	// it actually lives — can differ from the pull request repository for
	// fork pull requests (#605's post-merge cleanup needs this to delete
	// the correct branch).
	HeadBranch     string         `json:"headBranch,omitempty"`
	HeadRepository *RepositoryRef `json:"headRepository,omitempty"`
	HeadSHA        string         `json:"headSha,omitempty"`
	BaseSHA        string         `json:"baseSha,omitempty"`
	// BaseBranch is the target branch name (e.g. "main") — distinct from
	// BaseSHA (a pinned commit): issue #361's post-merge fan-out needs the
	// branch name to find OTHER open PRs targeting the same base, not the
	// commit the SHA-pin checks against.
	BaseBranch string `json:"baseBranch,omitempty"`
	// Body is the PR's description text — issue #361's post-merge close-out
	// parses it for a GitHub closing keyword ("Fixes #N"/"Closes #N"/
	// "Resolves #N", the same convention `goobers open-pr` writes) to find
	// the backlog issue a merged PR belongs to.
	Body             string         `json:"body,omitempty"`
	ReviewDecision   ReviewDecision `json:"reviewDecision"`
	RequestedChanges int            `json:"requestedChanges"`
	CheckState       CheckState     `json:"checkState"`
	Checks           []CheckDetail  `json:"checks,omitempty"`
	// MergeableState is the provider's own advisory-aware mergeability verdict,
	// where it supplies one (GitHub's mergeable_state enum; empty otherwise).
	// Unlike CheckState — which this codebase derives from raw check-runs and so
	// cannot tell a required check from an advisory/continue-on-error one —
	// MergeableStateUnstable specifically means the PR is mergeable and every
	// failing/pending check is NON-required, so a red advisory check must not
	// gate a merge (#961). The provider's determination (rulesets + branch
	// protection) is authoritative for that distinction.
	MergeableState string               `json:"mergeableState,omitempty"`
	CommentsSince  []PullRequestComment `json:"commentsSince,omitempty"`
	URL            string               `json:"url,omitempty"`
	Integrity      apiintegrity.Grade   `json:"integrity,omitempty"`
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

// PullRequestStatusRequest publishes a provider-native pull-request status
// (Azure DevOps PR status) that a repository's status-check branch policy can
// gate on. It is the seam by which goobers publishes its own evidence — an
// agentic reviewer verdict or a local-CI result — into the provider's policy
// engine so the validation loop can prove PR correctness against that repo's
// required policies (#772).
type PullRequestStatusRequest struct {
	Repository  RepositoryRef `json:"repository"`
	PullID      string        `json:"pullId"`
	Genre       string        `json:"genre,omitempty"`
	Name        string        `json:"name"`
	State       CheckState    `json:"state"`
	Description string        `json:"description,omitempty"`
	TargetURL   string        `json:"targetUrl,omitempty"`
}

// PullRequestStatusResult reports the published status's provider-assigned id.
type PullRequestStatusResult struct {
	ID int `json:"id"`
}

// UpdateBranchRequest asks a provider to incorporate the current base branch
// into a pull request's head branch. ExpectedHeadSHA is mandatory optimistic
// concurrency: the update must be refused if the head moved after selection.
type UpdateBranchRequest struct {
	Repository      RepositoryRef `json:"repository"`
	PullID          string        `json:"pullId"`
	ExpectedHeadSHA string        `json:"expectedHeadSha"`
}

// UpdateBranchResult reports an accepted pull request branch update.
type UpdateBranchResult struct {
	Number  int    `json:"number"`
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
}

// MergeMethod controls how a provider incorporates a pull request's commits.
type MergeMethod string

// Supported pull request merge methods.
const (
	MergeMethodMerge  MergeMethod = "merge"
	MergeMethodSquash MergeMethod = "squash"
	MergeMethodRebase MergeMethod = "rebase"
)

// IsValid reports whether m is a supported pull request merge method.
func (m MergeMethod) IsValid() bool {
	switch m {
	case MergeMethodMerge, MergeMethodSquash, MergeMethodRebase:
		return true
	default:
		return false
	}
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
	// CommitTitle, if set, overrides GitHub's default merge-commit title.
	CommitTitle string `json:"commitTitle,omitempty"`
	// CommitMessage, if set, overrides GitHub's default merge-commit message.
	CommitMessage string `json:"commitMessage,omitempty"`
	// MergeMethod, if set, selects merge, squash, or rebase semantics.
	MergeMethod MergeMethod `json:"mergeMethod,omitempty"`
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

// MergePolicy identifies how a repo lands an approved pull request (issue
// #758): direct-merge calls the merge API as today; merge-queue-enqueue adds
// the pull request to the repo's merge queue instead, deferring the actual
// merge to GitHub's own queue processing. Different target repos run
// different policies (the V1 milestone's arbitrary-repos mandate), so
// merge-review/pr-remediation/auto-merge must dispatch on this rather than
// hardcode which one is in effect — see internal/mergepolicy.
type MergePolicy string

// Supported merge policies. A third policy (e.g. a different provider's
// queue equivalent) is a new value here plus a new internal/mergepolicy
// Lander implementation, not a change to either existing one.
const (
	MergePolicyDirect     MergePolicy = "direct"
	MergePolicyMergeQueue MergePolicy = "merge_queue"
)

// RepoMergePolicyRequest asks a provider to detect a repo's active merge
// policy for req.Branch (the target/base branch a pull request lands on)
// from its live branch protection/ruleset state — detected, never
// statically configured, so a repo that later flips its GitHub settings
// (e.g. #631/#759 enabling merge queue on Goobers' own repo) is picked up
// without a code or config change.
type RepoMergePolicyRequest struct {
	Repository RepositoryRef `json:"repository"`
	Branch     string        `json:"branch"`
}

// RepoMergePolicyResult reports req.Branch's detected merge policy.
type RepoMergePolicyResult struct {
	Policy MergePolicy `json:"policy"`
}

// TokenScopeStatus reports whether a provider could introspect its own
// token's granted scopes (issue #916). GitHub only exposes this for classic
// PATs (the X-OAuth-Scopes response header) — fine-grained PATs and GitHub
// App installation tokens never send it. Per the accepted V1 contract, an
// unavailable scope is reported as such and never inferred from a failed
// call.
type TokenScopeStatus string

// Supported token-scope introspection outcomes.
const (
	TokenScopeAvailable   TokenScopeStatus = "available"
	TokenScopeUnavailable TokenScopeStatus = "unavailable"
)

// RepoPolicyRequest asks a provider to report req.Branch's live
// conformance-relevant settings (issue #916, Tier 4 of #903) — the facts
// `goobers doctor --repo` diffs against a declared instance-config manifest.
type RepoPolicyRequest struct {
	Repository RepositoryRef `json:"repository"`
	Branch     string        `json:"branch"`
}

// RepoPolicyResult reports req.Branch's live conformance-relevant settings.
// AllowedMergeMethods lists every merge method the repo currently permits
// (a repo-level setting, not branch-scoped); a single-entry list is the
// repo's de facto required method, matching the "squash-only ruleset
// conflicting with the requested merge method" scenario (#877) the issue
// cites. MergeQueuePolicy reuses DetectMergePolicy's enum. RequiredStatusChecks
// lists Branch's required-status-checks rule contexts, empty when none is
// configured.
type RepoPolicyResult struct {
	AllowedMergeMethods  []MergeMethod    `json:"allowedMergeMethods"`
	MergeQueuePolicy     MergePolicy      `json:"mergeQueuePolicy"`
	RequiredStatusChecks []string         `json:"requiredStatusChecks"`
	TokenScope           TokenScopeStatus `json:"tokenScope"`
	TokenScopes          []string         `json:"tokenScopes,omitempty"`
}

// EnqueuePullRequestRequest adds a pull request to its repo's merge queue
// (issue #758) — the enqueue-policy counterpart to MergePullRequestRequest.
// Like MergePullRequest, this performs no conjunct checking of its own; the
// caller (internal/mergepolicy's Land, driven by the same poll->decide
// closure mergepr.go already runs) is responsible for verifying every merge
// conjunct first.
type EnqueuePullRequestRequest struct {
	Repository RepositoryRef `json:"repository"`
	PullID     string        `json:"pullId"`
	// ExpectedHeadSHA, if set, is the same optimistic-concurrency guard
	// MergePullRequestRequest passes to the provider — GitHub's enqueue
	// mutation spells it expectedHeadOid, and it serves the identical
	// purpose: refuse to land a head commit the caller's merge conjuncts
	// were never checked against.
	ExpectedHeadSHA string `json:"expectedHeadSha,omitempty"`
	// MergeMethod is the same merge-method selection
	// MergePullRequestRequest carries. It is provider-neutral and remains
	// meaningful for a backend whose enqueue operation accepts one, but
	// GitHub ignores it: a merge queue takes its method from the
	// repository ruleset's merge_queue rule, not from the enqueue call,
	// and GitHub's enqueue mutation has no such field (issue #882). It
	// stays required on the direct-merge path, which is what #877 fixed.
	MergeMethod MergeMethod `json:"mergeMethod,omitempty"`
}

// EnqueuePullRequestResult reports the outcome of an enqueue attempt.
// Merged=true means the pull request was ALREADY merged when the enqueue
// was attempted — a retried stage attempt whose pull request the queue
// landed in the meantime — which internal/mergepolicy's enqueueLander maps
// back to Outcome=merged rather than mis-reporting "enqueued" for a pull
// request that is, in fact, already landed. A successful enqueue never
// reports Merged=true: queuing a pull request never merges it inline, so
// the caller polls the queue entry (PollMergeQueueEntryRequest) for the
// terminal outcome.
type EnqueuePullRequestResult struct {
	Number   int    `json:"number"`
	Merged   bool   `json:"merged"`
	MergeSHA string `json:"mergeSha,omitempty"`
	Message  string `json:"message,omitempty"`
}

// DequeuePullRequestRequest removes a pull request that a caller previously
// observed in GitHub's merge queue. PullRequestNodeID comes from
// PollMergeQueueEntryResult so the dequeue is one mutation with no intervening
// lookup race.
type DequeuePullRequestRequest struct {
	Repository        RepositoryRef `json:"repository"`
	PullID            string        `json:"pullId"`
	PullRequestNodeID string        `json:"pullRequestNodeId"`
}

// MergeQueueEntryState normalizes the possible outcomes of watching a pull
// request already enqueued via EnqueuePullRequest resolve (issue #758).
type MergeQueueEntryState string

// Merge queue entry states a poll can report.
const (
	// MergeQueueEntryPending: still in the queue, no terminal outcome yet.
	MergeQueueEntryPending MergeQueueEntryState = "pending"
	// MergeQueueEntryMerged: the queue merged the pull request.
	MergeQueueEntryMerged MergeQueueEntryState = "merged"
	// MergeQueueEntryEvicted: the queue removed the pull request without
	// merging it (its combined build against the projected merge state
	// failed) — a first-class, explicit outcome (issue #758's acceptance
	// criterion), never silently conflated with "still pending" or left for
	// a generic failure path to bury.
	MergeQueueEntryEvicted MergeQueueEntryState = "evicted"
	// MergeQueueEntryAbsent: the pull request is open and unmerged, and has
	// no merge queue entry at all. For a pull request the caller enqueued
	// itself, this is what a real eviction looks like — GitHub leaves an
	// evicted pull request OPEN and simply removes its entry, so there is
	// no closed state to detect (issue #885). It is reported separately
	// from Evicted rather than folded into it because it is also, briefly,
	// what the moments between a successful enqueue and the entry becoming
	// visible look like: only the caller knows whether it has already seen
	// an entry for this pull request, so only the caller can tell those
	// two apart.
	MergeQueueEntryAbsent MergeQueueEntryState = "absent"
)

// PollMergeQueueEntryRequest polls the live state of a pull request the
// caller has already enqueued (EnqueuePullRequest), to learn whether the
// queue has since merged or evicted it.
type PollMergeQueueEntryRequest struct {
	Repository RepositoryRef `json:"repository"`
	PullID     string        `json:"pullId"`
}

// PollMergeQueueEntryResult reports a pull request's current merge-queue
// entry state.
type PollMergeQueueEntryResult struct {
	State MergeQueueEntryState `json:"state"`
	// PullRequestNodeID is the provider-native identity required to remove
	// this pull request from GitHub's merge queue.
	PullRequestNodeID string `json:"pullRequestNodeId,omitempty"`
	// Labels are read in the same live query as the queue entry so a watcher
	// can honor an opt-out without a second, racy provider read.
	Labels []string `json:"labels,omitempty"`
	// MergeSHA, on a merged outcome, is the commit that actually landed on
	// the base branch — never the pull request's head SHA, which under the
	// squash method a merge queue requires is a different commit entirely.
	MergeSHA string `json:"mergeSha,omitempty"`
	// QueueState and QueuePosition carry the provider's own view of a
	// pending entry (for GitHub: QUEUED/AWAITING_CHECKS/MERGEABLE/
	// UNMERGEABLE/LOCKED, and the entry's place in line) so a poll's
	// progress is legible in logs rather than an opaque "still pending".
	QueueState    string `json:"queueState,omitempty"`
	QueuePosition int    `json:"queuePosition,omitempty"`
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
	// Author, Assignee, and RequestedReviewer are exact provider identities.
	// They are applied client-side to the metadata returned by each provider.
	Author            string `json:"author,omitempty"`
	Assignee          string `json:"assignee,omitempty"`
	RequestedReviewer string `json:"requestedReviewer,omitempty"`
	// Limit caps the raw provider records inspected before client-side filters.
	// Zero preserves the exhaustive-list behavior.
	Limit int `json:"limit,omitempty"`
	// Page starts a bounded raw-provider scan at this one-based page. It is
	// ignored when Limit is zero.
	Page int `json:"page,omitempty"`
	// SkipCheckState leaves each summary's CheckState empty instead of
	// resolving it per candidate — resolving costs two extra API requests
	// per PR (combined status + check-runs), which dominates the list's
	// API budget as the open-PR set grows (issue #523: 21 open PRs =
	// 40+ requests per gather, every merge-review cycle). Opt-in so the
	// callers that gate on CheckState (pr-select's eligibility filter)
	// keep their fresh-by-default behavior; a caller that sets this owns
	// resolving check state itself (RefCheckState) for the candidates it
	// actually needs it for. #605's stacked-PR guard is exactly such a
	// caller — it only needs to know whether an open PR exists, not its
	// check state, so it sets this too rather than duplicating the knob.
	SkipCheckState bool `json:"skipCheckState,omitempty"`
}

// MatchesIdentityFields applies the request's opt-in identity filters to one
// pull request decoded from a provider list response.
func (r ListPullRequestsRequest) MatchesIdentityFields(author string, assignees, requestedReviewers []string) bool {
	return (r.Author == "" || author == r.Author) &&
		(r.Assignee == "" || slices.Contains(assignees, r.Assignee)) &&
		(r.RequestedReviewer == "" || slices.Contains(requestedReviewers, r.RequestedReviewer))
}

// PullRequestSummary is one PR as merge-review's selection stage sees it —
// enough to filter eligibility (draft, labels, CI) without a second round-trip
// per candidate. ListPullRequests returns open PRs; bounded terminal-PR queries
// also populate State and Merged for consumers that need current sibling state.
type PullRequestSummary struct {
	ID                 string   `json:"id"`
	Number             int      `json:"number"`
	URL                string   `json:"url"`
	Author             string   `json:"author,omitempty"`
	Assignees          []string `json:"assignees,omitempty"`
	RequestedReviewers []string `json:"requestedReviewers,omitempty"`
	State              string   `json:"state"`
	Merged             bool     `json:"merged"`
	Head               string   `json:"head"`
	Base               string   `json:"base"`
	HeadSHA            string   `json:"headSha"`
	BaseSHA            string   `json:"baseSha"`
	// MergeSHA is the landed commit GitHub reports for a merged PR. It is
	// empty for open/unmerged PRs and providers that do not expose one.
	MergeSHA   string     `json:"mergeSha,omitempty"`
	Draft      bool       `json:"draft"`
	Labels     []string   `json:"labels,omitempty"`
	CheckState CheckState `json:"checkState"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	// Body is the PR's description text — issue #414's open-PR eligibility
	// backstop parses it for a GitHub closing keyword ("Fixes #N"/"Closes
	// #N"/"Resolves #N", the same convention `goobers open-pr` writes and
	// `goobers post-merge` already parses via PullRequestPollResult.Body) to
	// tell whether a candidate backlog issue already has an open PR, without
	// a second round-trip per candidate (GitHub's list-pulls response
	// already carries the body).
	Body      string             `json:"body,omitempty"`
	Integrity apiintegrity.Grade `json:"integrity,omitempty"`
}

// ChangedFile is one file a pull request touches (issue #359's sibling-set
// context: what does the OTHER open PR touch, for cross-PR conflict/drift
// detection).
type ChangedFile struct {
	Path string `json:"path"`
	// PreviousPath is populated for renames so consumers can correlate
	// conflicts reported under either side of the move.
	PreviousPath string `json:"previousPath,omitempty"`
	Status       string `json:"status"` // added|modified|removed|renamed
	Additions    int    `json:"additions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
	// Patch is the file's unified-diff hunk text as the provider reports it
	// (GitHub omits this for binary files and diffs over its size cutoff —
	// empty in that case, not an error). Issue #718: the rebase-invariant
	// component of the merge-review verdict cache key is derived from this
	// text (with hunk line-numbers normalized out), not from the PR's raw
	// head SHA, so a clean rebase — which changes the head SHA but not the
	// actual patch content — still produces a cache hit.
	Patch     string             `json:"patch,omitempty"`
	Integrity apiintegrity.Grade `json:"integrity,omitempty"`
}

// CompareResult is the result of comparing two commits/refs: their common
// ancestor plus the file-level diff between base and head (issue #718 —
// the merge-base is what lets a caller compute "what changed on base SINCE
// this PR's own merge-base," the delta-aware replacement for a raw base-SHA
// comparison).
type CompareResult struct {
	MergeBaseSHA string             `json:"mergeBaseSha"`
	Files        []ChangedFile      `json:"files"`
	Integrity    apiintegrity.Grade `json:"integrity,omitempty"`
}

// ListWorkItemsRequest filters backlog items for scheduler admission.
type ListWorkItemsRequest struct {
	Repository     RepositoryRef             `json:"repository"`
	Labels         []string                  `json:"labels,omitempty"`
	LabelPredicate *labelpredicate.Predicate `json:"-"`
	FieldPredicate *fieldpredicate.Predicate `json:"-"`
	State          string                    `json:"state,omitempty"`
	Assignee       string                    `json:"assignee,omitempty"`
	// UpdatedSince, when set, restricts results to items updated at or after it.
	UpdatedSince *time.Time `json:"updatedSince,omitempty"`
	Limit        int        `json:"limit,omitempty"`
	// Page selects a 1-based page for stable pagination; 0 means the first page.
	Page int `json:"page,omitempty"`
	// Cursor resumes a caller-driven bounded scan. Its provider-specific value
	// comes from PageInfo.NextCursor.
	Cursor string `json:"cursor,omitempty"`
	// PageInfo receives raw-candidate pagination metadata. Providers populate it
	// before applying exact predicates or dropping non-work-item API records.
	PageInfo *ListWorkItemsPageInfo `json:"-"`
	// OldestFirst, when set, asks the provider to return items in creation
	// order (oldest filed first) rather than its own default. This matters
	// whenever Limit truncates the result set: a FIFO consumer (#532) must
	// have truncation drop the NEWEST items — the ones that stay reachable
	// once older ones drain — not the oldest, which GitHub's undocumented
	// newest-first default would otherwise starve forever.
	OldestFirst bool `json:"oldestFirst,omitempty"`
}

// ListWorkItemsPageInfo describes one bounded raw-candidate provider window.
type ListWorkItemsPageInfo struct {
	CandidateCount int
	HasNext        bool
	NextCursor     string
}

// MatchesLabelPredicate applies the request's exact client-side label filter.
func (r ListWorkItemsRequest) MatchesLabelPredicate(labels []string) (bool, error) {
	return r.LabelPredicate.Matches(labels)
}

// MatchesFieldPredicate applies the request's exact client-side native-field filter.
func (r ListWorkItemsRequest) MatchesFieldPredicate(fields fieldpredicate.Fields) (bool, error) {
	return r.FieldPredicate.Matches(fields)
}

// candidateScanCeiling bounds a bounded/caller-paged ListWorkItems scan's
// raw candidate fetch when NeedsOversizedCandidateScan reports a post-fetch
// filter is active (#2067) — the provider-layer counterpart to
// cmd/goobers/backlogquery.go's identical backlogScanCeiling, one layer up.
const candidateScanCeiling = 250

// NeedsOversizedCandidateScan reports whether r carries a filter that is
// NEVER expressible in either provider's native bounded-scan query, so a
// caller-paged fetch capped at exactly r.Limit raw candidates risks
// under-matching (#2067): the candidate at the truncation boundary might be
// one the filter rejects, even though a real match exists further into the
// true result set — silently breaking the contract a caller reasonably
// assumes ("Limit = up to N matches"). LabelPredicate (CEL-based label
// logic beyond a simple AND-of-tags) and FieldPredicate are such filters on
// BOTH providers — GitHub's own `labels`/`state` query params and ADO's
// WIQL `CONTAINS`/state-equality clauses reliably pre-filter the simple
// Labels/State fields server-side, so those two do NOT belong here; ADO's
// state normalization and req.Labels/hasAllLabels DO still need an
// oversized scan (WIQL CONTAINS is a substring match, and process-specific
// state categories can't be WIQL-compared directly) — ADOProvider.
// ListWorkItems adds those as its own extra, ADO-only conditions rather
// than folding them into this shared check, which both providers' bounded
// paths call identically.
func (r ListWorkItemsRequest) NeedsOversizedCandidateScan() bool {
	// IsZero, not a bare nil check: Compile("") / Compile("", nil, nil)
	// return a non-nil Predicate that still matches unconditionally, so a
	// caller (e.g. cmd/goobers/backlogquery.go) that always compiles a
	// FieldPredicate from a possibly-empty input string must not be treated
	// as "a real filter is active" just because the pointer is non-nil.
	return !r.LabelPredicate.IsZero() || !r.FieldPredicate.IsZero()
}

// UpdateWorkItemRequest is a general backlog item edit: title/body edits, assignee
// changes, label add/remove, open/close, milestone assignment, and an optional
// comment. Nil pointer fields are unchanged; a non-nil empty Assignee clears the
// current assignment.
type UpdateWorkItemRequest struct {
	Repository RepositoryRef `json:"repository"`
	ID         string        `json:"id"`
	// ExpectedRevision, when set, rejects the edit if the item changed after
	// the caller's immediately preceding read.
	ExpectedRevision string   `json:"expectedRevision,omitempty"`
	Title            *string  `json:"title,omitempty"`
	Body             *string  `json:"body,omitempty"`
	Assignee         *string  `json:"assignee,omitempty"`
	AddLabels        []string `json:"addLabels,omitempty"`
	RemoveLabels     []string `json:"removeLabels,omitempty"`
	// Milestone, when set, assigns an existing provider milestone by number.
	Milestone *int `json:"milestone,omitempty"`
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
	// LedgerAuthorized permits release to reconcile a provider marker left by a
	// historical run. Callers set it only after verifying that RunID currently
	// owns the authoritative ledger lease.
	LedgerAuthorized bool `json:"ledgerAuthorized,omitempty"`
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

// AttachWorkItemChildRequest describes a provider-native parent/child link.
// ExpectedParentRevision and ExpectedChildRevision are optimistic-concurrency
// guards obtained immediately before the mutation.
type AttachWorkItemChildRequest struct {
	Repository             RepositoryRef `json:"repository"`
	ParentID               string        `json:"parentId"`
	ChildID                string        `json:"childId"`
	ExpectedParentRevision string        `json:"expectedParentRevision"`
	ExpectedChildRevision  string        `json:"expectedChildRevision"`
}

// AttachWorkItemBlockerRequest describes a provider-native blocked-by link.
type AttachWorkItemBlockerRequest struct {
	Repository              RepositoryRef `json:"repository"`
	ItemID                  string        `json:"itemId"`
	BlockerID               string        `json:"blockerId"`
	ExpectedItemRevision    string        `json:"expectedItemRevision"`
	ExpectedBlockerRevision string        `json:"expectedBlockerRevision"`
}

// RevisionConflictError reports that a mutation's immediately observed item
// revision no longer matches the caller's guard.
type RevisionConflictError struct {
	ItemID   string
	Expected string
	Actual   string
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("work item %s revision changed: expected %q, got %q", e.ItemID, e.Expected, e.Actual)
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
