package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHubProvider implements repo, backlog, and trigger operations for GitHub.
type GitHubProvider struct {
	BaseURL string
	Token   string
	Client  HTTPClient
	Runner  CommandRunner

	// tokenSource, when set, resolves the token per request (issue #14 seam);
	// otherwise Token is used.
	tokenSource TokenSource
	// recorder receives "external ref touched" facts for the run journal.
	recorder MutationRecorder
	// rateObserver receives rate-limit backoff signals for telemetry.
	rateObserver RateLimitObserver
	// maxRetries bounds rate-limit retries on a single request.
	maxRetries int
	// maxRateLimitWait bounds the total time one request spends sleeping on
	// rate-limit backoff before giving up with a typed RateLimitError (#614).
	maxRateLimitWait time.Duration
	// now and sleep are injectable for deterministic rate-limit tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewGitHubProvider constructs a GitHub provider with optional overrides.
func NewGitHubProvider(token string, opts ...func(*GitHubProvider)) *GitHubProvider {
	p := &GitHubProvider{
		BaseURL:          "https://api.github.com",
		Token:            token,
		maxRetries:       defaultRateLimitRetries,
		maxRateLimitWait: defaultRateLimitMaxWait,
		now:              time.Now,
		sleep:            contextSleep,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.Client = httpClientOrDefault(p.Client)
	p.Runner = commandRunnerOrDefault(p.Runner)
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = contextSleep
	}
	return p
}

// WithTokenSource resolves the access token per request from the given source
// (issue #14 token-source seam) instead of the statically injected token.
func WithTokenSource(source TokenSource) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.tokenSource = source }
}

// WithMutationRecorder records every provider-side mutation as an external-ref
// touched fact for the run journal.
func WithMutationRecorder(recorder MutationRecorder) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.recorder = recorder }
}

// WithRateLimitObserver receives rate-limit backoff signals for telemetry.
func WithRateLimitObserver(observer RateLimitObserver) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.rateObserver = observer }
}

// WithMaxRateLimitRetries overrides how many times a rate-limited request is
// retried before the error is surfaced.
func WithMaxRateLimitRetries(n int) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.maxRetries = n }
}

// WithRateLimitMaxWait overrides the total time one request may spend
// sleeping on rate-limit backoff (#614) before giving up with a typed
// RateLimitError. Keep it under the invoking stage's own timeout: a wait
// that outlives the stage gets the whole process killed as "timeout",
// masking the rate-limit cause.
func WithRateLimitMaxWait(d time.Duration) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.maxRateLimitWait = d }
}

// Kind returns the GitHub provider kind.
func (p *GitHubProvider) Kind() ProviderKind {
	return ProviderGitHub
}

// CloneRepository clones a GitHub repository to a local destination.
func (p *GitHubProvider) CloneRepository(ctx context.Context, req CloneRequest) (CloneResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return CloneResult{}, err
	}
	if req.Destination == "" {
		return CloneResult{}, fmt.Errorf("destination is required")
	}
	cloneURL := req.Repository.URL
	if cloneURL == "" {
		cloneURL = fmt.Sprintf("https://github.com/%s/%s.git", req.Repository.Owner, req.Repository.Name)
	}
	args := []string{"clone"}
	if req.Branch != "" {
		args = append(args, "--branch", req.Branch)
	}
	args = append(args, cloneURL, req.Destination)
	if out, err := p.Runner.Run(ctx, "git", args...); err != nil {
		return CloneResult{}, fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return CloneResult{Path: req.Destination, URL: cloneURL}, nil
}

