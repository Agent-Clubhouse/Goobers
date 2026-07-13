package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// errADOBacklogV1 marks the extended backlog operations that reach ADO parity in
// V1 (BL-033); the V0 workload runs on GitHub only.
var errADOBacklogV1 = errors.New("ado: extended backlog operations (comments, general update, claim) land in V1 (BL-033)")

// ADOProvider implements repo, backlog, and trigger operations for Azure DevOps.
type ADOProvider struct {
	Organization string
	Project      string
	BaseURL      string
	Token        string
	Username     string
	Client       HTTPClient
	Runner       CommandRunner
}

// NewADOProvider constructs an Azure DevOps provider with optional overrides.
func NewADOProvider(organization, project, token string, opts ...func(*ADOProvider)) *ADOProvider {
	p := &ADOProvider{
		Organization: organization,
		Project:      project,
		BaseURL:      "https://dev.azure.com",
		Token:        token,
		Username:     "goobers",
	}
	for _, opt := range opts {
		opt(p)
	}
	p.Client = httpClientOrDefault(p.Client)
	p.Runner = commandRunnerOrDefault(p.Runner)
	return p
}

// Kind returns the Azure DevOps provider kind.
func (p *ADOProvider) Kind() ProviderKind {
	return ProviderADO
}

// CloneRepository clones an Azure DevOps repository to a local destination.
func (p *ADOProvider) CloneRepository(ctx context.Context, req CloneRequest) (CloneResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return CloneResult{}, err
	}
	if req.Destination == "" {
		return CloneResult{}, fmt.Errorf("destination is required")
	}
	cloneURL := req.Repository.URL
	if cloneURL == "" {
		cloneURL = fmt.Sprintf("%s/%s/%s/_git/%s", strings.TrimRight(p.BaseURL, "/"), url.PathEscape(p.Organization), url.PathEscape(p.project(req.Repository)), url.PathEscape(req.Repository.Name))
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

// CreateBranch creates an Azure DevOps branch ref.
func (p *ADOProvider) CreateBranch(ctx context.Context, req BranchRequest) (BranchResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return BranchResult{}, err
	}
	if req.Name == "" {
		return BranchResult{}, fmt.Errorf("branch name is required")
	}
	if req.BaseSHA == "" {
		return BranchResult{}, fmt.Errorf("base sha is required for ado branch creation")
	}
	endpoint, err := p.repoURL(req.Repository, "refs")
	if err != nil {
		return BranchResult{}, err
	}
	body := []map[string]string{{
		"name":        "refs/heads/" + req.Name,
		"oldObjectId": "0000000000000000000000000000000000000000",
		"newObjectId": req.BaseSHA,
	}}
	var out adoRefsResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return BranchResult{}, err
	}
	if len(out.Value) == 0 {
		return BranchResult{}, fmt.Errorf("ado branch creation returned no refs")
	}
	return BranchResult{Name: strings.TrimPrefix(out.Value[0].Name, "refs/heads/"), SHA: out.Value[0].ObjectID, URL: out.Value[0].URL}, nil
}

// Commit writes file changes to an Azure DevOps branch.
func (p *ADOProvider) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	if err := requireRepo(req.Repository); err != nil {
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
	changes := make([]adoChange, 0, len(req.Files))
	for _, file := range req.Files {
		if file.Path == "" {
			return CommitResult{}, fmt.Errorf("file path is required")
		}
		exists, err := p.pathExists(ctx, req.Repository, req.Branch, file.Path)
		if err != nil {
			return CommitResult{}, err
		}
		changeType, err := normalizeCommitChange(file.ChangeType, exists)
		if err != nil {
			return CommitResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		change := adoChange{
			ChangeType: string(changeType),
			Item:       map[string]string{"path": "/" + strings.TrimPrefix(file.Path, "/")},
		}
		if changeType != CommitChangeDelete {
			change.NewContent = &adoNewContent{Content: file.Content, ContentType: "rawtext"}
		}
		changes = append(changes, change)
	}
	endpoint, err := p.repoURL(req.Repository, "pushes")
	if err != nil {
		return CommitResult{}, err
	}
	oldObjectID := req.BaseSHA
	if oldObjectID == "" {
		var err error
		oldObjectID, err = p.branchSHA(ctx, req.Repository, req.Branch)
		if err != nil {
			return CommitResult{}, err
		}
	}
	body := adoPushRequest{
		RefUpdates: []adoRefUpdate{{Name: "refs/heads/" + req.Branch, OldObjectID: oldObjectID}},
		Commits:    []adoCommit{{Comment: req.Message, Changes: changes}},
	}
	var out adoPushResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return CommitResult{}, err
	}
	commitID := ""
	if len(out.Commits) > 0 {
		commitID = out.Commits[0].CommitID
	}
	return CommitResult{SHA: commitID, URL: out.URL}, nil
}

