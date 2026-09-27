package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// Topology (b) keeps the backlog on a different provider than the code
// (docs/design/ado-parity-dsl-2-0.md §7.2). A pull request then lives on one
// provider and the item it resolves on another, so the "#<id>" shorthand is
// wrong in both directions: on Azure DevOps "#<id>" in a pull request body or
// squash commit message names ADO work item <id>, and completing the pull
// request can transition it. Everything below writes and reads the item's
// full URL instead whenever the backlog is on another provider, and keeps
// "#<id>" byte-identical for every same-provider gaggle.

// prIssueReference is how a pull request on repo refers to backlog item
// issueID: "#<id>" when the backlog shares repo's provider, and the item's
// full URL when it does not.
func prIssueReference(root string, repo providers.RepositoryRef, issueID string) string {
	backlog := backlogRepoRefForStage(root, repo)
	if !backlogOnOtherProvider(repo, backlog) {
		return "#" + issueID
	}
	return backlogIssueURL(backlog, issueID)
}

// backlogIssueURL is the web URL of issue id in a GitHub or Gitea backlog
// repository: the backlog's base URL (github.com when none is declared),
// then owner/name/issues/id.
func backlogIssueURL(backlog providers.RepositoryRef, id string) string {
	base := strings.TrimRight(strings.TrimSpace(backlog.URL), "/")
	if base == "" {
		base = "https://github.com"
	}
	return base + "/" + backlog.Owner + "/" + backlog.Name + "/issues/" + id
}

// closingIssueURLs extracts, in first-seen order, the ids of the backlog
// issues a pull request body closes by full URL ("Fixes <backlog issue
// URL>"), the form prIssueReference writes in topology (b). Only URLs that
// point into backlog count, so a closing reference to any other repository
// never closes an item here.
func closingIssueURLs(body string, backlog providers.RepositoryRef) []string {
	pattern := regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+` +
		regexp.QuoteMeta(backlogIssueURL(backlog, "")) + `(\d+)\b`)
	return distinctIssueRefs(pattern, body)
}

// postMergeClosingIDs is the set of backlog items a merged pull request on
// repo closes. On a same-provider gaggle it is closingIssueNumbers exactly. In
// topology (b) only full-URL references into the backlog count: a bare
// "#<id>" there names an item of the pull request's own provider, and closing
// the backlog item with that id would close the wrong object.
func postMergeClosingIDs(body string, repo, backlog providers.RepositoryRef) []string {
	if !backlogOnOtherProvider(repo, backlog) {
		return closingIssueNumbers(body)
	}
	return closingIssueURLs(body, backlog)
}

// mergeCommitClosingRefs renders the closing references a merge commit
// message carries for a pull request on repo: "Closes #<id>" on a
// same-provider gaggle, as before, and "Closes <backlog issue URL>" in
// topology (b), so a squash message never carries a bare "#<id>" that the
// pull request's provider would resolve to one of its own items.
func mergeCommitClosingRefs(body string, repo, backlog providers.RepositoryRef) []string {
	ids := postMergeClosingIDs(body, repo, backlog)
	refs := make([]string, 0, len(ids))
	for _, id := range ids {
		ref := "#" + id
		if backlogOnOtherProvider(repo, backlog) {
			ref = backlogIssueURL(backlog, id)
		}
		refs = append(refs, "Closes "+ref)
	}
	return refs
}

// workItemGetter is the one read open-pr's staleness re-check makes.
type workItemGetter interface {
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
}

// openPRWorkItem reads the claimed item for open-pr's staleness re-check. A
// backlog on the routed provider is read through the stage's own provider, as
// before. In topology (b) the item is on the backlog provider, which the
// stage's pull-request credential cannot read, so it is read with the stage's
// issue credential; a stage that declares none gets an error, which the
// caller's fail-open re-check reports and proceeds past.
func openPRWorkItem(ctx context.Context, root string, repo, backlog providers.RepositoryRef, routed workItemGetter, id string) (providers.WorkItem, error) {
	if !backlogOnOtherProvider(repo, backlog) {
		return routed.GetWorkItem(ctx, backlog, id)
	}
	issueCapability, ok := declaredBacklogReadCapability()
	if !ok {
		return providers.WorkItem{}, fmt.Errorf("the backlog is on %s and this stage declares no github:issues:read capability to read it", backlog.Provider)
	}
	reader, err := newProviderForStage(root, backlog, true, withStageProviderCapability(issueCapability))
	if err != nil {
		return providers.WorkItem{}, err
	}
	return reader.GetWorkItem(ctx, backlog, id)
}

// declaredBacklogReadCapability is the first issue capability the stage
// declared that can read a backlog item: github:issues:read, else
// github:issues:write. Each providerToken call names its capability as a
// constant so the provider-capability drift check can resolve it.
func declaredBacklogReadCapability() (capability.Capability, bool) {
	if _, err := providerToken(capability.GitHubIssuesRead); err == nil {
		return capability.GitHubIssuesRead, true
	}
	if _, err := providerToken(capability.GitHubIssuesWrite); err == nil {
		return capability.GitHubIssuesWrite, true
	}
	return "", false
}