// CreateBranch creates a branch ref in GitHub.
func (p *GitHubProvider) CreateBranch(ctx context.Context, req BranchRequest) (BranchResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return BranchResult{}, err
	}
	if req.Name == "" {
		return BranchResult{}, fmt.Errorf("branch name is required")
	}
	baseSHA := req.BaseSHA
	if baseSHA == "" {
		baseBranch := req.BaseBranch
		if baseBranch == "" {
			baseBranch = "main"
		}
		ref, err := p.getGitHubRef(ctx, req.Repository, "heads/"+baseBranch)
		if err != nil {
			return BranchResult{}, err
		}
		baseSHA = ref.Object.SHA
	}
	var out githubRef
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "git", "refs")
	if err != nil {
		return BranchResult{}, err
	}
	body := map[string]string{"ref": "refs/heads/" + req.Name, "sha": baseSHA}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return BranchResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, req.Name),
		URL:       out.URL,
		Operation: "branch",
		Fields: map[string]FieldDigest{
			"sha": {After: digestString(out.Object.SHA)},
		},
	})
	return BranchResult{Name: req.Name, SHA: out.Object.SHA, URL: out.URL}, nil
}

// Commit writes file changes to a GitHub branch.
func (p *GitHubProvider) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return CommitResult{}, err
	}
	if req.Branch == "" {
		return CommitResult{}, fmt.Errorf("branch is required")
	}
	if req.Message == "" {
		return CommitResult{}, fmt.Errorf("message is required")
	}
	if len(req.Files) == 0 {
		return CommitResult{}, fmt.Errorf("at least one file is required")
	}

	var last githubContentResponse
	for _, file := range req.Files {
		if file.Path == "" {
			return CommitResult{}, fmt.Errorf("file path is required")
		}
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "contents", file.Path)
		if err != nil {
			return CommitResult{}, err
		}
		endpoint, err = addQuery(endpoint, url.Values{"ref": []string{req.Branch}})
		if err != nil {
			return CommitResult{}, err
		}
		sha, exists, err := p.contentSHA(ctx, endpoint)
		if err != nil {
			return CommitResult{}, err
		}
		changeType, err := normalizeCommitChange(file.ChangeType, exists)
		if err != nil {
			return CommitResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		body := map[string]string{
			"message": req.Message,
			"branch":  req.Branch,
		}
		if exists {
			body["sha"] = sha
		}
		method := http.MethodPut
		if changeType == CommitChangeDelete {
			method = http.MethodDelete
		} else {
			body["content"] = base64.StdEncoding.EncodeToString([]byte(file.Content))
		}
		if err := p.do(ctx, method, endpoint, body, &last); err != nil {
			return CommitResult{}, err
		}
	}
	result := CommitResult{SHA: last.Commit.SHA, URL: last.Commit.HTMLURL}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, result.SHA),
		URL:       result.URL,
		Operation: "commit",
		Fields: map[string]FieldDigest{
			"sha":   {After: digestString(result.SHA)},
			"files": {After: digestString(strconv.Itoa(len(req.Files)))},
		},
	})
	// NOTE(#140 item 4): this still commits one Contents-API call per file, so a
	// mid-loop failure can strand a half-committed branch and CommitResult
	// reports only the last file's SHA. Making it atomic needs the Git data API
	// (blobs -> tree -> commit -> update-ref); tracked as a follow-up so it can
	// land with its own multi-file atomicity test rather than ride this PR.
	return result, nil
}

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
	endpoint, err = addQuery(endpoint, url.Values{
		"head":  []string{repo.Owner + ":" + head},
		"base":  []string{base},
		"state": []string{"open"},
	})
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

