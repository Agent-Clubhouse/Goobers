package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// adoCompletionPollInterval/adoCompletionPollAttempts bound
// MergePullRequest's internal wait for ADO's asynchronous completion job
// to reach a terminal mergeStatus (CONF-3 #2076) — unlike GitHub's
// synchronous merge endpoint, ADO's "complete" PATCH only requests
// completion; the actual merge runs as a background job. A direct
// (non-queued) landing implies DetectMergePolicy already found no
// blocking policies, so the job is expected to settle well under this
// budget.
const (
	adoCompletionPollInterval = 2 * time.Second
	adoCompletionPollAttempts = 10
)

// getPullRequestDetail GETs pullID's live detail — the shared read
// MergePullRequest/EnqueuePullRequest/PollMergeQueueEntry (CONF-3 #2076)
// all start from, so each sees the same status/mergeStatus/
// autoCompleteSetBy fields PollPullRequest's own inline GET already reads.
func (p *ADOProvider) getPullRequestDetail(ctx context.Context, repo RepositoryRef, pullID string) (adoPullRequestDetail, error) {
	if pullID == "" {
		return adoPullRequestDetail{}, errPullIDRequired
	}
	endpoint, err := p.repoURL(repo, "pullrequests", pullID)
	if err != nil {
		return adoPullRequestDetail{}, err
	}
	var out adoPullRequestDetail
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return adoPullRequestDetail{}, err
	}
	return out, nil
}

// branchPolicyConfigurations fetches every policy configuration for repo's
// project. DetectMergePolicy (CONF-3 #2076) filters the result client-side
// to the scopes that actually apply to a specific branch.
func (p *ADOProvider) branchPolicyConfigurations(ctx context.Context, repo RepositoryRef) ([]adoPolicyConfiguration, error) {
	endpoint, err := joinURL(p.BaseURL, p.Organization, p.project(repo), "_apis", "policy", "configurations")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"api-version": []string{"7.1"}})
	if err != nil {
		return nil, err
	}
	var out adoPolicyConfigurationsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return out.Value, nil
}

// DetectMergePolicy reports req.Branch's active merge policy (CONF-3
// #2076, design doc §4: pr.landing.detect-policy ≙ branch policies on the
// target ref) from ADO's policy configurations: any enabled, non-deleted,
// blocking policy scoped to req.Branch means completion must go through
// auto-complete (pr.landing.enqueue) so ADO's own policy-evaluation queue
// gates the merge; no such policy means an immediate completion (pr.merge)
// is safe. This mirrors GitHub's DetectMergePolicy (branch rules ->
// merge_queue rule present) with ADO's own policy model substituted for
// rulesets — ADO has no literal merge-queue concept, so "policy-gated"
// stands in for it. A policy config with an empty scope list (no
// repository/ref restriction at all) is treated as not applying to a
// specific branch, matching how ADO's UI always requires a scope when a
// branch policy is created. A read, so it does not emit a mutation event.
func (p *ADOProvider) DetectMergePolicy(ctx context.Context, req RepoMergePolicyRequest) (RepoMergePolicyResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return RepoMergePolicyResult{}, err
	}
	if req.Branch == "" {
		return RepoMergePolicyResult{}, fmt.Errorf("branch is required")
	}
	configs, err := p.branchPolicyConfigurations(ctx, req.Repository)
	if err != nil {
		return RepoMergePolicyResult{}, err
	}
	targetRef := "refs/heads/" + strings.TrimPrefix(req.Branch, "refs/heads/")
	repoID := req.Repository.ID
	for _, c := range configs {
		if !c.IsEnabled || !c.IsBlocking || c.IsDeleted {
			continue
		}
		for _, scope := range c.Settings.Scope {
			if scope.RepositoryID != "" && repoID != "" && scope.RepositoryID != repoID {
				continue
			}
			if scope.RefName != "" && scope.RefName != targetRef {
				continue
			}
			return RepoMergePolicyResult{Policy: MergePolicyMergeQueue}, nil
		}
	}
	return RepoMergePolicyResult{Policy: MergePolicyDirect}, nil
}

