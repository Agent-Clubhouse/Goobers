package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// OpenPullRequest opens a GitHub pull request — idempotent on repass (#132):
// a workflow's open-pr stage reuses the same stable run branch
// (providers.BranchName) on every repass through it, so a second call here
// must find and update the PR it already opened rather than attempting a
// duplicate POST (which GitHub 422s on, since a PR already exists for that
// head/base). Checking first, rather than POSTing and catching the 422, also
// sidesteps this package's lack of a typed HTTP-status error to match against
// (doStatus's non-2xx path returns a plain fmt.Errorf).
func (p *GitHubProvider) OpenPullRequest(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestResult{}, err
	}
	if existing, ok, err := p.FindPullRequestByBranch(ctx, req.Repository, req.Head, req.Base); err != nil {
		return PullRequestResult{}, err
	} else if ok {
		return p.updatePullRequest(ctx, req, existing.Number)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return PullRequestResult{}, err
	}
	prBody := withRunIDFooter(req.Body, req.RunID)
	body := map[string]interface{}{
		"title": req.Title,
		"body":  prBody,
		"head":  req.Head,
		"base":  req.Base,
		"draft": req.Draft,
	}
	var out githubPullRequest
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		if IsPullRequestAlreadyExistsError(err) {
			// #1767: lost a create race against a concurrent open-pr call for
			// this same head/base between the check above and this POST.
			// OpenPullRequest's own doc comment promises convergence on the
			// PR that already exists rather than a duplicate — honor that
			// here instead of surfacing the race as a stage failure.
			if existing, ok, ferr := p.FindPullRequestByBranch(ctx, req.Repository, req.Head, req.Base); ferr == nil && ok {
				return p.updatePullRequest(ctx, req, existing.Number)
			}
		}
		return PullRequestResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, strconv.Itoa(out.Number)),
		URL:       out.HTMLURL,
		Operation: "open",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(req.Title)},
			"body":  {After: digestString(prBody)},
		},
	})
	return PullRequestResult{ID: strconv.Itoa(out.Number), Number: out.Number, URL: out.HTMLURL}, nil
}

// FindPullRequestByBranch looks up an open PR for head/base, returning
// ok=false (not an error) if none exists. Exported (not just OpenPullRequest's
// internal idempotency check) so a caller that already knows a run's stable
// branch name (providers.BranchName) but has no other way to recover that
// run's PR — e.g. `goobers issue-close-out` (#132), which runs as its own
// process several stages after open-pr, with no threaded reference to the PR
// it opened — can rediscover it directly from the provider instead.
func (p *GitHubProvider) FindPullRequestByBranch(ctx context.Context, repo RepositoryRef, head, base string) (PullRequestResult, bool, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls")
	if err != nil {
		return PullRequestResult{}, false, err
	}
	query := url.Values{
		"head":  []string{repo.Owner + ":" + head},
		"state": []string{"open"},
	}
	if base != "" {
		query.Set("base", base)
	}
	endpoint, err = addQuery(endpoint, query)
	if err != nil {
		return PullRequestResult{}, false, err
	}
	var out []githubPullRequest
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return PullRequestResult{}, false, err
	}
	if len(out) == 0 {
		return PullRequestResult{}, false, nil
	}
	pr := out[0]
	return PullRequestResult{ID: strconv.Itoa(pr.Number), Number: pr.Number, URL: pr.HTMLURL}, true, nil
}

// OpenPRSummary is the slim per-PR view the open-PR-count throttle (#353/#986)
// needs: the head-branch ref (to bucket by workflow run-branch namespace) and
// the PR's labels (to exclude human-parked PRs the daemon cannot drain from the
// cap). Deliberately minimal — the throttle never needs the full PR.
type OpenPRSummary struct {
	Head   string
	Labels []string
}

// ListOpenPullRequests returns the head-branch ref and labels of every open PR
// on the repo. Used by the scheduler's open-PR-count throttle (#353) to count
// the loop's own un-merged sibling PRs (those under the goobers/ run-branch
// namespace), excluding human-parked ones (#986), so it can pace dispatch.
// Single page of up to 100 — ample for a dogfood loop; a full paginator is a
// follow-up if a repo ever carries >100 open PRs (at which point the cap has
// long since engaged anyway).
func (p *GitHubProvider) ListOpenPullRequests(ctx context.Context, repo RepositoryRef) ([]OpenPRSummary, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"state":    []string{"open"},
		"per_page": []string{"100"},
	})
	if err != nil {
		return nil, err
	}
	var out []githubPullRequest
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	prs := make([]OpenPRSummary, 0, len(out))
	for _, pr := range out {
		labels := make([]string, 0, len(pr.Labels))
		for _, l := range pr.Labels {
			labels = append(labels, l.Name)
		}
		prs = append(prs, OpenPRSummary{Head: pr.Head.Ref, Labels: labels})
	}
	return prs, nil
}

// updatePullRequest applies title/body edits to an already-open PR (its
// number found by FindPullRequestByBranch) — the repass path: the same run
// branch already has an open PR, so this call updates it in place instead of
// opening a duplicate.
func (p *GitHubProvider) updatePullRequest(ctx context.Context, req PullRequestRequest, existingNumber int) (PullRequestResult, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", strconv.Itoa(existingNumber))
	if err != nil {
		return PullRequestResult{}, err
	}
	prBody := withRunIDFooter(req.Body, req.RunID)
	var out githubPullRequest
	if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"title": req.Title, "body": prBody}, &out); err != nil {
		return PullRequestResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, strconv.Itoa(out.Number)),
		URL:       out.HTMLURL,
		Operation: "update",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(req.Title)},
			"body":  {After: digestString(prBody)},
		},
	})
	return PullRequestResult{ID: strconv.Itoa(out.Number), Number: out.Number, URL: out.HTMLURL}, nil
}