// ListOpenPullRequestHeads returns the head-branch ref of every open PR on the
// repo. Used by the scheduler's open-PR-count throttle (#353) to count the
// loop's own un-merged sibling PRs (those under the goobers/ run-branch
// namespace) so it can pace dispatch. Single page of up to 100 — ample for a
// dogfood loop; a full paginator is a follow-up if a repo ever carries >100
// open PRs (at which point the cap has long since engaged anyway).
func (p *GitHubProvider) ListOpenPullRequestHeads(ctx context.Context, repo RepositoryRef) ([]string, error) {
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
	heads := make([]string, 0, len(out))
	for _, pr := range out {
		heads = append(heads, pr.Head.Ref)
	}
	return heads, nil
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
		return PullRequestPollResult{}, fmt.Errorf("pull id is required")
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

	comments, err := p.pullRequestComments(ctx, req.Repository, req.PullID, req.CommentsSince)
	if err != nil {
		return PullRequestPollResult{}, err
	}

	return PullRequestPollResult{
		Number:           pr.Number,
		State:            pr.State,
		Merged:           pr.Merged,
		Mergeable:        pr.Mergeable,
		Draft:            pr.Draft,
		HeadSHA:          pr.Head.SHA,
		BaseSHA:          pr.Base.SHA,
		BaseBranch:       pr.Base.Ref,
		Body:             pr.Body,
		ReviewDecision:   decision,
		RequestedChanges: requestedChanges,
		CheckState:       checkState,
		Checks:           checks,
		CommentsSince:    comments,
		URL:              pr.HTMLURL,
	}, nil
}