// EnqueuePullRequest requests Azure DevOps auto-complete on a pull request
// (CONF-3 #2076, design doc §4: pr.landing.enqueue ≙ set auto-complete
// with policy satisfaction as the completion condition). ADO's completion
// job then waits for every blocking policy DetectMergePolicy already found
// before landing, mirroring GitHub's merge-queue semantics without a
// literal queue concept. Idempotent (§4's obligation): a pull request that
// is already completed, or already has auto-complete armed, is observed
// rather than re-PATCHed — the second call never duplicates the mutation.
func (p *ADOProvider) EnqueuePullRequest(ctx context.Context, req EnqueuePullRequestRequest) (EnqueuePullRequestResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return EnqueuePullRequestResult{}, err
	}
	if req.PullID == "" {
		return EnqueuePullRequestResult{}, errPullIDRequired
	}
	if req.MergeMethod != "" && !req.MergeMethod.IsValid() {
		return EnqueuePullRequestResult{}, fmt.Errorf("unsupported merge method %q", req.MergeMethod)
	}
	detail, err := p.getPullRequestDetail(ctx, req.Repository, req.PullID)
	if err != nil {
		return EnqueuePullRequestResult{}, err
	}
	if strings.EqualFold(detail.Status, "completed") {
		// The queue's own completion job landed it already — a genuine
		// "merged" outcome (mirrors GitHub's identical Merged=true case),
		// not a re-attempted enqueue.
		return EnqueuePullRequestResult{Number: detail.PullRequestID, Merged: true, MergeSHA: detail.LastMergeCommit.CommitID}, nil
	}
	if detail.AutoCompleteSetBy != nil {
		return EnqueuePullRequestResult{Number: detail.PullRequestID}, nil
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID)
	if err != nil {
		return EnqueuePullRequestResult{}, err
	}
	// lastMergeSourceCommit is read-only on ADO's PR-update endpoint (it rejects
	// any attempt to set it), so the head pin is enforced by comparing the freshly
	// fetched detail — a fetch→compare→PATCH window ADO's API forces, unlike
	// GitHub's server-enforced expected_head_sha. An empty commit id means ADO has
	// not computed the merge preview yet (mergeStatus "notSet"): there is nothing
	// to compare against, and auto-complete re-evaluates the source at completion
	// time, so the pin is asserted only once ADO has resolved the source commit.
	if req.ExpectedHeadSHA != "" && detail.LastMergeSourceCommit.CommitID != "" &&
		!strings.EqualFold(detail.LastMergeSourceCommit.CommitID, req.ExpectedHeadSHA) {
		return EnqueuePullRequestResult{}, fmt.Errorf("pull request head moved to %s, expected %s", detail.LastMergeSourceCommit.CommitID, req.ExpectedHeadSHA)
	}
	body := map[string]interface{}{
		"autoCompleteSetBy": map[string]string{"id": detail.CreatedBy.ID},
		"completionOptions": adoCompletionOptions{
			MergeStrategy: adoMergeStrategy(req.MergeMethod),
		},
	}
	var out adoPullRequestDetail
	if err := p.do(ctx, http.MethodPatch, endpoint, body, &out); err != nil {
		return EnqueuePullRequestResult{}, err
	}
	if strings.EqualFold(out.Status, "completed") {
		return EnqueuePullRequestResult{Number: out.PullRequestID, Merged: true, MergeSHA: out.LastMergeCommit.CommitID}, nil
	}
	return EnqueuePullRequestResult{Number: out.PullRequestID}, nil
}

// PollMergeQueueEntry reports whether a pull request previously enqueued
// via EnqueuePullRequest has since merged or been evicted (CONF-3 #2076,
// design doc §4: pr.landing.poll ≙ PR status completed / auto-complete-
// cleared / policy-rejection). ADO's policy-rejection and auto-complete-
// cleared cases both surface identically — active with no
// AutoCompleteSetBy — mapping to the same Evicted outcome GitHub's queue
// eviction reports, so merge-review's repass loop stays provider-neutral;
// pr.landing.poll is the sole landed-oracle, exactly as it is for GitHub.
func (p *ADOProvider) PollMergeQueueEntry(ctx context.Context, req PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PollMergeQueueEntryResult{}, err
	}
	if req.PullID == "" {
		return PollMergeQueueEntryResult{}, errPullIDRequired
	}
	detail, err := p.getPullRequestDetail(ctx, req.Repository, req.PullID)
	if err != nil {
		return PollMergeQueueEntryResult{}, err
	}
	labels := make([]string, 0, len(detail.Labels))
	for _, l := range detail.Labels {
		labels = append(labels, l.Name)
	}
	switch {
	case strings.EqualFold(detail.Status, "completed"):
		return PollMergeQueueEntryResult{State: MergeQueueEntryMerged, MergeSHA: detail.LastMergeCommit.CommitID, Labels: labels}, nil
	case strings.EqualFold(detail.Status, "abandoned"):
		return PollMergeQueueEntryResult{State: MergeQueueEntryEvicted, Labels: labels}, nil
	case detail.AutoCompleteSetBy != nil:
		return PollMergeQueueEntryResult{State: MergeQueueEntryPending, Labels: labels}, nil
	default:
		// Active, no auto-complete armed: ADO cleared it (policy
		// rejection, or a human/other automation cleared it manually) —
		// the same Evicted outcome as a GitHub queue eviction (§4).
		return PollMergeQueueEntryResult{State: MergeQueueEntryEvicted, Labels: labels}, nil
	}
}

