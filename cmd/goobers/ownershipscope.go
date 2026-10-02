package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

const (
	errorCodeIssueOwnershipScope = "issue_ownership_scope_refused"
	ownershipUnassignedAllow     = "allow"
	ownershipUnassignedRefuse    = "refuse"
)

type issueOwnershipScope struct {
	assignees  []string
	unassigned string
}

func issueOwnershipScopeFromInputs() issueOwnershipScope {
	scope := issueOwnershipScope{
		assignees:  splitLabelList(providerInput(executor.InputOwnershipAssignees, "")),
		unassigned: strings.ToLower(strings.TrimSpace(providerInput(executor.InputOwnershipUnassigned, ""))),
	}
	if scope.unassigned == "" && len(scope.assignees) > 0 {
		scope.unassigned = ownershipUnassignedRefuse
	}
	return scope
}

func (s issueOwnershipScope) active() bool {
	return len(s.assignees) > 0 || s.unassigned == ownershipUnassignedRefuse
}

func (s issueOwnershipScope) permits(item providers.WorkItem) bool {
	return s.permitsAssignee(item.Assignee, item.AssigneeAliases)
}

func (s issueOwnershipScope) permitsAssignee(assignee string, aliases []string) bool {
	if !s.active() {
		return true
	}
	if assignee == "" {
		return s.unassigned != ownershipUnassignedRefuse
	}
	if len(s.assignees) == 0 {
		return true
	}
	item := providers.WorkItem{Assignee: assignee, AssigneeAliases: aliases}
	for _, assignee := range s.assignees {
		if item.AssigneeMatches(assignee) {
			return true
		}
	}
	return false
}

func (s issueOwnershipScope) scopeDescription() string {
	parts := make([]string, 0, 2)
	if len(s.assignees) > 0 {
		parts = append(parts, "assignees="+strings.Join(s.assignees, ","))
	}
	if s.unassigned != "" {
		parts = append(parts, "unassigned="+s.unassigned)
	}
	if len(parts) == 0 {
		return "unrestricted"
	}
	return strings.Join(parts, ";")
}

type issueOwnershipScopeError struct {
	Repository providers.RepositoryRef
	ItemID     string
	Owner      string
	Operation  string
	Scope      string
}

func (e *issueOwnershipScopeError) Error() string {
	owner := e.Owner
	if owner == "" {
		owner = "<unassigned>"
	}
	return fmt.Sprintf("issue %s is owned by %s outside ownership scope %s for %s", e.ItemID, owner, e.Scope, e.Operation)
}

func (e *issueOwnershipScopeError) ErrorCode() string  { return errorCodeIssueOwnershipScope }
func (e *issueOwnershipScopeError) ErrorClass() string { return "policy_refusal" }

type scopedIssueProvider struct {
	providers.Provider
	scope issueOwnershipScope
}

func wrapIssueOwnershipProvider(provider providers.Provider) providers.Provider {
	scope := issueOwnershipScopeFromInputs()
	if !scope.active() {
		return provider
	}
	return scopedIssueProvider{Provider: provider, scope: scope}
}

func (p scopedIssueProvider) guardedItem(ctx context.Context, repo providers.RepositoryRef, id, operation string) (providers.WorkItem, error) {
	item, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return providers.WorkItem{}, err
	}
	if p.scope.permits(item) {
		return item, nil
	}
	return providers.WorkItem{}, &issueOwnershipScopeError{
		Repository: repo,
		ItemID:     id,
		Owner:      item.Assignee,
		Operation:  operation,
		Scope:      p.scope.scopeDescription(),
	}
}

func (p scopedIssueProvider) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	item, err := p.guardedItem(ctx, req.Repository, req.ID, "update issue")
	if err != nil {
		return providers.WorkItem{}, err
	}
	if req.Assignee != nil && !p.scope.permitsAssignee(*req.Assignee, nil) {
		return providers.WorkItem{}, &issueOwnershipScopeError{
			Repository: req.Repository,
			ItemID:     req.ID,
			Owner:      *req.Assignee,
			Operation:  "assign issue",
			Scope:      p.scope.scopeDescription(),
		}
	}
	if req.ExpectedRevision == "" {
		req.ExpectedRevision = item.Revision
	}
	return p.Provider.UpdateWorkItem(ctx, req)
}