// ClosePullRequest closes a GitHub pull request, detecting merged-vs-closed, and
// optionally leaves a comment.
func (p *GitHubProvider) ClosePullRequest(ctx context.Context, req ClosePullRequestRequest) (ClosePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.PullID == "" {
		return ClosePullRequestResult{}, fmt.Errorf("pull id is required")
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
		comments, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.PullID, "comments")
		if err != nil {
			return ClosePullRequestResult{}, err
		}
		if err := p.do(ctx, http.MethodPost, comments, map[string]string{"body": req.Comment}, nil); err != nil {
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
		return MergePullRequestResult{}, fmt.Errorf("pull id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "merge")
	if err != nil {
		return MergePullRequestResult{}, err
	}
	body := map[string]interface{}{}
	if req.ExpectedHeadSHA != "" {
		body["sha"] = req.ExpectedHeadSHA
	}
	if req.CommitMessage != "" {
		body["commit_message"] = req.CommitMessage
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
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return nil, err
	}
	values := url.Values{"state": []string{"open"}}
	if req.Base != "" {
		values.Set("base", req.Base)
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
		prs = append(prs, pageOut...)
		return nil
	}); err != nil {
		return nil, err
	}

	out := make([]PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		if req.HeadPrefix != "" && !strings.HasPrefix(pr.Head.Ref, req.HeadPrefix) {
			continue
		}
		var checkState CheckState
		if !req.SkipCheckState {
			checkState, _, err = p.combinedCheckState(ctx, req.Repository, pr.Head.SHA)
			if err != nil {
				return nil, err
			}
		}
		labels := make([]string, 0, len(pr.Labels))
		for _, l := range pr.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, PullRequestSummary{
			ID:         strconv.Itoa(pr.Number),
			Number:     pr.Number,
			URL:        pr.HTMLURL,
			Head:       pr.Head.Ref,
			Base:       pr.Base.Ref,
			HeadSHA:    pr.Head.SHA,
			BaseSHA:    pr.Base.SHA,
			Draft:      pr.Draft,
			Labels:     labels,
			CheckState: checkState,
			UpdatedAt:  pr.UpdatedAt,
			Body:       pr.Body,
		})
	}
	return out, nil
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
		return nil, fmt.Errorf("pull id is required")
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
		out = append(out, ChangedFile{Path: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions})
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
		return nil, fmt.Errorf("pull id is required")
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

// combinedCheckState normalizes GitHub's legacy combined status plus check-runs
// into a single CheckState + per-check detail refs (BL-031).
func (p *GitHubProvider) combinedCheckState(ctx context.Context, repo RepositoryRef, ref string) (CheckState, []CheckDetail, error) {
	if ref == "" {
		return CheckStatePending, nil, nil
	}
	var details []CheckDetail
	failing, pending := false, false

	statusEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "status")
	if err != nil {
		return "", nil, err
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
		return "", nil, err
	}
	for _, status := range statuses {
		state := normalizeCombinedStatusState(status.State)
		details = append(details, CheckDetail{Name: status.Context, State: state, URL: status.TargetURL, Summary: status.Description})
		switch state {
		case CheckStateFailing:
			failing = true
		case CheckStatePending:
			pending = true
		}
	}

	runsEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "check-runs")
	if err != nil {
		return "", nil, err
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
		return "", nil, err
	}
	for _, run := range checkRuns {
		state := normalizeCheckRunState(run.Status, run.Conclusion)
		details = append(details, CheckDetail{Name: run.Name, State: state, URL: run.HTMLURL, Summary: run.Output.Summary})
		switch state {
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

func (p *GitHubProvider) pullRequestComments(ctx context.Context, repo RepositoryRef, pullID string, since *time.Time) ([]PullRequestComment, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", pullID, "comments")
	if err != nil {
		return nil, err
	}
	if since != nil {
		endpoint, err = addQuery(endpoint, url.Values{"since": []string{since.UTC().Format(time.RFC3339)}})
		if err != nil {
			return nil, err
		}
	}
	var raw []githubIssueComment
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &raw); err != nil {
		return nil, err
	}
	comments := make([]PullRequestComment, 0, len(raw))
	for _, c := range raw {
		comments = append(comments, PullRequestComment{ID: c.ID, Author: c.User.Login, Body: c.Body, URL: c.HTMLURL, CreatedAt: c.CreatedAt})
	}
	return comments, nil
}

func normalizeCombinedStatusState(state string) CheckState {
	switch strings.ToLower(state) {
	case "success":
		return CheckStatePassing
	case "failure", "error":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
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
		return fmt.Errorf("pull id is required")
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

// ListWorkItems lists GitHub issues as unified work items.
func (p *GitHubProvider) ListWorkItems(ctx context.Context, req ListWorkItemsRequest) ([]WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
	if err != nil {
		return nil, err
	}
	values := url.Values{"state": []string{"all"}}
	if req.State != "" {
		values.Set("state", req.State)
	}
	if len(req.Labels) > 0 {
		values.Set("labels", strings.Join(req.Labels, ","))
	}
	if req.Assignee != "" {
		values.Set("assignee", req.Assignee)
	}
	if req.UpdatedSince != nil {
		values.Set("since", req.UpdatedSince.UTC().Format(time.RFC3339))
	}
	if req.OldestFirst {
		// GitHub's issues list defaults to newest-first (sort=created,
		// direction=desc — undocumented but confirmed live, #532). An explicit
		// ascending sort makes the fetch itself FIFO, so a Limit-truncated read
		// drops the newest items (still reachable after older ones drain)
		// instead of permanently starving the oldest.
		values.Set("sort", "created")
		values.Set("direction", "asc")
	}
	if req.Page > 0 {
		// An explicit Page means the caller drives pagination itself: honor it
		// as a single-page read (its own per_page, no Link following).
		if req.Limit > 0 {
			values.Set("per_page", strconv.Itoa(req.Limit))
		}
		values.Set("page", strconv.Itoa(req.Page))
		endpoint, err = addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		var issues []githubIssue
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &issues); err != nil {
			return nil, err
		}
		return issuesToWorkItems(issues, req.Limit), nil
	}

	endpoint, err = addQuery(endpoint, values)
	if err != nil {
		return nil, err
	}
	// Follow pagination and accumulate up to Limit NON-PR items. The issues
	// endpoint also returns pull requests (excluded — PRs are the repo
	// provider's surface, #13); filtering them out of a single Limit-sized page
	// silently returned fewer than Limit real issues (#139).
	var items []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode issues page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest != nil {
				continue
			}
			items = append(items, mapGitHubIssue(issue))
			if req.Limit > 0 && len(items) >= req.Limit {
				return errStopPaging
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// issuesToWorkItems maps a page of GitHub issues to WorkItems, skipping pull
// requests, and truncates to limit (0 = no cap).
func issuesToWorkItems(issues []githubIssue, limit int) []WorkItem {
	items := make([]WorkItem, 0, len(issues))
	for _, issue := range issues {
		if issue.PullRequest != nil {
			continue
		}
		items = append(items, mapGitHubIssue(issue))
		if limit > 0 && len(items) >= limit {
			break
		}
	}
	return items
}

// GetWorkItem reads a GitHub issue as a unified work item.
func (p *GitHubProvider) GetWorkItem(ctx context.Context, repo RepositoryRef, id string) (WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return WorkItem{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id)
	if err != nil {
		return WorkItem{}, err
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &issue); err != nil {
		return WorkItem{}, err
	}
	return mapGitHubIssue(issue), nil
}

// CreateWorkItem creates a GitHub issue from a unified work item request.
func (p *GitHubProvider) CreateWorkItem(ctx context.Context, req CreateWorkItemRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
	if err != nil {
		return WorkItem{}, err
	}
	// Idempotency (#140): a prior attempt's POST may have committed on the
	// server before its response reached us (a timeout), so a policy retry must
	// not file a duplicate. When the caller supplies a RunID we stamp a
	// run-id footer into the body and, before creating, search for an existing
	// item carrying it — returning that instead. Best-effort: GitHub's search
	// index is eventually consistent, so a retry within a second or two of the
	// original may still miss it; the footer at least makes any duplicate
	// traceable and recordExternalRef journals every create.
	itemBody := withRunIDFooter(req.Body, req.RunID)
	if req.RunID != "" {
		if existing, found, err := p.findRunItem(ctx, req.Repository, req.RunID); err != nil {
			return WorkItem{}, err
		} else if found {
			return existing, nil
		}
	}
	labels := replaceStatusLabel(req.Labels, req.Status)
	body := map[string]interface{}{
		"title":  req.Title,
		"body":   itemBody,
		"labels": labels,
	}
	if req.Assignee != "" {
		body["assignees"] = []string{req.Assignee}
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodPost, endpoint, body, &issue); err != nil {
		return WorkItem{}, err
	}
	item := mapGitHubIssue(issue)
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, strconv.Itoa(issue.Number)),
		URL:       item.URL,
		Operation: "create",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(req.Title)},
			"body":  {After: digestString(itemBody)},
		},
	})
	return item, nil
}