// PollPullRequest reports mergeability, review decision, combined check state,
// and comments-since for a GitHub pull request (BL-031). A read, so it does not
// emit a mutation event.
func (p *GitHubProvider) PollPullRequest(ctx context.Context, req PullRequestPollRequest) (PullRequestPollResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestPollResult{}, err
	}
	if req.PullID == "" {
		return PullRequestPollResult{}, errPullIDRequired
	}
	prEndpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID)
	if err != nil {
		return PullRequestPollResult{}, err
	}
	var pr githubPullRequestDetail
	if err := p.do(ctx, http.MethodGet, prEndpoint, nil, &pr); err != nil {
		return PullRequestPollResult{}, err
	}

	decision, requestedChanges, err := p.reviewDecision(ctx, req.Repository, req.PullID)
	if err != nil {
		return PullRequestPollResult{}, err
	}

	checkState, checks, err := p.combinedCheckState(ctx, req.Repository, pr.Head.SHA)
	if err != nil {
		return PullRequestPollResult{}, err
	}

	comments, err := pullRequestComments(ctx, p, p.BaseURL, req.Repository, req.PullID, req.CommentsSince)
	if err != nil {
		return PullRequestPollResult{}, err
	}
	labels := make([]string, 0, len(pr.Labels))
	for _, label := range pr.Labels {
		labels = append(labels, label.Name)
	}
	assignees := githubUserLogins(pr.Assignees)
	requestedReviewers := githubUserLogins(pr.RequestedReviewers)

	return PullRequestPollResult{
		Number:             pr.Number,
		Title:              pr.Title,
		Author:             pr.User.Login,
		Assignees:          assignees,
		RequestedReviewers: requestedReviewers,
		State:              pr.State,
		Merged:             pr.Merged,
		MergedAt:           pr.MergedAt,
		Mergeable:          pr.Mergeable,
		MergeableState:     pr.MergeableState,
		Draft:              pr.Draft,
		Labels:             labels,
		HeadBranch:         pr.Head.Ref,
		HeadRepository:     repositoryRef(ProviderGitHub, pr.Head.Repo),
		HeadSHA:            pr.Head.SHA,
		BaseSHA:            pr.Base.SHA,
		BaseBranch:         pr.Base.Ref,
		Body:               pr.Body,
		ReviewDecision:     decision,
		RequestedChanges:   requestedChanges,
		CheckState:         checkState,
		Checks:             checks,
		CommentsSince:      comments,
		URL:                pr.HTMLURL,
		Integrity:          apiintegrity.Unapproved,
	}, nil
}

// ClosePullRequest closes a GitHub pull request, detecting merged-vs-closed, and
// optionally leaves a comment.
func (p *GitHubProvider) ClosePullRequest(ctx context.Context, req ClosePullRequestRequest) (ClosePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.PullID == "" {
		return ClosePullRequestResult{}, errPullIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID)
	if err != nil {
		return ClosePullRequestResult{}, err
	}
	var out githubPullRequestDetail
	if err := p.do(ctx, http.MethodPatch, endpoint, map[string]string{"state": "closed"}, &out); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.Comment != "" {
		if err := postAttributedComment(ctx, p, p.BaseURL, p.attribution, req.Repository, req.PullID, req.Comment, "pull-request-close"); err != nil {
			return ClosePullRequestResult{}, err
		}
	}
	state := "closed"
	operation := "close"
	if out.Merged {
		state = "merged"
		operation = "merge"
	}
	fields := map[string]FieldDigest{"state": {After: digestString(state)}}
	if req.Comment != "" {
		fields["comment"] = FieldDigest{After: digestString(req.Comment)}
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       out.HTMLURL,
		Operation: operation,
		Fields:    fields,
	})
	return ClosePullRequestResult{Number: out.Number, Merged: out.Merged, State: state}, nil
}

// UpdateBranchError is a typed rejection from GitHub's update-branch endpoint.
// StatusCode distinguishes lease/conflict validation failures (422) from
// permission failures (403) without requiring callers to parse error strings.
type UpdateBranchError struct {
	StatusCode int
	Message    string
}

func (e *UpdateBranchError) Error() string {
	return fmt.Sprintf("update pull request branch failed: status %d: %s", e.StatusCode, e.Message)
}

