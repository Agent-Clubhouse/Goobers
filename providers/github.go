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
}

// NewGitHubProvider constructs a GitHub provider with optional overrides.
func NewGitHubProvider(token string, opts ...func(*GitHubProvider)) *GitHubProvider {
	p := &GitHubProvider{
		BaseURL: "https://api.github.com",
		Token:   token,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.Client = httpClientOrDefault(p.Client)
	p.Runner = commandRunnerOrDefault(p.Runner)
	return p
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
	body := map[string]interface{}{
		"title": req.Title,
		"body":  req.Body,
		"head":  req.Head,
		"base":  req.Base,
		"draft": req.Draft,
	}
	var out githubPullRequest
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestResult{}, err
	}
	return PullRequestResult{ID: strconv.Itoa(out.Number), Number: out.Number, URL: out.HTMLURL}, nil
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
	if req.Limit > 0 {
		values.Set("per_page", strconv.Itoa(req.Limit))
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
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
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
	req, err := newJSONRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	return doJSON(httpClientOrDefault(p.Client), req, out)
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
