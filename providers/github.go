package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	// now and sleep are injectable for deterministic rate-limit tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewGitHubProvider constructs a GitHub provider with optional overrides.
func NewGitHubProvider(token string, opts ...func(*GitHubProvider)) *GitHubProvider {
	p := &GitHubProvider{
		BaseURL:    "https://api.github.com",
		Token:      token,
		maxRetries: defaultRateLimitRetries,
		now:        time.Now,
		sleep:      contextSleep,
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
	return CommitResult{SHA: last.Commit.SHA, URL: last.Commit.HTMLURL}, nil
}

// OpenPullRequest opens a GitHub pull request.
func (p *GitHubProvider) OpenPullRequest(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestResult{}, err
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

// reviewDecision aggregates a PR's review list into a single decision: the
// latest review per author wins, and any outstanding CHANGES_REQUESTED beats
// any APPROVED (BL-031 review-decision normalization).
func (p *GitHubProvider) reviewDecision(ctx context.Context, repo RepositoryRef, pullID string) (ReviewDecision, int, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "reviews")
	if err != nil {
		return "", 0, err
	}
	var reviews []githubReview
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &reviews); err != nil {
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
	var combined githubCombinedStatus
	if err := p.do(ctx, http.MethodGet, statusEndpoint, nil, &combined); err != nil {
		return "", nil, err
	}
	for _, status := range combined.Statuses {
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
	var runsOut githubCheckRunsResponse
	if err := p.do(ctx, http.MethodGet, runsEndpoint, nil, &runsOut); err != nil {
		return "", nil, err
	}
	for _, run := range runsOut.CheckRuns {
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
	case "failure", "timed_out", "cancelled", "action_required", "stale":
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
	return p.do(ctx, http.MethodPost, endpoint, body, nil)
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
	if req.Limit > 0 {
		values.Set("per_page", strconv.Itoa(req.Limit))
	}
	if req.Page > 0 {
		values.Set("page", strconv.Itoa(req.Page))
	}
	endpoint, err = addQuery(endpoint, values)
	if err != nil {
		return nil, err
	}
	var issues []githubIssue
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &issues); err != nil {
		return nil, err
	}
	items := make([]WorkItem, 0, len(issues))
	for _, issue := range issues {
		// The GitHub issues endpoint also returns pull requests; a backlog issues
		// query excludes them (PRs are the repo provider's surface, issue #13).
		if issue.PullRequest != nil {
			continue
		}
		items = append(items, mapGitHubIssue(issue))
	}
	return items, nil
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
	labels := replaceStatusLabel(req.Labels, req.Status)
	body := map[string]interface{}{
		"title":  req.Title,
		"body":   req.Body,
		"labels": labels,
	}
	if req.Assignee != "" {
		body["assignees"] = []string{req.Assignee}
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodPost, endpoint, body, &issue); err != nil {
		return WorkItem{}, err
	}
	return mapGitHubIssue(issue), nil
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
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	body := map[string]interface{}{"labels": replaceStatusLabel(current.Labels, req.Status)}
	if req.Status == WorkItemStatusDone {
		body["state"] = "closed"
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodPatch, endpoint, body, &issue); err != nil {
		return WorkItem{}, err
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
	return mapGitHubIssue(issue), nil
}

// Subscribe emits GitHub backlog item availability events.
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
	req, err := newJSONRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	token, err := p.resolveToken(ctx)
	if err != nil {
		return "", false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClientOrDefault(p.Client).Do(req)
	if err != nil {
		return "", false, fmt.Errorf("send request: %w", err)
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

// doStatus performs a GitHub request with rate-limit-aware retries. Status codes in
// allowStatus are treated as success (used to tolerate a 404 when removing a label
// that is not present); the response body is not decoded for those.
func (p *GitHubProvider) doStatus(ctx context.Context, method, endpoint string, body, out interface{}, allowStatus []int) error {
	for attempt := 0; ; attempt++ {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return err
		}
		token, err := p.resolveToken(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClientOrDefault(p.Client).Do(req)
		if err != nil {
			return fmt.Errorf("send request: %w", err)
		}
		if isRateLimited(resp) && attempt < p.maxRetries {
			wait, ev := p.rateLimitPlan(resp, endpoint, attempt)
			_ = resp.Body.Close()
			p.observeRateLimit(ctx, ev)
			if err := p.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		for _, code := range allowStatus {
			if resp.StatusCode == code {
				_ = resp.Body.Close()
				return nil
			}
		}
		return readJSONResponse(resp, method, endpoint, out)
	}
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
}

type githubPullRequestDetail struct {
	Number    int    `json:"number"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Mergeable *bool  `json:"mergeable"`
	HTMLURL   string `json:"html_url"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
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