// UpdateBranch merges a pull request's current base into its head through
// GitHub's native update-branch endpoint. expected_head_sha is always sent:
// omitting the lease would allow a stale selector to update a head it never
// inspected.
func (p *GitHubProvider) UpdateBranch(ctx context.Context, req UpdateBranchRequest) (UpdateBranchResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return UpdateBranchResult{}, err
	}
	if req.PullID == "" {
		return UpdateBranchResult{}, errPullIDRequired
	}
	if req.ExpectedHeadSHA == "" {
		return UpdateBranchResult{}, fmt.Errorf("expected head SHA is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "update-branch")
	if err != nil {
		return UpdateBranchResult{}, err
	}
	resp, err := p.send(ctx, http.MethodPut, endpoint, map[string]string{"expected_head_sha": req.ExpectedHeadSHA})
	if err != nil {
		return UpdateBranchResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return UpdateBranchResult{}, fmt.Errorf("read update-branch response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := strings.TrimSpace(string(body))
		var failure struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Message != "" {
			message = failure.Message
		}
		return UpdateBranchResult{}, &UpdateBranchError{
			StatusCode: resp.StatusCode,
			Message:    message,
		}
	}
	var out struct {
		Message string `json:"message"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return UpdateBranchResult{}, fmt.Errorf("decode update-branch response: %w", err)
	}
	number, _ := strconv.Atoi(req.PullID)
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       out.URL,
		Operation: "update-branch",
		Fields: map[string]FieldDigest{
			"headSha": {Before: digestString(req.ExpectedHeadSHA)},
		},
	})
	return UpdateBranchResult{Number: number, Message: out.Message, URL: out.URL}, nil
}

// MergePullRequest merges a GitHub pull request (issue #360) via the
// dedicated merge endpoint (PUT .../pulls/{number}/merge) — distinct from
// ClosePullRequest's PATCH state=closed, which merely closes without
// merging. GitHub refuses the request server-side (non-2xx, surfaced as an
// error by p.do) if req.ExpectedHeadSHA is set and no longer matches the
// PR's actual current head (405/409), or if the PR is not mergeable at all
// (draft, blocked by branch protection, merge conflict) — this method
// performs no policy check of its own; see MergePullRequestRequest's doc.
func (p *GitHubProvider) MergePullRequest(ctx context.Context, req MergePullRequestRequest) (MergePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return MergePullRequestResult{}, err
	}
	if req.PullID == "" {
		return MergePullRequestResult{}, errPullIDRequired
	}
	if req.MergeMethod != "" && !req.MergeMethod.IsValid() {
		return MergePullRequestResult{}, fmt.Errorf("unsupported merge method %q", req.MergeMethod)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "merge")
	if err != nil {
		return MergePullRequestResult{}, err
	}
	body := map[string]interface{}{}
	if req.ExpectedHeadSHA != "" {
		body["sha"] = req.ExpectedHeadSHA
	}
	if req.CommitTitle != "" {
		body["commit_title"] = req.CommitTitle
	}
	if req.CommitMessage != "" {
		body["commit_message"] = req.CommitMessage
	}
	if req.MergeMethod != "" {
		body["merge_method"] = string(req.MergeMethod)
	}
	var out githubMergeResult
	if err := p.do(ctx, http.MethodPut, endpoint, body, &out); err != nil {
		return MergePullRequestResult{}, err
	}
	number, convErr := strconv.Atoi(req.PullID)
	if convErr != nil {
		number = 0
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		Operation: "merge",
		Fields:    map[string]FieldDigest{"state": {After: digestString("merged")}},
	})
	return MergePullRequestResult{Number: number, Merged: out.Merged, MergeSHA: out.SHA, Message: out.Message}, nil
}

// DetectMergePolicy reports req.Branch's active merge policy (issue #758)
// via GitHub's "get rules for a branch" endpoint (GET .../rules/branches/
// {branch}), which returns every ruleset rule that actually applies to the
// branch, regardless of which ruleset(s) define them. A "merge_queue"-typed
// rule present in that list means GitHub requires the merge queue for this
// branch; its absence means direct-merge (today's behavior, and classic
// branch-protection repos that have no rulesets at all). A read, so it does
// not emit a mutation event.
func (p *GitHubProvider) DetectMergePolicy(ctx context.Context, req RepoMergePolicyRequest) (RepoMergePolicyResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return RepoMergePolicyResult{}, err
	}
	if req.Branch == "" {
		return RepoMergePolicyResult{}, fmt.Errorf("branch is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "rules", "branches", req.Branch)
	if err != nil {
		return RepoMergePolicyResult{}, err
	}
	var rules []githubBranchRule
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &rules); err != nil {
		// The rules endpoint is entitlement-gated: private repos on free
		// plans answer 403 ("Upgrade to ...") even to an admin token. That
		// is a plan limitation, not an auth failure — treating it as one
		// fails every merge and can latch the auth circuit. No readable
		// rules means no merge-queue rule is detectable; degrade to
		// direct-merge and let GitHub remain the enforcer at merge time (a
		// server-side queue requirement still rejects the direct merge
		// loudly there).
		var respErr *providerResponseError
		if errors.As(err, &respErr) && respErr.statusCode == http.StatusForbidden {
			return RepoMergePolicyResult{Policy: MergePolicyDirect}, nil
		}
		return RepoMergePolicyResult{}, err
	}
	for _, rule := range rules {
		if rule.Type == "merge_queue" {
			return RepoMergePolicyResult{Policy: MergePolicyMergeQueue}, nil
		}
	}
	return RepoMergePolicyResult{Policy: MergePolicyDirect}, nil
}

// GetRepoPolicy reports req.Branch's live forge-conformance settings (issue
// #916, Tier 4 of #903): which merge methods the repo allows, whether the
// branch requires GitHub's merge queue, which status checks its rules
// require, and whether this provider's token exposes its own granted
// scopes. It issues two reads: GET .../repos/{owner}/{repo} (repo-level
// merge-method settings, and the X-OAuth-Scopes response header GitHub sends
// only for classic PAT auth) and the same "rules for a branch" endpoint
// DetectMergePolicy uses (merge-queue requirement and required-status-checks
// contexts), so both facts come from one extra round trip rather than a
// second rules fetch.
func (p *GitHubProvider) GetRepoPolicy(ctx context.Context, req RepoPolicyRequest) (RepoPolicyResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return RepoPolicyResult{}, err
	}
	if req.Branch == "" {
		return RepoPolicyResult{}, fmt.Errorf("branch is required")
	}

	repoEndpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name)
	if err != nil {
		return RepoPolicyResult{}, err
	}
	resp, err := p.send(ctx, http.MethodGet, repoEndpoint, nil)
	if err != nil {
		return RepoPolicyResult{}, err
	}
	// GitHub sends X-OAuth-Scopes on any authenticated response for classic
	// PAT auth; fine-grained PATs and GitHub App installation tokens never
	// send it. Read before readJSONResponse closes the body.
	scopeHeader := resp.Header.Get("X-OAuth-Scopes")
	var detail githubRepoDetail
	if err := readJSONResponse(resp, http.MethodGet, repoEndpoint, &detail); err != nil {
		return RepoPolicyResult{}, err
	}

	rulesEndpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "rules", "branches", req.Branch)
	if err != nil {
		return RepoPolicyResult{}, err
	}
	var rules []githubBranchRule
	if err := p.do(ctx, http.MethodGet, rulesEndpoint, nil, &rules); err != nil {
		return RepoPolicyResult{}, err
	}

	result := RepoPolicyResult{
		AllowedMergeMethods: detail.allowedMergeMethods(),
		MergeQueuePolicy:    MergePolicyDirect,
		TokenScope:          TokenScopeUnavailable,
	}
	if scopeHeader != "" {
		result.TokenScope = TokenScopeAvailable
		result.TokenScopes = splitAndTrim(scopeHeader, ",")
	}
	for _, rule := range rules {
		switch rule.Type {
		case "merge_queue":
			result.MergeQueuePolicy = MergePolicyMergeQueue
		case "required_status_checks":
			var parameters githubRequiredStatusChecksParameters
			if len(rule.Parameters) > 0 {
				if err := json.Unmarshal(rule.Parameters, &parameters); err != nil {
					return RepoPolicyResult{}, fmt.Errorf("decode required_status_checks rule: %w", err)
				}
			}
			for _, check := range parameters.RequiredStatusChecks {
				if check.Context != "" {
					result.RequiredStatusChecks = append(result.RequiredStatusChecks, check.Context)
				}
			}
		}
	}
	sort.Strings(result.RequiredStatusChecks)
	return result, nil
}

func splitAndTrim(value, sep string) []string {
	var out []string
	for _, part := range strings.Split(value, sep) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// enqueuePullRequestLookupQuery resolves the GraphQL node ID the enqueue
// mutation requires from the pull request number the rest of the codebase
// carries, and in the same round trip reads the two states that make the
// enqueue a no-op: already merged, and already sitting in the queue.
const enqueuePullRequestLookupQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      id
      merged
      mergeCommit{ oid }
      mergeQueueEntry{ state position }
    }
  }
}`

// enqueuePullRequestMutation is GitHub's only supported way to add a pull
// request to a merge queue. expectedHeadOid is the same optimistic-
// concurrency guard the REST merge endpoint spells "sha".
const enqueuePullRequestMutation = `mutation($pullRequestId:ID!,$expectedHeadOid:GitObjectID){
  enqueuePullRequest(input:{pullRequestId:$pullRequestId,expectedHeadOid:$expectedHeadOid}){
    mergeQueueEntry{ state position }
  }
}`

const dequeuePullRequestMutation = `mutation($pullRequestId:ID!){
  dequeuePullRequest(input:{pullRequestId:$pullRequestId}){
    clientMutationId
  }
}`

// EnqueuePullRequest adds a GitHub pull request to its repo's merge queue
// (issue #758) via the GraphQL enqueuePullRequest mutation.
//
// It previously used the REST merge endpoint (PUT .../pulls/{number}/merge)
// on the assumption that GitHub converts that call into an enqueue when the
// base branch requires a merge queue. That assumption is wrong, and issue
// #882 is the live evidence: against a queue-required branch, with the
// ruleset's own required merge method sent correctly, GitHub rejected the
// call outright — 405 "Repository rule violations found / Changes must be
// made through the merge queue" — rather than queuing anything. There is no
// REST endpoint for this operation at all; the GraphQL mutation is the only
// one, which is why `gh pr merge` reaches for it too.
//
// Two states make the mutation unnecessary, and both are checked first so a
// retried stage attempt is idempotent rather than an error:
//
//   - already merged (the queue landed it between attempts) — reported as
//     Merged=true with the real merge commit, which internal/mergepolicy's
//     enqueueLander maps to Outcome=merged;
//   - already enqueued — reported as a successful no-op enqueue, since the
//     desired end state already holds.
//
// Merged=true on the mutation path is impossible by construction: unlike
// the REST endpoint, enqueueing never merges inline, so a queue with
// nothing ahead of this pull request still yields an entry to poll rather
// than an immediate merge.
//
// req.MergeMethod is deliberately not sent. The queue's merge method comes
// from the repository ruleset's merge_queue rule, not from the enqueue
// call, and the mutation accepts no such field — #877's fix remains correct
// for the direct-merge path and is simply inapplicable here.
func (p *GitHubProvider) EnqueuePullRequest(ctx context.Context, req EnqueuePullRequestRequest) (EnqueuePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return EnqueuePullRequestResult{}, err
	}
	if req.PullID == "" {
		return EnqueuePullRequestResult{}, errPullIDRequired
	}
	number, err := strconv.Atoi(req.PullID)
	if err != nil {
		// The GraphQL lookup takes the number as an Int, so a non-numeric
		// pull id cannot be resolved to a node at all — fail with that
		// reason rather than sending a request GitHub will reject opaquely.
		return EnqueuePullRequestResult{}, fmt.Errorf("pull id %q must be a pull request number: %w", req.PullID, err)
	}

	var lookup struct {
		Repository struct {
			PullRequest *struct {
				ID          string `json:"id"`
				Merged      bool   `json:"merged"`
				MergeCommit *struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
				MergeQueueEntry *struct {
					State    string `json:"state"`
					Position int    `json:"position"`
				} `json:"mergeQueueEntry"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := p.graphql(ctx, enqueuePullRequestLookupQuery, map[string]interface{}{
		"owner":  req.Repository.Owner,
		"name":   req.Repository.Name,
		"number": number,
	}, &lookup); err != nil {
		return EnqueuePullRequestResult{}, err
	}
	pr := lookup.Repository.PullRequest
	if pr == nil || pr.ID == "" {
		return EnqueuePullRequestResult{}, fmt.Errorf("pull request %s/%s#%d not found", req.Repository.Owner, req.Repository.Name, number)
	}

	if pr.Merged {
		mergeSHA := ""
		if pr.MergeCommit != nil {
			mergeSHA = pr.MergeCommit.OID
		}
		return EnqueuePullRequestResult{
			Number:   number,
			Merged:   true,
			MergeSHA: mergeSHA,
			Message:  "pull request is already merged",
		}, nil
	}
	if pr.MergeQueueEntry != nil {
		p.recordEnqueue(ctx, req.Repository, req.PullID)
		return EnqueuePullRequestResult{
			Number:  number,
			Message: fmt.Sprintf("pull request is already enqueued (state %s, position %d)", pr.MergeQueueEntry.State, pr.MergeQueueEntry.Position),
		}, nil
	}

	variables := map[string]interface{}{"pullRequestId": pr.ID}
	if req.ExpectedHeadSHA != "" {
		variables["expectedHeadOid"] = req.ExpectedHeadSHA
	}
	var mutation struct {
		EnqueuePullRequest struct {
			MergeQueueEntry *struct {
				State    string `json:"state"`
				Position int    `json:"position"`
			} `json:"mergeQueueEntry"`
		} `json:"enqueuePullRequest"`
	}
	if err := p.graphql(ctx, enqueuePullRequestMutation, variables, &mutation); err != nil {
		return EnqueuePullRequestResult{}, err
	}

	p.recordEnqueue(ctx, req.Repository, req.PullID)
	message := "pull request enqueued"
	if entry := mutation.EnqueuePullRequest.MergeQueueEntry; entry != nil {
		message = fmt.Sprintf("pull request enqueued (state %s, position %d)", entry.State, entry.Position)
	}
	return EnqueuePullRequestResult{Number: number, Message: message}, nil
}

// recordEnqueue journals the enqueue as a mutation of the pull request's
// external ref, so a queued-but-not-yet-merged pull request is as visible
// in the run journal as a merged one.
func (p *GitHubProvider) recordEnqueue(ctx context.Context, repo RepositoryRef, pullID string) {
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(repo, pullID),
		Operation: "enqueue",
		Fields:    map[string]FieldDigest{"state": {After: digestString("enqueued")}},
	})
}

// DequeuePullRequest removes a pull request from GitHub's merge queue.
func (p *GitHubProvider) DequeuePullRequest(ctx context.Context, req DequeuePullRequestRequest) error {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return err
	}
	if req.PullID == "" {
		return errPullIDRequired
	}
	if req.PullRequestNodeID == "" {
		return fmt.Errorf("pull request node id is required")
	}
	var mutation struct {
		DequeuePullRequest struct {
			ClientMutationID string `json:"clientMutationId"`
		} `json:"dequeuePullRequest"`
	}
	if err := p.graphql(ctx, dequeuePullRequestMutation, map[string]interface{}{
		"pullRequestId": req.PullRequestNodeID,
	}, &mutation); err != nil {
		return err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		Operation: "dequeue",
		Fields:    map[string]FieldDigest{"state": {Before: digestString("enqueued"), After: digestString("dequeued")}},
	})
	return nil
}

// pollMergeQueueEntryQuery reads the pull request's own state and its live
// merge queue entry in one round trip. The entry is the only surface that
// distinguishes "still queued" from "no longer queued" — REST exposes
// nothing equivalent.
const pollMergeQueueEntryQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      id
      state
      merged
      mergeCommit{ oid }
      mergeQueueEntry{ state position }
      labels(first:100){ nodes{ name } }
    }
  }
}`

// PollMergeQueueEntry reports whether the merge queue has since merged or
// evicted a pull request previously enqueued via EnqueuePullRequest (issue
// #758), by reading the pull request's live merge queue entry over GraphQL.
//
// This previously re-polled the pull request over REST and classified on
// pr.State == "closed" alone. That never fires for a real eviction: GitHub
// leaves an evicted pull request OPEN and simply removes its queue entry,
// so every eviction reported Pending until the caller's poll timed out, and
// mergeQueuePollEvicted's goobers:needs-remediation routing — #758's own
// acceptance criterion — could never run (issue #885). The old doc flagged
// exactly this as needing re-validation "once #759 actually enables the
// queue live"; it since has, and REST turned out to be the wrong surface.
//
// Classification:
//
//   - merged: unambiguous, and the merge commit is reported as such. Never
//     the head SHA — under the squash method a merge queue requires, the
//     commit that lands on the base branch is a brand-new SHA that can
//     never equal the pull request's head.
//   - closed without merging: evicted-and-closed, still first-class.
//   - open, unmerged, entry present: pending; the caller's own bounded poll
//     loop keeps watching.
//   - open, unmerged, NO entry: absent. For a pull request the caller
//     enqueued, that is what an eviction looks like — but it is also what
//     the moments right after a successful enqueue look like, so the
//     distinction is left to the caller, which is the only party that
//     knows whether it has already seen an entry. See
//     MergeQueueEntryAbsent's doc.
//
// A read, so it does not emit a mutation event.
func (p *GitHubProvider) PollMergeQueueEntry(ctx context.Context, req PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PollMergeQueueEntryResult{}, err
	}
	if req.PullID == "" {
		return PollMergeQueueEntryResult{}, errPullIDRequired
	}
	number, err := strconv.Atoi(req.PullID)
	if err != nil {
		return PollMergeQueueEntryResult{}, fmt.Errorf("pull id %q must be a pull request number: %w", req.PullID, err)
	}
	var out struct {
		Repository struct {
			PullRequest *struct {
				ID          string `json:"id"`
				State       string `json:"state"`
				Merged      bool   `json:"merged"`
				MergeCommit *struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
				MergeQueueEntry *struct {
					State    string `json:"state"`
					Position int    `json:"position"`
				} `json:"mergeQueueEntry"`
				Labels struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := p.graphql(ctx, pollMergeQueueEntryQuery, map[string]interface{}{
		"owner":  req.Repository.Owner,
		"name":   req.Repository.Name,
		"number": number,
	}, &out); err != nil {
		return PollMergeQueueEntryResult{}, err
	}
	pr := out.Repository.PullRequest
	if pr == nil {
		return PollMergeQueueEntryResult{}, fmt.Errorf("pull request %s/%s#%d not found", req.Repository.Owner, req.Repository.Name, number)
	}
	result := PollMergeQueueEntryResult{PullRequestNodeID: pr.ID}
	for _, label := range pr.Labels.Nodes {
		result.Labels = append(result.Labels, label.Name)
	}
	if pr.Merged {
		if pr.MergeCommit != nil {
			result.MergeSHA = pr.MergeCommit.OID
		}
		result.State = MergeQueueEntryMerged
		return result, nil
	}
	if strings.EqualFold(pr.State, "closed") {
		result.State = MergeQueueEntryEvicted
		return result, nil
	}
	if pr.MergeQueueEntry == nil {
		result.State = MergeQueueEntryAbsent
		return result, nil
	}
	result.State = MergeQueueEntryPending
	result.QueueState = pr.MergeQueueEntry.State
	result.QueuePosition = pr.MergeQueueEntry.Position
	return result, nil
}

// ListPullRequests lists open pull requests targeting req.Base, filtered
// client-side to those whose head branch starts with req.HeadPrefix —
// merge-review's selection stage and sibling-set context gathering (issue
// #359), and #361's post-merge fan-out (find every other open PR targeting
// the branch a just-merged PR targeted). GitHub's pulls-list API has no
// server-side prefix match on head (only an exact head=owner:branch filter,
// which FindPullRequestByBranch already uses for the single-branch case), so
// the prefix filter is applied here instead. A read, so it does not emit a
// mutation event.
func (p *GitHubProvider) ListPullRequests(ctx context.Context, req ListPullRequestsRequest) ([]PullRequestSummary, error) {
	return p.listPullRequests(ctx, req, "open", time.Time{})
}

// GetPullRequest returns one pull request's current state and metadata without
// resolving reviews, comments, or check runs.
func (p *GitHubProvider) GetPullRequest(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestSummary, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return PullRequestSummary{}, err
	}
	if pullID == "" {
		return PullRequestSummary{}, errPullIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID)
	if err != nil {
		return PullRequestSummary{}, err
	}
	var pr githubPullRequestDetail
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return PullRequestSummary{}, err
	}
	return summarizePullRequest(pr, ""), nil
}

// ListRecentlyClosedPullRequests lists pull requests closed or merged since
// updatedSince. It is the bounded terminal-PR complement to ListPullRequests
// used when a workflow needs current state for recently relevant siblings.
func (p *GitHubProvider) ListRecentlyClosedPullRequests(ctx context.Context, req ListPullRequestsRequest, updatedSince time.Time) ([]PullRequestSummary, error) {
	if updatedSince.IsZero() {
		return nil, fmt.Errorf("updatedSince is required")
	}
	req.SkipCheckState = true
	return p.listPullRequests(ctx, req, "closed", updatedSince)
}

func (p *GitHubProvider) listPullRequests(ctx context.Context, req ListPullRequestsRequest, state string, updatedSince time.Time) ([]PullRequestSummary, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return nil, err
	}
	values := url.Values{"state": []string{state}}
	if req.Base != "" {
		values.Set("base", req.Base)
	}
	if !updatedSince.IsZero() {
		values.Set("sort", "updated")
		values.Set("direction", "desc")
	}
	if req.Limit > 0 {
		values.Set("per_page", strconv.Itoa(min(req.Limit, 100)))
		if req.Page > 1 {
			values.Set("page", strconv.Itoa(req.Page))
		}
	}
	endpoint, err = addQuery(endpoint, values)
	if err != nil {
		return nil, err
	}

	var prs []githubPullRequestDetail
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageOut []githubPullRequestDetail
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode pulls page: %w", err)
		}
		for _, pr := range pageOut {
			if req.Limit > 0 && len(prs) >= req.Limit {
				return errStopPaging
			}
			if !updatedSince.IsZero() && pr.UpdatedAt.Before(updatedSince) {
				return errStopPaging
			}
			if !updatedSince.IsZero() {
				closedRecently := pr.ClosedAt != nil && !pr.ClosedAt.Before(updatedSince)
				mergedRecently := pr.MergedAt != nil && !pr.MergedAt.Before(updatedSince)
				if !closedRecently && !mergedRecently {
					continue
				}
			}
			prs = append(prs, pr)
		}
		if req.Limit > 0 && len(prs) >= req.Limit {
			return errStopPaging
		}
		return nil
	}); err != nil {
		return nil, err
	}

	out := make([]PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		if req.HeadPrefix != "" && !strings.HasPrefix(pr.Head.Ref, req.HeadPrefix) {
			continue
		}
		if !req.MatchesIdentityFields(pr.User.Login, githubUserLogins(pr.Assignees), githubUserLogins(pr.RequestedReviewers)) {
			continue
		}
		var checkState CheckState
		if !req.SkipCheckState {
			checkState, _, err = p.combinedCheckState(ctx, req.Repository, pr.Head.SHA)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, summarizePullRequest(pr, checkState))
	}
	return out, nil
}

func summarizePullRequest(pr githubPullRequestDetail, checkState CheckState) PullRequestSummary {
	labels := githubLabelNames(pr.Labels)
	return PullRequestSummary{
		ID:                 strconv.Itoa(pr.Number),
		Number:             pr.Number,
		URL:                pr.HTMLURL,
		Author:             pr.User.Login,
		Assignees:          githubUserLogins(pr.Assignees),
		RequestedReviewers: githubUserLogins(pr.RequestedReviewers),
		State:              pr.State,
		Merged:             pr.Merged || pr.MergedAt != nil,
		Head:               pr.Head.Ref,
		Base:               pr.Base.Ref,
		HeadSHA:            pr.Head.SHA,
		BaseSHA:            pr.Base.SHA,
		MergeSHA:           pr.MergeCommitSHA,
		Draft:              pr.Draft,
		Labels:             labels,
		CheckState:         checkState,
		UpdatedAt:          pr.UpdatedAt,
		Body:               pr.Body,
		Integrity:          apiintegrity.Unapproved,
	}
}

func githubLabelNames(labels []githubLabel) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names
}

func githubUserLogins(users []githubUser) []string {
	logins := make([]string, 0, len(users))
	for _, user := range users {
		logins = append(logins, user.Login)
	}
	return logins
}

// PullRequestFiles lists the files pullID touches — merge-review's
// sibling-set context gathering (issue #359): what does the OTHER open PR
// change, for cross-PR conflict/drift detection. A read, so it does not
// emit a mutation event.
func (p *GitHubProvider) PullRequestFiles(ctx context.Context, repo RepositoryRef, pullID string) ([]ChangedFile, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, errPullIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "files")
	if err != nil {
		return nil, err
	}
	var files []githubPullRequestFile
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageOut []githubPullRequestFile
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode pull files page: %w", err)
		}
		files = append(files, pageOut...)
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]ChangedFile, 0, len(files))
	for _, f := range files {
		out = append(out, ChangedFile{
			Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch,
			Integrity: apiintegrity.Unapproved,
		})
	}
	return out, nil
}