// findRunItem searches the repo for an issue whose body carries the run-id
// footer for runID, used by CreateWorkItem for idempotency (#140). The search
// term is fuzzy, so the match is confirmed against the exact footer before
// returning.
func (p *GitHubProvider) findRunItem(ctx context.Context, repo RepositoryRef, runID string) (WorkItem, bool, error) {
	endpoint, err := joinURL(p.BaseURL, "search", "issues")
	if err != nil {
		return WorkItem{}, false, err
	}
	query := fmt.Sprintf(`repo:%s/%s in:body type:issue "%s"`, repo.Owner, repo.Name, runFooter(runID))
	endpoint, err = addQuery(endpoint, url.Values{"q": {query}, "per_page": {"20"}})
	if err != nil {
		return WorkItem{}, false, err
	}
	var out struct {
		Items []githubIssue `json:"items"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return WorkItem{}, false, err
	}
	for _, issue := range out.Items {
		if strings.Contains(issue.Body, runFooter(runID)) {
			return mapGitHubIssue(issue), true, nil
		}
	}
	return WorkItem{}, false, nil
}

// UpdateWorkItemStatus mirrors Goobers processing status to GitHub labels.
func (p *GitHubProvider) UpdateWorkItemStatus(ctx context.Context, req UpdateWorkItemStatusRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	// Swap only the status label via the label sub-API (add new, remove any
	// stale status labels) rather than PATCHing the whole label set. A
	// read-modify-write of all labels would silently clobber a label a human or
	// the curator added between our GET above and the write — the status mirror
	// has no business overwriting unrelated labels (#140).
	newLabel := statusLabel(req.Status)
	var remove []string
	for _, l := range current.Labels {
		if strings.HasPrefix(l, statusLabelPrefix) && l != newLabel {
			remove = append(remove, l)
		}
	}
	if err := p.applyLabelChanges(ctx, req.Repository, req.ID, []string{newLabel}, remove); err != nil {
		return WorkItem{}, err
	}
	if req.Status == WorkItemStatusDone {
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID)
		if err != nil {
			return WorkItem{}, err
		}
		// state-only PATCH — labels are handled above, so closing never
		// round-trips (and races) the label set.
		if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"state": "closed"}, nil); err != nil {
			return WorkItem{}, err
		}
	}
	if req.Comment != "" {
		comments, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID, "comments")
		if err != nil {
			return WorkItem{}, err
		}
		if err := p.do(ctx, http.MethodPost, comments, map[string]string{"body": req.Comment}, nil); err != nil {
			return WorkItem{}, err
		}
	}
	item, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.ID),
		URL:       item.URL,
		Operation: "status",
		Fields: map[string]FieldDigest{
			"status": {Before: digestString(string(statusFromLabels(current.Labels, current.State))), After: digestString(string(req.Status))},
		},
	})
	return item, nil
}

// Subscribe emits GitHub backlog item availability events.
//
// NOT WIRED YET — banner per #140 item 5. Two issues to resolve before anyone
// depends on this: (1) the poll loop silently swallows ListWorkItems errors
// (a persistent failure looks like an empty backlog forever, not an error);
// (2) the `seen` map grows unbounded for the process lifetime. At V0 the
// scheduler triggers via cron backlog-query stages, not this in-process
// subscription, so it has no live caller; fix both before it gets one.
func (p *GitHubProvider) Subscribe(ctx context.Context, sub TriggerSubscription) (<-chan WorkItemEvent, error) {
	if sub.Kind != TriggerPolling {
		return nil, fmt.Errorf("github provider supports polling subscriptions in-process; webhook delivery is configured externally")
	}
	interval := sub.PollInterval
	if interval <= 0 {
		interval = time.Minute
	}
	events := make(chan WorkItemEvent, 1)
	go func() {
		defer close(events)
		seen := map[string]time.Time{}
		for {
			items, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: sub.Repository, State: "open", Limit: 100})
			if err == nil {
				for _, item := range items {
					if !shouldEmitWorkItem(seen, item) {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case events <- WorkItemEvent{Provider: ProviderGitHub, Kind: TriggerPolling, Item: item, Action: "available"}:
					}
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return events, nil
}

func (p *GitHubProvider) contentSHA(ctx context.Context, endpoint string) (string, bool, error) {
	// Routed through send so this read gets the same rate-limit/5xx/transport
	// retries as every other request — it previously issued a raw Do and a
	// single blip failed the caller outright (#139). The 404 = "no such
	// content" semantic below is preserved (send does not retry 404).
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", false, fmt.Errorf("GET %s failed: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, fmt.Errorf("decode response: %w", err)
	}
	return out.SHA, out.SHA != "", nil
}

func (p *GitHubProvider) getGitHubRef(ctx context.Context, repo RepositoryRef, ref string) (githubRef, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "ref", ref)
	if err != nil {
		return githubRef{}, err
	}
	var out githubRef
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return githubRef{}, err
	}
	return out, nil
}

func (p *GitHubProvider) do(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	return p.doStatus(ctx, method, endpoint, body, out, nil)
}

// send issues one GitHub request, retrying transient failures — rate limits,
// 5xx server errors, and transport errors — with bounded backoff (up to
// p.maxRetries attempts and, for rate limits, up to maxRateLimitWait total
// sleep, honoring X-RateLimit-Reset/Retry-After). It returns the final
// response for the caller to consume and close; a nil error guarantees a
// non-nil response. A rate limit that cannot be absorbed within those
// budgets returns a typed *RateLimitError (#614) rather than the response,
// so no caller ever folds it into a generic non-2xx string error. Callers
// that only need a decoded body should use doStatus; getAllPages uses send
// directly so it can read the Link header for pagination (#139).
func (p *GitHubProvider) send(ctx context.Context, method, endpoint string, body interface{}) (*http.Response, error) {
	maxWait := p.maxRateLimitWait
	if maxWait <= 0 {
		maxWait = defaultRateLimitMaxWait
	}
	var rateLimitWaited time.Duration
	for attempt := 0; ; attempt++ {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		token, err := p.resolveToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClientOrDefault(p.Client).Do(req)
		if err != nil {
			// Transport error (connection reset, DNS blip, timeout): retry with
			// backoff rather than fail the stage on a single network hiccup
			// (#139). No response to close on this path.
			if attempt < p.maxRetries {
				if serr := p.sleep(ctx, backoffDuration(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, fmt.Errorf("send request: %w", err)
		}
		if isRateLimited(resp) {
			wait, ev := p.rateLimitPlan(resp, endpoint, attempt)
			_ = resp.Body.Close()
			if attempt >= p.maxRetries || rateLimitWaited+wait > maxWait {
				// Waiting can't help within this request's budget — the
				// retry allowance is spent, or the reset is further out than
				// the wait budget allows (#614). Fail FAST with the typed
				// error so the caller (and the run journal) sees "rate
				// limited, resets at <t>" instead of a generic 403 string,
				// and no time is burned sleeping toward a wait that cannot
				// reach the reset anyway.
				return nil, rateLimitErrorFrom(ev)
			}
			p.observeRateLimit(ctx, ev)
			if err := p.sleep(ctx, wait); err != nil {
				return nil, err
			}
			rateLimitWaited += wait
			continue
		}
		if resp.StatusCode >= 500 && attempt < p.maxRetries {
			// Server-side error: retry with backoff. GitHub 5xx is usually
			// transient; without this a single blip fails the stage attempt.
			_ = resp.Body.Close()
			if err := p.sleep(ctx, backoffDuration(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
}

// doStatus performs a GitHub request with transient-failure retries (see send).
// Status codes in allowStatus are treated as success (used to tolerate a 404
// when removing a label that is not present); the response body is not decoded
// for those.
func (p *GitHubProvider) doStatus(ctx context.Context, method, endpoint string, body, out interface{}, allowStatus []int) error {
	resp, err := p.send(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	for _, code := range allowStatus {
		if resp.StatusCode == code {
			_ = resp.Body.Close()
			return nil
		}
	}
	return readJSONResponse(resp, method, endpoint, out)
}

// getAllPages issues GET requests against endpoint with per_page maximized,
// following the response Link header's rel="next" until the result set is
// exhausted, and invokes onPage with each page's raw JSON body. This is the
// shared paginator (#139): before it, every list/read site consumed only the
// first (default 30-item) page, so a claim breadcrumb, failing check, or
// changes-requested review beyond page 1 was silently invisible.
func (p *GitHubProvider) getAllPages(ctx context.Context, endpoint string, onPage func([]byte) error) error {
	next, err := withPerPage(endpoint, maxPerPage)
	if err != nil {
		return err
	}
	for next != "" {
		resp, err := p.send(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		body, nextLink, err := readPage(resp, http.MethodGet, next)
		if err != nil {
			return err
		}
		if err := onPage(body); err != nil {
			if errors.Is(err, errStopPaging) {
				return nil
			}
			return err
		}
		next = nextLink
	}
	return nil
}

// resolveToken returns the per-request token from the token source when configured,
// falling back to the statically injected token.
func (p *GitHubProvider) resolveToken(ctx context.Context) (string, error) {
	if p.tokenSource != nil {
		return p.tokenSource.Token(ctx)
	}
	return p.Token, nil
}

func (p *GitHubProvider) recordExternalRef(ctx context.Context, ref ExternalRef) {
	if p.recorder != nil {
		p.recorder.RecordExternalRef(ctx, ref)
	}
}

func (p *GitHubProvider) observeRateLimit(ctx context.Context, ev RateLimitEvent) {
	if p.rateObserver != nil {
		p.rateObserver.ObserveRateLimit(ctx, ev)
	}
}

type githubIssue struct {
	ID        int64         `json:"id"`
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	State     string        `json:"state"`
	HTMLURL   string        `json:"html_url"`
	Labels    []githubLabel `json:"labels"`
	Assignees []githubUser  `json:"assignees"`
	Milestone *githubNode   `json:"milestone"`
	UpdatedAt *time.Time    `json:"updated_at"`
	// PullRequest is non-nil when this "issue" is actually a pull request.
	PullRequest *githubPullRequestLink `json:"pull_request"`
}

// githubPullRequestLink marks an issues-endpoint entry as a pull request.
type githubPullRequestLink struct {
	URL string `json:"url"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubUser struct {
	Login string `json:"login"`
}

type githubNode struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
}

type githubRef struct {
	Ref    string `json:"ref"`
	URL    string `json:"url"`
	Object struct {
		SHA string `json:"sha"`
		URL string `json:"url"`
	} `json:"object"`
}

type githubContentResponse struct {
	Commit struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	} `json:"commit"`
}