func (p scopedIssueProvider) UpdateWorkItemStatus(ctx context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	if _, err := p.guardedItem(ctx, req.Repository, req.ID, "update issue status"); err != nil {
		return providers.WorkItem{}, err
	}
	return p.Provider.UpdateWorkItemStatus(ctx, req)
}

func (p scopedIssueProvider) ClaimWorkItem(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.ClaimResult, error) {
	if _, err := p.guardedItem(ctx, req.Repository, req.ID, "claim issue"); err != nil {
		return providers.ClaimResult{}, err
	}
	return p.Provider.ClaimWorkItem(ctx, req)
}

func (p scopedIssueProvider) ReleaseWorkItemClaim(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	if _, err := p.guardedItem(ctx, req.Repository, req.ID, "release issue claim"); err != nil {
		return providers.WorkItem{}, err
	}
	releaser, ok := p.Provider.(workItemClaimReleaser)
	if !ok {
		return providers.WorkItem{}, fmt.Errorf("repository provider %q does not support release issue claim", req.Repository.Provider)
	}
	return releaser.ReleaseWorkItemClaim(ctx, req)
}

func (p scopedIssueProvider) CreateWorkItem(ctx context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	if !p.scope.permitsAssignee(req.Assignee, nil) {
		return providers.WorkItem{}, &issueOwnershipScopeError{
			Repository: req.Repository,
			Owner:      req.Assignee,
			Operation:  "create assigned issue",
			Scope:      p.scope.scopeDescription(),
		}
	}
	return p.Provider.CreateWorkItem(ctx, req)
}

func (p scopedIssueProvider) CreateWorkItemComment(ctx context.Context, repo providers.RepositoryRef, id, body string) (providers.Comment, error) {
	if _, err := p.guardedItem(ctx, repo, id, "create issue comment"); err != nil {
		return providers.Comment{}, err
	}
	commenter, ok := p.Provider.(workItemCommentCreator)
	if !ok {
		return providers.Comment{}, fmt.Errorf("repository provider %q does not support create issue comment", repo.Provider)
	}
	return commenter.CreateWorkItemComment(ctx, repo, id, body)
}

func (p scopedIssueProvider) EnsureWorkItemLabels(ctx context.Context, repo providers.RepositoryRef, labels []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error) {
	ensurer, ok := p.Provider.(workItemLabelEnsurer)
	if !ok {
		return providers.EnsureWorkItemLabelsResult{}, fmt.Errorf("repository provider %q does not support ensure issue labels", repo.Provider)
	}
	return ensurer.EnsureWorkItemLabels(ctx, repo, labels)
}

func (p scopedIssueProvider) ListWorkItemLabelTransitionsForItem(
	ctx context.Context,
	repo providers.RepositoryRef,
	id string,
	label string,
) ([]providers.WorkItemLabelTransition, error) {
	lister, ok := p.Provider.(workItemLabelTransitionLister)
	if !ok {
		return nil, fmt.Errorf("repository provider %q does not support work item label transitions", repo.Provider)
	}
	return lister.ListWorkItemLabelTransitionsForItem(ctx, repo, id, label)
}

type workItemLabelTransitionLister interface {
	ListWorkItemLabelTransitionsForItem(context.Context, providers.RepositoryRef, string, string) ([]providers.WorkItemLabelTransition, error)
}

type workItemCommentCreator interface {
	CreateWorkItemComment(context.Context, providers.RepositoryRef, string, string) (providers.Comment, error)
}

type workItemLabelEnsurer interface {
	EnsureWorkItemLabels(context.Context, providers.RepositoryRef, []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error)
}