// awaitMergeCompletion polls pullID's live detail until its completion job
// (started by MergePullRequest's status=completed PATCH) reaches a
// terminal mergeStatus, bounded by adoCompletionPollAttempts (CONF-3
// #2076): unlike GitHub's synchronous merge endpoint, ADO's completion
// runs as a background job, so the PATCH response itself often still
// reports mergeStatus "queued".
func (p *ADOProvider) awaitMergeCompletion(ctx context.Context, repo RepositoryRef, pullID string, current adoPullRequestDetail) (adoPullRequestDetail, error) {
	for attempt := 0; ; attempt++ {
		switch strings.ToLower(current.MergeStatus) {
		case "succeeded":
			return current, nil
		case "conflicts":
			return adoPullRequestDetail{}, fmt.Errorf("ado: pull request %s has merge conflicts: %w", pullID, ErrMergeConflict)
		case "failure", "rejectedbypolicy":
			return adoPullRequestDetail{}, fmt.Errorf("ado: pull request %s completion failed: mergeStatus=%s", pullID, current.MergeStatus)
		}
		if strings.EqualFold(current.Status, "completed") {
			return current, nil
		}
		if attempt >= adoCompletionPollAttempts {
			return adoPullRequestDetail{}, fmt.Errorf("ado: pull request %s completion did not reach a terminal state within %d attempts (mergeStatus=%s)", pullID, adoCompletionPollAttempts, current.MergeStatus)
		}
		if err := p.sleep(ctx, adoCompletionPollInterval); err != nil {
			return adoPullRequestDetail{}, err
		}
		next, err := p.getPullRequestDetail(ctx, repo, pullID)
		if err != nil {
			return adoPullRequestDetail{}, err
		}
		current = next
	}
}

// MergePullRequest completes a pull request without auto-complete (CONF-3
// #2076, design doc §4: pr.merge ≙ complete without auto-complete) —
// called only once the caller has independently verified every merge
// conjunct AND DetectMergePolicy reported MergePolicyDirect (no blocking
// policy stood in the way), mirroring MergePullRequestRequest's own
// provider-neutral contract. Idempotent (§4's obligation): a pull request
// that is already completed is observed rather than re-PATCHed.
func (p *ADOProvider) MergePullRequest(ctx context.Context, req MergePullRequestRequest) (MergePullRequestResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return MergePullRequestResult{}, err
	}
	if req.PullID == "" {
		return MergePullRequestResult{}, errPullIDRequired
	}
	if req.MergeMethod != "" && !req.MergeMethod.IsValid() {
		return MergePullRequestResult{}, fmt.Errorf("unsupported merge method %q", req.MergeMethod)
	}
	detail, err := p.getPullRequestDetail(ctx, req.Repository, req.PullID)
	if err != nil {
		return MergePullRequestResult{}, err
	}
	if strings.EqualFold(detail.Status, "completed") {
		return MergePullRequestResult{Number: detail.PullRequestID, Merged: true, MergeSHA: detail.LastMergeCommit.CommitID}, nil
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID)
	if err != nil {
		return MergePullRequestResult{}, err
	}
	// lastMergeSourceCommit is read-only on ADO's PR-update endpoint (it rejects
	// any attempt to set it), so the head pin is enforced by comparing the freshly
	// fetched detail — a fetch→compare→PATCH window ADO's API forces, unlike
	// GitHub's server-enforced expected_head_sha. An empty commit id means ADO has
	// not computed the merge preview yet (mergeStatus "notSet"): there is nothing
	// to compare against, so the pin is asserted only once ADO has resolved the
	// source commit (a residual gap on the direct-merge path tracked separately).
	if req.ExpectedHeadSHA != "" && detail.LastMergeSourceCommit.CommitID != "" &&
		!strings.EqualFold(detail.LastMergeSourceCommit.CommitID, req.ExpectedHeadSHA) {
		return MergePullRequestResult{}, fmt.Errorf("pull request head moved to %s, expected %s", detail.LastMergeSourceCommit.CommitID, req.ExpectedHeadSHA)
	}
	body := map[string]interface{}{
		"status": "completed",
		"completionOptions": adoCompletionOptions{
			MergeStrategy:      adoMergeStrategy(req.MergeMethod),
			MergeCommitMessage: req.CommitMessage,
		},
	}
	var out adoPullRequestDetail
	if err := p.do(ctx, http.MethodPatch, endpoint, body, &out); err != nil {
		return MergePullRequestResult{}, err
	}
	final, err := p.awaitMergeCompletion(ctx, req.Repository, req.PullID, out)
	if err != nil {
		return MergePullRequestResult{}, err
	}
	p.recordMutation(ctx, "pr", req.PullID, "merge")
	return MergePullRequestResult{Number: final.PullRequestID, Merged: true, MergeSHA: final.LastMergeCommit.CommitID}, nil
}