type githubPullRequest struct {
	ID      int    `json:"id"`
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

type githubPullRequestDetail struct {
	ID        int64         `json:"id"`
	Number    int           `json:"number"`
	State     string        `json:"state"`
	Merged    bool          `json:"merged"`
	Mergeable *bool         `json:"mergeable"`
	Draft     bool          `json:"draft"`
	Body      string        `json:"body"`
	HTMLURL   string        `json:"html_url"`
	Labels    []githubLabel `json:"labels"`
	UpdatedAt time.Time     `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

type githubMergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

type githubPullRequestFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type githubReview struct {
	State string     `json:"state"`
	User  githubUser `json:"user"`
}

type githubCombinedStatus struct {
	State    string         `json:"state"`
	Statuses []githubStatus `json:"statuses"`
}

type githubStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
}

type githubCheckRunsResponse struct {
	CheckRuns []githubCheckRun `json:"check_runs"`
}

type githubCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Output     struct {
		Summary string `json:"summary"`
	} `json:"output"`
}

type githubIssueComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	User      githubUser `json:"user"`
	CreatedAt time.Time  `json:"created_at"`
}

func mapGitHubIssue(issue githubIssue) WorkItem {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.Name)
	}
	links := []Link{{Rel: "self", URL: issue.HTMLURL}}
	var parent *WorkItemRef
	hierarchy := map[string]interface{}{}
	if issue.Milestone != nil {
		parent = &WorkItemRef{Provider: ProviderGitHub, ID: strconv.Itoa(issue.Milestone.Number), URL: issue.Milestone.HTMLURL, Type: "milestone"}
		hierarchy["milestone"] = issue.Milestone
	}
	assignee := ""
	if len(issue.Assignees) > 0 {
		assignee = issue.Assignees[0].Login
	}
	return WorkItem{
		Provider:   ProviderGitHub,
		ID:         strconv.Itoa(issue.Number),
		ExternalID: strconv.FormatInt(issue.ID, 10),
		Type:       "issue",
		Title:      issue.Title,
		Body:       issue.Body,
		Labels:     labels,
		State:      issue.State,
		Status:     statusFromLabels(labels, issue.State),
		Assignee:   assignee,
		Links:      links,
		Parent:     parent,
		Hierarchy:  hierarchy,
		URL:        issue.HTMLURL,
		UpdatedAt:  issue.UpdatedAt,
		Raw:        issue,
	}
}
