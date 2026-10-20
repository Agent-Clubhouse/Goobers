package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

const prSelectConflictMarker = "<!-- pr-select-conflict: "

type prSelectConflictProvider interface {
	GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error)
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

type prSelectConflictSource interface {
	conflictProvider() prSelectConflictProvider
}

func (s refCheckPRSelectSource) conflictProvider() prSelectConflictProvider     { return s.provider }
func (s branchPolicyPRSelectSource) conflictProvider() prSelectConflictProvider { return s.conflicts }

// ADO's detail response does not reliably include labels. Re-read them before
// a handoff so a human hold cannot disappear behind a list/detail projection.
type prSelectADOConflictProvider struct{ mergeReviewADOProvider }

func (p prSelectADOConflictProvider) GetPullRequest(ctx context.Context, repo providers.RepositoryRef, id string) (providers.PullRequestSummary, error) {
	pr, err := p.ADOProvider.GetPullRequest(ctx, repo, id)
	if err != nil {
		return pr, err
	}
	pr.Labels, err = p.PullRequestLabelNames(ctx, repo, id)
	return pr, err
}

// excludePRSelectConflict uses only a confirmed false; unknown computation is
// still eligible. Non-owned PRs stay read-only. Existing eligibility gates run
// first, and managed handoffs recheck head and holds before any mutation.
func excludePRSelectConflict(ctx context.Context, source prSelectSource, repo providers.RepositoryRef, pr providers.PullRequestSummary, managed bool) (bool, error) {
	access, ok := source.(prSelectConflictSource)
	if !ok || access.conflictProvider() == nil {
		return pr.Mergeable != nil && !*pr.Mergeable, nil
	}
	provider := access.conflictProvider()
	id := strconv.Itoa(pr.Number)
	if pr.Mergeable == nil && repo.Provider != providers.ProviderADO {
		fresh, err := provider.GetPullRequest(ctx, repo, id)
		if err != nil {
			return false, err
		}
		if fresh.HeadSHA != pr.HeadSHA {
			return false, fmt.Errorf("PR #%s head changed while checking mergeability; retry selection", id)
		}
		pr.Mergeable = fresh.Mergeable
	}
	if pr.Mergeable == nil || *pr.Mergeable {
		return false, nil
	}
	if !managed || prSelectConflictHeld(pr) {
		return true, nil
	}
	fresh, err := provider.GetPullRequest(ctx, repo, id)
	if err != nil {
		return true, err
	}
	if fresh.HeadSHA != pr.HeadSHA || fresh.HeadSHA == "" {
		return true, fmt.Errorf("PR #%s head changed or is unknown; retry conflict handoff", id)
	}
	if fresh.State != "open" || fresh.Draft || prSelectConflictHeld(fresh) || fresh.Mergeable == nil || *fresh.Mergeable {
		return true, nil
	}
	return true, handoffPRSelectConflict(ctx, provider, repo, fresh)
}

func prSelectConflictHeld(pr providers.PullRequestSummary) bool {
	return hasAnyLabel(pr.Labels, []string{
		providers.LabelNeedsHuman, remediationEscalatedLabel, mergeDemotedLabel,
		scopeGateLabel, noMergeReviewLabel, abortedRunLabel, mergeReadyLabel,
	})
}

func handoffPRSelectConflict(ctx context.Context, provider prSelectConflictProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) error {
	id := strconv.Itoa(pr.Number)
	comments, err := provider.ListComments(ctx, repo, id)
	if err != nil {
		return err
	}
	marker := prSelectConflictMarker + pr.HeadSHA + " -->"
	posted := false
	for _, comment := range comments {
		for _, line := range strings.Split(comment.Body, "\n") {
			if line == marker {
				posted = true
			}
		}
	}
	if !posted {
		state := "mergeable=false"
		if repo.Provider == providers.ProviderADO {
			state = "mergeStatus=conflicts"
		}
		body := fmt.Sprintf("The forge reports a merge conflict (`%s`) for head `%s`. "+
			"Excluded from merge review and handed to remediation to resolve the branch conflict.\n\n%s",
			state, pr.HeadSHA, marker)
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: repo, ID: id, Comment: body}); err != nil {
			return err
		}
	}
	// A failed label write can be retried without duplicating the visible note.
	if !hasAnyLabel(pr.Labels, []string{needsRemediationLabel}) {
		_, err = provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: id, AddLabels: []string{needsRemediationLabel},
		})
	}
	return err
}
