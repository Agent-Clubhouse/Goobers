package providers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// githubAncestryDefaultFields: a GitHub issue's intent lives in its body.
var githubAncestryDefaultFields = []string{"body"}

// githubParentIssue is the sub-issues "get parent issue" response: an issue
// plus the repository it lives in and its issue type, when one is set.
type githubParentIssue struct {
	githubIssue
	RepositoryURL string `json:"repository_url"`
	Type          *struct {
		Name string `json:"name"`
	} `json:"type"`
}

// AncestryRoot starts an ancestry walk at a directly referenced issue. A
// GitHub issue does not report its parent inline, so the root's parent is
// unknown until ReadWorkItemParents asks.
func (p *GitHubProvider) AncestryRoot(repo RepositoryRef, item WorkItem) WorkItemNode {
	return WorkItemNode{
		Provider: ProviderGitHub, Project: repo.Owner + "/" + repo.Name, ID: item.ID,
		Type: item.Type, Title: item.Title, URL: item.URL, ParentUnknown: true, Integrity: item.Integrity,
	}
}

// ReadWorkItemParents reads each child's parent through GitHub's native
// sub-issue relation (GET /repos/{owner}/{repo}/issues/{n}/parent), one call
// per child, in the child's own repository. A 404 there means the issue has
// no parent: the child itself was already read.
func (p *GitHubProvider) ReadWorkItemParents(ctx context.Context, _ RepositoryRef, children []WorkItemNode, fields []string) ([]WorkItemParentRead, error) {
	reads := make([]WorkItemParentRead, len(children))
	for i, child := range children {
		read, err := p.readGitHubParent(ctx, child, fields)
		if err != nil {
			return nil, err
		}
		reads[i] = read
	}
	return reads, nil
}

func (p *GitHubProvider) readGitHubParent(ctx context.Context, child WorkItemNode, fields []string) (WorkItemParentRead, error) {
	owner, name, ok := strings.Cut(child.Project, "/")
	if !ok || owner == "" || name == "" {
		return WorkItemParentRead{Omission: AncestryOmitReadFailed, Detail: "child has no owner/name repository"}, nil
	}
	endpoint, err := joinURL(p.BaseURL, "repos", owner, name, "issues", child.ID, "parent")
	if err != nil {
		return WorkItemParentRead{Omission: AncestryOmitReadFailed, Detail: err.Error()}, nil
	}
	var issue githubParentIssue
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &issue); err != nil {
		if ctxErr := ancestryContextError(ctx, err); ctxErr != nil {
			return WorkItemParentRead{}, ctxErr
		}
		if IsNotFoundError(err) {
			return WorkItemParentRead{}, nil
		}
		reason, detail := AncestryReadOmission(err)
		return WorkItemParentRead{Omission: reason, Detail: detail}, nil
	}
	node := githubAncestryNode(issue, child.Project, fields)
	return WorkItemParentRead{Parent: &node, ParentID: node.ID}, nil
}

func githubAncestryNode(issue githubParentIssue, fallbackProject string, fields []string) WorkItemNode {
	project := fallbackProject
	if parts := strings.Split(strings.TrimRight(issue.RepositoryURL, "/"), "/"); len(parts) >= 2 && issue.RepositoryURL != "" {
		project = parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	itemType := "issue"
	if issue.Type != nil && issue.Type.Name != "" {
		itemType = issue.Type.Name
	}
	body := issue.Body
	return WorkItemNode{
		Provider:      ProviderGitHub,
		Project:       project,
		ID:            strconv.Itoa(issue.Number),
		Type:          itemType,
		Title:         issue.Title,
		State:         issue.State,
		URL:           issue.HTMLURL,
		Fields:        selectAncestryFields(fields, githubAncestryDefaultFields, func(name string) string { return githubAncestryField(name, body) }),
		ParentUnknown: true,
		Integrity:     apiintegrity.Unapproved,
	}
}

// githubAncestryField resolves the one field name a GitHub issue carries
// intent in. Other names have no GitHub equivalent and select nothing.
func githubAncestryField(name, body string) string {
	if strings.EqualFold(name, "body") {
		return body
	}
	return ""
}