// OpenPullRequest opens an Azure DevOps pull request.
func (p *ADOProvider) OpenPullRequest(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PullRequestResult{}, err
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests")
	if err != nil {
		return PullRequestResult{}, err
	}
	body := map[string]interface{}{
		"sourceRefName": "refs/heads/" + strings.TrimPrefix(req.Head, "refs/heads/"),
		"targetRefName": "refs/heads/" + strings.TrimPrefix(req.Base, "refs/heads/"),
		"title":         req.Title,
		"description":   req.Body,
		"isDraft":       req.Draft,
	}
	var out adoPullRequest
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestResult{}, err
	}
	return PullRequestResult{ID: strconv.Itoa(out.PullRequestID), Number: out.PullRequestID, URL: out.URL}, nil
}

// RequestReview requests Azure DevOps reviewers for a pull request.
func (p *ADOProvider) RequestReview(ctx context.Context, req ReviewRequest) error {
	if err := requireRepo(req.Repository); err != nil {
		return err
	}
	if req.PullID == "" {
		return fmt.Errorf("pull id is required")
	}
	for _, reviewer := range req.Reviewers {
		endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID, "reviewers", reviewer)
		if err != nil {
			return err
		}
		if err := p.do(ctx, http.MethodPut, endpoint, map[string]int{"vote": 0}, nil); err != nil {
			return err
		}
	}
	return nil
}

// ListWorkItems lists Azure Boards work items as unified work items.
func (p *ADOProvider) ListWorkItems(ctx context.Context, req ListWorkItemsRequest) ([]WorkItem, error) {
	project := p.project(req.Repository)
	query := "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project"
	if req.State != "" {
		query += fmt.Sprintf(" AND [System.State] = '%s'", strings.ReplaceAll(req.State, "'", "''"))
	}
	endpoint, err := p.workURL(project, "wiql")
	if err != nil {
		return nil, err
	}
	var wiql adoWIQLResponse
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"query": query}, &wiql); err != nil {
		return nil, err
	}
	items := make([]WorkItem, 0, len(wiql.WorkItems))
	for _, ref := range wiql.WorkItems {
		item, err := p.GetWorkItem(ctx, req.Repository, strconv.Itoa(ref.ID))
		if err != nil {
			return nil, err
		}
		if hasAllLabels(item.Labels, req.Labels) {
			items = append(items, item)
			if req.Limit > 0 && len(items) >= req.Limit {
				break
			}
		}
	}
	return items, nil
}

// GetWorkItem reads an Azure Boards item as a unified work item.
func (p *ADOProvider) GetWorkItem(ctx context.Context, repo RepositoryRef, id string) (WorkItem, error) {
	endpoint, err := p.workURL(p.project(repo), "workitems", id)
	if err != nil {
		return WorkItem{}, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"$expand": []string{"Relations"}})
	if err != nil {
		return WorkItem{}, err
	}
	var out adoWorkItem
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return WorkItem{}, err
	}
	return mapADOWorkItem(out), nil
}

// CreateWorkItem creates an Azure Boards work item.
func (p *ADOProvider) CreateWorkItem(ctx context.Context, req CreateWorkItemRequest) (WorkItem, error) {
	itemType := req.Type
	if itemType == "" {
		itemType = "Issue"
	}
	endpoint, err := p.workURL(p.project(req.Repository), "workitems", "$"+itemType)
	if err != nil {
		return WorkItem{}, err
	}
	labels := replaceStatusLabel(req.Labels, req.Status)
	patch := []adoPatchOperation{
		{Op: "add", Path: "/fields/System.Title", Value: req.Title},
		{Op: "add", Path: "/fields/System.Description", Value: req.Body},
	}
	if len(labels) > 0 {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.Tags", Value: strings.Join(labels, "; ")})
	}
	if req.Assignee != "" {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.AssignedTo", Value: req.Assignee})
	}
	var out adoWorkItem
	if err := p.doPatch(ctx, http.MethodPost, endpoint, patch, &out); err != nil {
		return WorkItem{}, err
	}
	return mapADOWorkItem(out), nil
}

