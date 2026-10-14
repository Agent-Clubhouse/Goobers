package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Azure DevOps run-branch cleanup (#5957).
//
// ADO is asked to delete a landed pull request's source branch through
// completionOptions.deleteSourceBranch (ADO-N25), but it does not always do
// so: a validation soak saw the ref survive 2 of 58 clean auto-completions,
// still at the merged head with no later push. Separately, a run that ended
// without a pull request left its pushed branch behind, because terminal
// cleanup had no Azure DevOps arm. Both cleanups live here, over the refs
// update API (DeleteBranch), and every delete is conditional: the ref update
// names the expected oldObjectId, so a branch that moved is kept.

// MergedBranchCleanupStatus is the outcome of CleanupMergedSourceBranch.
type MergedBranchCleanupStatus string

const (
	// MergedBranchDeleted means the source branch is gone, whether Azure DevOps
	// removed it or the cleanup did.
	MergedBranchDeleted MergedBranchCleanupStatus = "deleted"
	// MergedBranchSkippedStacked means another open pull request targets the
	// branch, so deleting it would break that pull request (PRL-072).
	MergedBranchSkippedStacked MergedBranchCleanupStatus = "skipped-stacked"
	// MergedBranchSkippedMoved means the branch no longer points at the merged
	// head; a later push is kept.
	MergedBranchSkippedMoved MergedBranchCleanupStatus = "skipped-moved"
)

// BranchInUseError reports a branch kept because an open pull request uses
// it: as its base (a stacked pull request, AsBase) or as its source.
type BranchInUseError struct {
	Name   string
	PullID int
	AsBase bool
}

func (e *BranchInUseError) Error() string {
	if e.AsBase {
		return fmt.Sprintf("branch %q is the base of open pull request #%d", e.Name, e.PullID)
	}
	return fmt.Sprintf("branch %q is the source of open pull request #%d", e.Name, e.PullID)
}

// CleanupMergedSourceBranch confirms that a merged pull request's source
// branch is gone, and deletes it when Azure DevOps left it behind.
// mergedHeadSHA is the head the pull request merged at, and it is required:
// without it a branch that moved after the merge could not be told from the
// merged one, so the cleanup refuses rather than delete whatever tip it reads.
// A branch that moved off the merged head is kept (MergedBranchSkippedMoved),
// and so is one another open pull request targets (MergedBranchSkippedStacked).
// A lost lease is read again, to tell Azure DevOps' own concurrent delete from
// a push.
func (p *ADOProvider) CleanupMergedSourceBranch(ctx context.Context, repo RepositoryRef, branch, mergedHeadSHA string) (MergedBranchCleanupStatus, error) {
	if err := requireRepo(repo); err != nil {
		return "", err
	}
	branch = strings.TrimPrefix(branch, adoHeadsRefPrefix)
	if strings.TrimSpace(branch) == "" {
		return "", errors.New("merged pull request did not report a head branch")
	}
	if strings.TrimSpace(mergedHeadSHA) == "" {
		return "", fmt.Errorf("merged head of branch %q is unknown; source branch kept", branch)
	}
	ref, found, err := p.findHeadRef(ctx, repo, branch)
	if err != nil {
		return "", fmt.Errorf("read branch %q: %w", branch, err)
	}
	if !found {
		return MergedBranchDeleted, nil
	}
	if !strings.EqualFold(ref.ObjectID, mergedHeadSHA) {
		return MergedBranchSkippedMoved, nil
	}
	stacked, err := p.ListPullRequests(ctx, ListPullRequestsRequest{Repository: repo, Base: branch, SkipCheckState: true})
	if err != nil {
		return "", fmt.Errorf("check stacked pull requests for %q: %w", branch, err)
	}
	if len(stacked) > 0 {
		return MergedBranchSkippedStacked, nil
	}
	_, err = p.DeleteBranch(ctx, DeleteBranchRequest{Repository: repo, Name: branch, ExpectedSHA: ref.ObjectID})
	var moved *BranchTipChangedError
	switch {
	case err == nil:
		return MergedBranchDeleted, nil
	case errors.As(err, &moved):
		_, stillThere, readErr := p.findHeadRef(ctx, repo, branch)
		if readErr != nil {
			return "", fmt.Errorf("re-read branch %q after a lost delete lease: %w", branch, readErr)
		}
		if !stillThere {
			return MergedBranchDeleted, nil
		}
		return MergedBranchSkippedMoved, nil
	default:
		return "", fmt.Errorf("delete branch %q: %w", branch, err)
	}
}

// DeleteUnusedBranch deletes a run branch no open pull request uses, for
// terminal cleanup of a run that ended without merging. It refuses with a
// *BranchInUseError when an active pull request has the branch as its base
// (a stacked pull request, PRL-072) or as its source (a pull request opened
// earlier in a resumed or continued run, or by a person); one list of the
// repository's active pull requests answers both. The delete itself is
// DeleteBranch, conditional on the tip it reads (or req.ExpectedSHA).
func (p *ADOProvider) DeleteUnusedBranch(ctx context.Context, req DeleteBranchRequest) (DeleteBranchResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return DeleteBranchResult{}, err
	}
	name := strings.TrimPrefix(req.Name, adoHeadsRefPrefix)
	if name == "" {
		return DeleteBranchResult{}, fmt.Errorf("branch name is required")
	}
	active, err := p.ListPullRequests(ctx, ListPullRequestsRequest{Repository: req.Repository, SkipCheckState: true})
	if err != nil {
		return DeleteBranchResult{}, fmt.Errorf("check open pull requests for %q: %w", name, err)
	}
	for _, pr := range active {
		switch name {
		case strings.TrimPrefix(pr.Base, adoHeadsRefPrefix):
			return DeleteBranchResult{}, &BranchInUseError{Name: name, PullID: pr.Number, AsBase: true}
		case strings.TrimPrefix(pr.Head, adoHeadsRefPrefix):
			return DeleteBranchResult{}, &BranchInUseError{Name: name, PullID: pr.Number}
		}
	}
	req.Name = name
	return p.DeleteBranch(ctx, req)
}

// adoTokenSourceCredential sends the value a TokenSource resolves as a
// credential of a fixed kind, resolving it again for every request so an
// expiring Microsoft Entra token refreshes through its source.
type adoTokenSourceCredential struct {
	kind   string
	source TokenSource
}

// NewADOTokenSourceCredentialSource returns an Azure DevOps credential source
// over source, sent as kind (ADOCredentialKindPAT or ADOCredentialKindBearer).
// The daemon uses it for a capability token it materialized itself, such as
// terminal branch cleanup's github:branch:delete.
func NewADOTokenSourceCredentialSource(kind string, source TokenSource) (ADOCredentialSource, error) {
	switch kind {
	case adoCredentialPAT, adoCredentialBearer:
	default:
		return nil, fmt.Errorf("unsupported ado credential kind %q", kind)
	}
	if source == nil {
		return nil, errors.New("ado token-source credential has no source")
	}
	return adoTokenSourceCredential{kind: kind, source: source}, nil
}

func (c adoTokenSourceCredential) Credential(ctx context.Context) (ADOCredential, error) {
	token, err := c.source.Token(ctx)
	if err != nil {
		return ADOCredential{}, err
	}
	return ADOCredential{Kind: c.kind, Secret: token}, nil
}