// RepositoryFileContent returns one file's contents at ref.
func (p *GitHubProvider) RepositoryFileContent(ctx context.Context, repo RepositoryRef, path, ref string) ([]byte, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("file path is required")
	}
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "contents", path)
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"ref": []string{ref}})
	if err != nil {
		return nil, err
	}
	resp, err := p.sendWithAccept(ctx, http.MethodGet, endpoint, nil, "application/vnd.github.raw+json")
	if err != nil {
		return nil, err
	}
	content, _, err := readPage(resp, http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// CompareCommits reports base and head's common ancestor plus the
// file-level diff between them (issue #718) via GitHub's three-dot compare
// endpoint — the same computation GitHub itself performs for a PR's own
// "files" view (pulls/{n}/files is exactly compare(base...head) for that
// PR's current head/base), exposed here for arbitrary ref/SHA pairs so a
// caller can also ask "what changed on base between two points in its own
// history" (merge-review's cache re-keying, merge-pr's delta-aware SHA-pin
// check) without either point being a PR's live head. A read, so it does
// not emit a mutation event.
func (p *GitHubProvider) CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return CompareResult{}, err
	}
	if base == "" || head == "" {
		return CompareResult{}, fmt.Errorf("base and head are both required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "compare", base+"..."+head)
	if err != nil {
		return CompareResult{}, err
	}
	var mergeBaseSHA string
	var files []githubPullRequestFile
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var resp githubCompareResponse
		if err := json.Unmarshal(page, &resp); err != nil {
			return fmt.Errorf("decode compare page: %w", err)
		}
		if mergeBaseSHA == "" {
			mergeBaseSHA = resp.MergeBaseCommit.SHA
		}
		files = append(files, resp.Files...)
		return nil
	}); err != nil {
		return CompareResult{}, err
	}
	out := CompareResult{
		MergeBaseSHA: mergeBaseSHA,
		Files:        make([]ChangedFile, 0, len(files)),
		Integrity:    apiintegrity.Unapproved,
	}
	for _, f := range files {
		out.Files = append(out.Files, ChangedFile{
			Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch,
			Integrity: apiintegrity.Unapproved,
		})
	}
	return out, nil
}