// UpdateWorkItemStatus mirrors Goobers processing status to Azure Boards tags.
func (p *ADOProvider) UpdateWorkItemStatus(ctx context.Context, req UpdateWorkItemStatusRequest) (WorkItem, error) {
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	labels := replaceStatusLabel(current.Labels, req.Status)
	patch := []adoPatchOperation{{Op: "add", Path: "/fields/System.Tags", Value: strings.Join(labels, "; ")}}
	if req.Status == WorkItemStatusDone {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.State", Value: "Done"})
	}
	if req.Comment != "" {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.History", Value: req.Comment})
	}
	endpoint, err := p.workURL(p.project(req.Repository), "workitems", req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	var out adoWorkItem
	if err := p.doPatch(ctx, http.MethodPatch, endpoint, patch, &out); err != nil {
		return WorkItem{}, err
	}
	return mapADOWorkItem(out), nil
}

// ListComments reaches parity in V1 (BL-033); the ADO discussion-thread mapping
// is not part of the V0 GitHub workload.
func (p *ADOProvider) ListComments(context.Context, RepositoryRef, string) ([]Comment, error) {
	return nil, errADOBacklogV1
}

// UpdateWorkItem reaches parity in V1 (BL-033).
func (p *ADOProvider) UpdateWorkItem(context.Context, UpdateWorkItemRequest) (WorkItem, error) {
	return WorkItem{}, errADOBacklogV1
}

// ClaimWorkItem reaches parity in V1 (BL-033).
func (p *ADOProvider) ClaimWorkItem(context.Context, ClaimWorkItemRequest) (ClaimResult, error) {
	return ClaimResult{}, errADOBacklogV1
}

// Subscribe emits Azure Boards backlog item availability events.
func (p *ADOProvider) Subscribe(ctx context.Context, sub TriggerSubscription) (<-chan WorkItemEvent, error) {
	if sub.Kind != TriggerPolling {
		return nil, fmt.Errorf("ado provider supports polling subscriptions in-process; service hook delivery is configured externally")
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
			items, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: sub.Repository, State: "New", Limit: 100})
			if err == nil {
				for _, item := range items {
					if !shouldEmitWorkItem(seen, item) {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case events <- WorkItemEvent{Provider: ProviderADO, Kind: TriggerPolling, Item: item, Action: "available"}:
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

func (p *ADOProvider) pathExists(ctx context.Context, repo RepositoryRef, branch, path string) (bool, error) {
	endpoint, err := p.repoURL(repo, "items")
	if err != nil {
		return false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"path":                          []string{"/" + strings.TrimPrefix(path, "/")},
		"versionDescriptor.versionType": []string{"branch"},
		"versionDescriptor.version":     []string{branch},
		"includeContentMetadata":        []string{"false"},
	})
	if err != nil {
		return false, err
	}
	req, err := newJSONRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", basicAuth(p.Username, p.Token))
	}
	resp, err := httpClientOrDefault(p.Client).Do(req)
	if err != nil {
		return false, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("GET %s failed: status %d", endpoint, resp.StatusCode)
	}
	return true, nil
}

func (p *ADOProvider) branchSHA(ctx context.Context, repo RepositoryRef, branch string) (string, error) {
	endpoint, err := p.repoURL(repo, "refs")
	if err != nil {
		return "", err
	}
	endpoint, err = addQuery(endpoint, url.Values{"filter": []string{"heads/" + strings.TrimPrefix(branch, "refs/heads/")}})
	if err != nil {
		return "", err
	}
	var out adoRefsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return "", err
	}
	if len(out.Value) == 0 || out.Value[0].ObjectID == "" {
		return "", fmt.Errorf("ado branch %q not found", branch)
	}
	return out.Value[0].ObjectID, nil
}

func (p *ADOProvider) repoURL(repo RepositoryRef, elems ...string) (string, error) {
	repoID := repo.ID
	if repoID == "" {
		repoID = repo.Name
	}
	parts := []string{p.Organization, p.project(repo), "_apis", "git", "repositories", repoID}
	parts = append(parts, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	return addQuery(endpoint, url.Values{"api-version": []string{"7.1"}})
}

func (p *ADOProvider) workURL(project string, elems ...string) (string, error) {
	parts := []string{p.Organization, project, "_apis", "wit"}
	parts = append(parts, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	return addQuery(endpoint, url.Values{"api-version": []string{"7.1"}})
}

func (p *ADOProvider) project(repo RepositoryRef) string {
	if repo.Project != "" {
		return repo.Project
	}
	return p.Project
}

func (p *ADOProvider) do(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	req, err := newJSONRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", basicAuth(p.Username, p.Token))
	}
	return doJSON(httpClientOrDefault(p.Client), req, out)
}

func (p *ADOProvider) doPatch(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	req, err := newJSONRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json-patch+json")
	if p.Token != "" {
		req.Header.Set("Authorization", basicAuth(p.Username, p.Token))
	}
	return doJSON(httpClientOrDefault(p.Client), req, out)
}

type adoWIQLResponse struct {
	WorkItems []struct {
		ID int `json:"id"`
	} `json:"workItems"`
}

type adoWorkItem struct {
	ID        int                    `json:"id"`
	Rev       int                    `json:"rev"`
	URL       string                 `json:"url"`
	Fields    map[string]interface{} `json:"fields"`
	Relations []adoRelation          `json:"relations"`
}

type adoRelation struct {
	Rel        string                 `json:"rel"`
	URL        string                 `json:"url"`
	Attributes map[string]interface{} `json:"attributes"`
}

type adoPatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

type adoRefsResponse struct {
	Value []struct {
		Name     string `json:"name"`
		ObjectID string `json:"objectId"`
		URL      string `json:"url"`
	} `json:"value"`
}

type adoRefUpdate struct {
	Name        string `json:"name"`
	OldObjectID string `json:"oldObjectId"`
}

type adoPushRequest struct {
	RefUpdates []adoRefUpdate `json:"refUpdates"`
	Commits    []adoCommit    `json:"commits"`
}

type adoCommit struct {
	Comment string      `json:"comment"`
	Changes []adoChange `json:"changes"`
}

type adoChange struct {
	ChangeType string            `json:"changeType"`
	Item       map[string]string `json:"item"`
	NewContent *adoNewContent    `json:"newContent,omitempty"`
}

type adoNewContent struct {
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
}

type adoPushResponse struct {
	URL     string `json:"url"`
	Commits []struct {
		CommitID string `json:"commitId"`
	} `json:"commits"`
}

type adoPullRequest struct {
	PullRequestID int    `json:"pullRequestId"`
	URL           string `json:"url"`
}

func mapADOWorkItem(item adoWorkItem) WorkItem {
	labels := adoLabels(stringField(item.Fields, "System.Tags"))
	parent, links, hierarchy := adoHierarchy(item.Relations)
	state := stringField(item.Fields, "System.State")
	updated := timeField(item.Fields, "System.ChangedDate")
	return WorkItem{
		Provider:   ProviderADO,
		ID:         strconv.Itoa(item.ID),
		ExternalID: strconv.Itoa(item.Rev),
		Type:       stringField(item.Fields, "System.WorkItemType"),
		Title:      stringField(item.Fields, "System.Title"),
		Body:       stringField(item.Fields, "System.Description"),
		Labels:     labels,
		State:      state,
		Status:     statusFromLabels(labels, state),
		Assignee:   stringField(item.Fields, "System.AssignedTo"),
		Links:      links,
		Parent:     parent,
		Hierarchy:  hierarchy,
		URL:        item.URL,
		UpdatedAt:  updated,
		Raw:        item,
	}
}

func adoLabels(tags string) []string {
	if tags == "" {
		return nil
	}
	return uniqueStrings(strings.Split(tags, ";"))
}

func adoHierarchy(relations []adoRelation) (*WorkItemRef, []Link, map[string]interface{}) {
	links := make([]Link, 0, len(relations))
	hierarchy := make(map[string]interface{})
	var parent *WorkItemRef
	for _, relation := range relations {
		links = append(links, Link{Rel: relation.Rel, URL: relation.URL})
		if relation.Rel == "System.LinkTypes.Hierarchy-Reverse" {
			parent = &WorkItemRef{Provider: ProviderADO, ID: lastPathSegment(relation.URL), URL: relation.URL, Type: "parent"}
			hierarchy["parent"] = relation
		}
	}
	return parent, links, hierarchy
}

func lastPathSegment(value string) string {
	trimmed := strings.TrimRight(value, "/")
	index := strings.LastIndex(trimmed, "/")
	if index == -1 {
		return trimmed
	}
	return trimmed[index+1:]
}

func stringField(fields map[string]interface{}, key string) string {
	value, ok := fields[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]interface{}:
		if display, ok := typed["displayName"].(string); ok {
			return display
		}
	}
	return fmt.Sprint(value)
}

func timeField(fields map[string]interface{}, key string) *time.Time {
	value := stringField(fields, key)
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func hasAllLabels(itemLabels, required []string) bool {
	if len(required) == 0 {
		return true
	}
	item := make(map[string]struct{}, len(itemLabels))
	for _, label := range itemLabels {
		item[label] = struct{}{}
	}
	for _, label := range required {
		if _, ok := item[label]; !ok {
			return false
		}
	}
	return true
}