// CompareCommits reports the common ancestor and file-level diff between
// base and head (CONF-3 #2076, design doc §4: pr.compare ≙ diffs commits
// API) via ADO's commit-diffs endpoint, whose response already carries the
// common (merge-base) commit directly — no separate merge-base lookup
// needed, unlike GitHub's compare endpoint which conflates the two. Single
// page (adoChangePageSize items): the diffs/commits endpoint's own
// continuation contract for very large diffs is unconfirmed against a live
// ADO instance pending CONF-4's fixture corpus, so an oversized diff is
// truncated rather than guessed at.
func (p *ADOProvider) CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error) {
	if err := requireRepo(repo); err != nil {
		return CompareResult{}, err
	}
	if base == "" || head == "" {
		return CompareResult{}, fmt.Errorf("base and head are both required")
	}
	endpoint, err := p.repoURL(repo, "diffs", "commits")
	if err != nil {
		return CompareResult{}, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"baseVersion":       []string{base},
		"baseVersionType":   []string{"commit"},
		"targetVersion":     []string{head},
		"targetVersionType": []string{"commit"},
		"$top":              []string{strconv.Itoa(adoChangePageSize)},
	})
	if err != nil {
		return CompareResult{}, err
	}
	var out adoCommitDiffsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return CompareResult{}, err
	}
	files := make([]ChangedFile, 0, len(out.Changes))
	for _, change := range out.Changes {
		if change.Item.IsFolder {
			continue
		}
		files = append(files, ChangedFile{
			Path:      strings.TrimPrefix(change.Item.Path, "/"),
			Status:    adoChangedFileStatus(change.ChangeType),
			Integrity: apiintegrity.Unapproved,
		})
	}
	return CompareResult{MergeBaseSHA: out.CommonCommit, Files: files, Integrity: apiintegrity.Unapproved}, nil
}

// adoCompletionOptions selects how ADO's completion job lands a pull
// request's commits — the request-body counterpart to
// MergePullRequestRequest.MergeMethod/CommitMessage (CONF-3 #2076).
type adoCompletionOptions struct {
	MergeStrategy      string `json:"mergeStrategy,omitempty"`
	DeleteSourceBranch bool   `json:"deleteSourceBranch,omitempty"`
	MergeCommitMessage string `json:"mergeCommitMessage,omitempty"`
}

// adoMergeStrategy maps the provider-neutral MergeMethod onto ADO's
// completionOptions.mergeStrategy enum ("noFastForward"/"squash"/"rebase").
// ADO has no third-parent "merge" analog to GitHub's plain merge commit;
// noFastForward (always create a merge commit) is the closest fit and is
// also ADO's own completion default.
func adoMergeStrategy(m MergeMethod) string {
	switch m {
	case MergeMethodSquash:
		return "squash"
	case MergeMethodRebase:
		return "rebase"
	default:
		return "noFastForward"
	}
}

// adoPolicyConfiguration is one project-scoped branch policy (required
// reviewers, build validation, status checks, …) as ADO's Policy
// Configurations API reports it — the read DetectMergePolicy (CONF-3
// #2076, design doc §4) uses to tell a policy-gated branch (must land via
// auto-complete/pr.landing.enqueue) from a policy-free one (safe to
// complete immediately/pr.merge).
type adoPolicyConfiguration struct {
	IsEnabled  bool `json:"isEnabled"`
	IsBlocking bool `json:"isBlocking"`
	IsDeleted  bool `json:"isDeleted"`
	Settings   struct {
		Scope []struct {
			RepositoryID string `json:"repositoryId"`
			RefName      string `json:"refName"`
		} `json:"scope"`
	} `json:"settings"`
}

type adoPolicyConfigurationsResponse struct {
	Value []adoPolicyConfiguration `json:"value"`
}

// adoCommitDiffsResponse is ADO's diffs/commits response (CONF-3 #2076,
// CompareCommits): CommonCommit is the merge-base the neutral CompareResult
// contract needs, computed server-side rather than requiring a second call.
type adoCommitDiffsResponse struct {
	CommonCommit string `json:"commonCommit"`
	Changes      []struct {
		ChangeType string `json:"changeType"`
		Item       struct {
			Path     string `json:"path"`
			IsFolder bool   `json:"isFolder"`
		} `json:"item"`
	} `json:"changes"`
}