// PullRequestMergeable resolves pullID's current GitHub-computed mergeable
// flag via a single-PR detail GET — issue #715's post-merge triage needs
// exactly this one field per sibling, not the review-decision/check-state/
// comments PollPullRequest also resolves (three extra requests it has no use
// for here). Returns nil when GitHub reports null (mergeability still being
// computed asynchronously — a normal, common state right after a merge
// changes a sibling's target, not an error): the caller must treat "unknown"
// as distinct from "known conflicted", since treating a computing-in-progress
// PR as conflicted would false-positive-label a PR that turns out clean once
// GitHub finishes. A read, so it does not emit a mutation event.
func (p *GitHubProvider) PullRequestMergeable(ctx context.Context, repo RepositoryRef, pullID string) (*bool, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, errPullIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID)
	if err != nil {
		return nil, err
	}
	var pr struct {
		Mergeable *bool `json:"mergeable"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return nil, err
	}
	return pr.Mergeable, nil
}

// reviewDecision aggregates a PR's review list into a single decision: the
// latest review per author wins, and any outstanding CHANGES_REQUESTED beats
// any APPROVED (BL-031 review-decision normalization).
func (p *GitHubProvider) reviewDecision(ctx context.Context, repo RepositoryRef, pullID string) (ReviewDecision, int, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "reviews")
	if err != nil {
		return "", 0, err
	}
	var reviews []githubReview
	// Follow pagination: a CHANGES_REQUESTED review on page 2+ would otherwise
	// be invisible and a truly-blocked PR would read as Approved (#139).
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageItems []githubReview
		if err := json.Unmarshal(page, &pageItems); err != nil {
			return fmt.Errorf("decode reviews page: %w", err)
		}
		reviews = append(reviews, pageItems...)
		return nil
	}); err != nil {
		return "", 0, err
	}
	latest := map[string]string{}
	order := map[string]int{}
	for i, review := range reviews {
		login := review.User.Login
		state := strings.ToUpper(review.State)
		if state == "COMMENTED" {
			continue // comment-only reviews carry no verdict
		}
		if prev, ok := order[login]; !ok || i > prev {
			latest[login] = state
			order[login] = i
		}
	}
	requestedChanges := 0
	approved := false
	for _, state := range latest {
		switch state {
		case "CHANGES_REQUESTED":
			requestedChanges++
		case "APPROVED":
			approved = true
		}
	}
	switch {
	case requestedChanges > 0:
		return ReviewDecisionChangesRequested, requestedChanges, nil
	case approved:
		return ReviewDecisionApproved, 0, nil
	default:
		return ReviewDecisionPending, 0, nil
	}
}

// RefCheckState resolves ref's combined check state on demand — the
// per-candidate resolution ListPullRequests does by default, exposed for
// callers that list with SkipCheckState and then resolve only the
// candidates whose state they cannot reuse from a prior gather (issue
// #523's sibling-context cache). A read, so it does not emit a mutation
// event.
func (p *GitHubProvider) RefCheckState(ctx context.Context, repo RepositoryRef, ref string) (CheckState, error) {
	state, _, err := p.combinedCheckState(ctx, repo, ref)
	return state, err
}

// RefCheckStates resolves combined check state for multiple commit refs in one
// GraphQL request.
func (p *GitHubProvider) RefCheckStates(ctx context.Context, repo RepositoryRef, refs []string) (map[string]CheckState, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	states := make(map[string]CheckState, len(refs))
	if len(refs) == 0 {
		return states, nil
	}

	var definitions, fields strings.Builder
	variables := map[string]interface{}{"owner": repo.Owner, "name": repo.Name}
	for i, ref := range refs {
		variable := fmt.Sprintf("ref%d", i)
		alias := fmt.Sprintf("r%d", i)
		fmt.Fprintf(&definitions, ",$%s:String!", variable)
		fmt.Fprintf(&fields, "%s:object(expression:$%s){... on Commit{statusCheckRollup{state}}}", alias, variable)
		variables[variable] = ref
	}
	query := fmt.Sprintf(
		"query($owner:String!,$name:String!%s){repository(owner:$owner,name:$name){%s}}",
		definitions.String(), fields.String(),
	)
	var response struct {
		Repository map[string]struct {
			StatusCheckRollup *struct {
				State string `json:"state"`
			} `json:"statusCheckRollup"`
		} `json:"repository"`
	}
	if err := p.graphql(ctx, query, variables, &response); err != nil {
		return nil, err
	}
	for i, ref := range refs {
		result, ok := response.Repository[fmt.Sprintf("r%d", i)]
		if !ok || result.StatusCheckRollup == nil {
			states[ref] = CheckStatePending
			continue
		}
		switch result.StatusCheckRollup.State {
		case "SUCCESS":
			states[ref] = CheckStatePassing
		case "FAILURE", "ERROR":
			states[ref] = CheckStateFailing
		default:
			states[ref] = CheckStatePending
		}
	}
	return states, nil
}

type resolvedCheckDetail struct {
	CheckDetail
	checkRunID int64
}

// CIFailures returns failing legacy statuses and check runs for ref. It fetches
// annotations only for failing check runs and never fetches raw workflow logs.
func (p *GitHubProvider) CIFailures(ctx context.Context, repo RepositoryRef, ref string) ([]CIFailureDetail, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	checks, err := p.checkDetails(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	failures := make([]CIFailureDetail, 0)
	for _, check := range checks {
		if check.State != CheckStateFailing {
			continue
		}
		annotations := []CheckAnnotation{}
		if check.checkRunID != 0 {
			annotations, err = p.checkRunAnnotations(ctx, repo, check.checkRunID)
			if err != nil {
				return nil, fmt.Errorf("list annotations for check %q: %w", check.Name, err)
			}
		}
		failures = append(failures, CIFailureDetail{
			CheckDetail: check.CheckDetail,
			Annotations: annotations,
			Integrity:   apiintegrity.Unapproved,
		})
	}
	return failures, nil
}

// combinedCheckState normalizes GitHub's legacy combined status plus check-runs
// into a single CheckState + per-check detail refs (BL-031).
func (p *GitHubProvider) combinedCheckState(ctx context.Context, repo RepositoryRef, ref string) (CheckState, []CheckDetail, error) {
	if ref == "" {
		return CheckStatePending, nil, nil
	}
	resolved, err := p.checkDetails(ctx, repo, ref)
	if err != nil {
		return "", nil, err
	}
	details := make([]CheckDetail, 0, len(resolved))
	failing, pending := false, false
	for _, check := range resolved {
		details = append(details, check.CheckDetail)
		switch check.State {
		case CheckStateFailing:
			failing = true
		case CheckStatePending:
			pending = true
		}
	}
	switch {
	case failing:
		return CheckStateFailing, details, nil
	case pending || len(details) == 0:
		return CheckStatePending, details, nil
	default:
		return CheckStatePassing, details, nil
	}
}

func (p *GitHubProvider) checkDetails(ctx context.Context, repo RepositoryRef, ref string) ([]resolvedCheckDetail, error) {
	var details []resolvedCheckDetail
	statusEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "status")
	if err != nil {
		return nil, err
	}
	var statuses []githubStatus
	if err := p.getAllPages(ctx, statusEndpoint, func(page []byte) error {
		var pageOut githubCombinedStatus
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode combined status page: %w", err)
		}
		statuses = append(statuses, pageOut.Statuses...)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, status := range statuses {
		state := normalizeCombinedStatusState(status.State)
		details = append(details, resolvedCheckDetail{CheckDetail: CheckDetail{
			Name: status.Context, State: state, Conclusion: status.State,
			URL: status.TargetURL, Summary: status.Description,
		}})
	}

	runsEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "check-runs")
	if err != nil {
		return nil, err
	}
	var checkRuns []githubCheckRun
	// The single biggest silent failure in the cluster: a failing check-run on
	// page 2+ would be unseen and the ci-gate would pass a red PR (#139).
	if err := p.getAllPages(ctx, runsEndpoint, func(page []byte) error {
		var pageOut githubCheckRunsResponse
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode check-runs page: %w", err)
		}
		checkRuns = append(checkRuns, pageOut.CheckRuns...)
		return nil
	}); err != nil {
		if !IsForbiddenPATError(err) {
			return nil, err
		}
		// Fine-grained PATs have no grantable "Checks" permission at all, so
		// this specific 403 is permanent, not transient (#2685). If the token
		// has instead been granted "Actions: Read", actions/runs exposes the
		// same success/failure signal for Actions-based CI, so retry there
		// before failing CI visibility closed.
		runs, actionsErr := p.actionsRunsForRef(ctx, repo, ref)
		if actionsErr != nil {
			return nil, fmt.Errorf("check-runs forbidden for fine-grained PAT (%w), actions/runs fallback also failed: %w", err, actionsErr)
		}
		for _, run := range runs {
			state := normalizeCheckRunState(run.Status, run.Conclusion)
			details = append(details, resolvedCheckDetail{CheckDetail: CheckDetail{
				Name: run.Name, State: state, Conclusion: run.Conclusion, URL: run.HTMLURL,
			}})
		}
		return details, nil
	}
	for _, run := range checkRuns {
		state := normalizeCheckRunState(run.Status, run.Conclusion)
		details = append(details, resolvedCheckDetail{
			CheckDetail: CheckDetail{
				Name: run.Name, State: state, Conclusion: run.Conclusion,
				URL: run.HTMLURL, Summary: run.Output.Summary,
			},
			checkRunID: run.ID,
		})
	}
	return details, nil
}

// actionsRunsForRef reads workflow-run conclusions for ref via the Actions
// API (GET .../actions/runs?head_sha=ref) — the fallback CI-state source for
// a fine-grained PAT that can reach Actions runs but not check-runs (#2685).
// It carries the same success/failure signal as check-runs (one entry per
// workflow run rather than per job/check), normalized through the same
// status/conclusion mapping so it merges into checkDetails' worst-case-wins
// result indistinguishably from a native check-run.
func (p *GitHubProvider) actionsRunsForRef(ctx context.Context, repo RepositoryRef, ref string) ([]githubActionsRun, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "actions", "runs")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"head_sha": []string{ref}})
	if err != nil {
		return nil, err
	}
	var runs []githubActionsRun
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageOut githubActionsRunsResponse
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode actions runs page: %w", err)
		}
		runs = append(runs, pageOut.WorkflowRuns...)
		return nil
	}); err != nil {
		return nil, err
	}
	return runs, nil
}

func (p *GitHubProvider) checkRunAnnotations(ctx context.Context, repo RepositoryRef, checkRunID int64) ([]CheckAnnotation, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "check-runs", strconv.FormatInt(checkRunID, 10), "annotations")
	if err != nil {
		return nil, err
	}
	annotations := []CheckAnnotation{}
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageOut []githubCheckAnnotation
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode check annotations page: %w", err)
		}
		for _, annotation := range pageOut {
			annotations = append(annotations, CheckAnnotation(annotation))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return annotations, nil
}

func normalizeCheckRunState(status, conclusion string) CheckState {
	if strings.ToLower(status) != "completed" {
		return CheckStatePending
	}
	switch strings.ToLower(conclusion) {
	case "success", "neutral", "skipped":
		return CheckStatePassing
	case "failure", "timed_out", "cancelled", "action_required", "stale", "startup_failure":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
}

// RequestReview requests GitHub reviewers for a pull request.
func (p *GitHubProvider) RequestReview(ctx context.Context, req ReviewRequest) error {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return err
	}
	if req.PullID == "" {
		return errPullIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "requested_reviewers")
	if err != nil {
		return err
	}
	body := map[string][]string{"reviewers": req.Reviewers}
	if err := p.do(ctx, http.MethodPost, endpoint, body, nil); err != nil {
		return err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		Operation: "request-review",
		Fields: map[string]FieldDigest{
			"reviewers": {After: digestString(strings.Join(req.Reviewers, ","))},
		},
	})
	return nil
}

// SubmitPullRequestReview publishes a SHA-pinned native GitHub review. GitHub
// associates the review with commit_id, allowing branch-protection
// stale-dismissal to invalidate an approval when the pull request moves.
func (p *GitHubProvider) SubmitPullRequestReview(ctx context.Context, req PullRequestReviewRequest) (PullRequestReviewResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestReviewResult{}, err
	}
	if req.PullID == "" {
		return PullRequestReviewResult{}, errPullIDRequired
	}
	if req.CommitSHA == "" {
		return PullRequestReviewResult{}, fmt.Errorf("commit sha is required")
	}
	if req.Body == "" {
		return PullRequestReviewResult{}, fmt.Errorf("review body is required")
	}
	reviewBody, err := withAttribution(req.Body, p.attribution, "pull-request-review")
	if err != nil {
		return PullRequestReviewResult{}, err
	}

	var event string
	switch req.Decision {
	case ReviewDecisionApproved:
		event = "APPROVE"
	case ReviewDecisionChangesRequested:
		event = "REQUEST_CHANGES"
	default:
		return PullRequestReviewResult{}, fmt.Errorf("unsupported review decision %q", req.Decision)
	}

	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "reviews")
	if err != nil {
		return PullRequestReviewResult{}, err
	}
	body := map[string]string{
		"body":      reviewBody,
		"commit_id": req.CommitSHA,
		"event":     event,
	}
	var out struct {
		ID       int64  `json:"id"`
		HTMLURL  string `json:"html_url"`
		CommitID string `json:"commit_id"`
		State    string `json:"state"`
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestReviewResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       out.HTMLURL,
		Operation: "review",
		Fields: map[string]FieldDigest{
			"body":      {After: digestString(req.Body)},
			"commitSha": {After: digestString(req.CommitSHA)},
			"decision":  {After: digestString(string(req.Decision))},
		},
	})
	return PullRequestReviewResult{
		ID:        out.ID,
		URL:       out.HTMLURL,
		CommitSHA: req.CommitSHA,
		Decision:  req.Decision,
	}, nil
}

// ListWorkItems lists GitHub issues as unified work items.
