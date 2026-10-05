package providers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// ApplyRepositoryProposalPhase uses explicit absent-ref creation, then a pinned
// single-file push. The new-branch Pushes convenience form is not an absence CAS.
func (p *ADOProvider) ApplyRepositoryProposalPhase(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	if err := validateRepositoryProposal(in, ProviderADO); err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	if in.Proposal.Repository.Owner != p.Organization || in.Proposal.Repository.Project == "" {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	ctx, cancel := proposalContext(ctx)
	defer cancel()
	if err := proposalSourceMatches(ctx, p, in.Proposal); err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	switch in.Phase {
	case "branch":
		return p.createProposalBranch(ctx, in)
	case "commit":
		return p.createProposalCommit(ctx, in)
	case "pull-request":
		return p.createProposalPR(ctx, in)
	default:
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
}

type proposalADORefResult struct {
	Name, OldObjectID, NewObjectID, UpdateStatus string
	Success                                      bool
}

func (p *ADOProvider) createProposalBranch(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	endpoint, err := p.repoURL(in.Proposal.Repository, "refs")
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	name := "refs/heads/" + RepositoryProposalBranch(in.Proposal.CommandID)
	body := []map[string]string{{"name": name, "oldObjectId": adoZeroObjectID, "newObjectId": in.Proposal.BaseCommit}}
	result := RepositoryProposalPhaseResult{MutationAttempted: true}
	var out struct{ Value []proposalADORefResult }
	if err = p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return result, err
	}
	if len(out.Value) != 1 {
		return result, ErrRepositoryProposal
	}
	ref := out.Value[0]
	if !ref.Success || ref.UpdateStatus != "succeeded" || ref.Name != name || ref.OldObjectID != adoZeroObjectID || ref.NewObjectID != in.Proposal.BaseCommit {
		return result, ErrRepositoryProposal
	}
	result.Acknowledged, result.CommitID = true, ref.NewObjectID
	return result, nil
}

type proposalADOCommit struct {
	CommitID, Comment, TreeID string
	Parents                   []string
}

func (p *ADOProvider) createProposalCommit(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	if in.CommitID != "" {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	proposal := in.Proposal
	head, found, err := p.proposalBranch(ctx, proposal)
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	if !found || head != proposal.BaseCommit {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	endpoint, err := p.repoURL(proposal.Repository, "pushes")
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	name := "refs/heads/" + RepositoryProposalBranch(proposal.CommandID)
	body := adoPushRequest{RefUpdates: []adoRefUpdate{{Name: name, OldObjectID: proposal.BaseCommit}}, Commits: []adoCommit{{Comment: proposal.Message, Changes: []adoChange{{ChangeType: "edit", Item: map[string]string{"path": "/" + proposal.Path}, NewContent: &adoNewContent{Content: string(proposal.Content), ContentType: "rawtext"}}}}}}
	result := RepositoryProposalPhaseResult{MutationAttempted: true}
	var out struct {
		Commits    []proposalADOCommit
		RefUpdates []proposalADORefResult
	}
	if err = p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return result, err
	}
	if len(out.Commits) != 1 || len(out.RefUpdates) != 1 || !proposalADOCommitMatches(out.Commits[0], in) {
		return result, ErrRepositoryProposal
	}
	commit, ref := out.Commits[0], out.RefUpdates[0]
	if ref.Name != name || ref.OldObjectID != proposal.BaseCommit || ref.NewObjectID != commit.CommitID {
		return result, ErrRepositoryProposal
	}
	result.Acknowledged, result.CommitID, result.TreeID = true, commit.CommitID, commit.TreeID
	return result, nil
}

func proposalADOCommitMatches(commit proposalADOCommit, in RepositoryProposalPhaseInput) bool {
	return ValidSourceCommit(commit.CommitID) && ValidSourceCommit(commit.TreeID) && len(commit.Parents) == 1 && commit.Parents[0] == in.Proposal.BaseCommit && strings.TrimSuffix(commit.Comment, "\n") == strings.TrimSuffix(in.Proposal.Message, "\n")
}

func (p *ADOProvider) verifyProposalCommit(ctx context.Context, in RepositoryProposalPhaseInput) error {
	endpoint, err := p.repoURL(in.Proposal.Repository, "commits", in.CommitID)
	if err != nil {
		return err
	}
	var out proposalADOCommit
	if err = p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return err
	}
	if out.CommitID != in.CommitID || !proposalADOCommitMatches(out, in) {
		return ErrRepositoryProposal
	}
	if err := p.verifyProposalChanges(ctx, in); err != nil {
		return err
	}
	return proposalContentMatches(ctx, p, in.Proposal, in.CommitID)
}

func (p *ADOProvider) verifyProposalChanges(ctx context.Context, in RepositoryProposalPhaseInput) error {
	endpoint, err := p.repoURL(in.Proposal.Repository, "commits", in.CommitID, "changes")
	if err != nil {
		return err
	}
	endpoint, err = addQuery(endpoint, url.Values{"top": {"2"}, "skip": {"0"}})
	if err != nil {
		return err
	}
	var out struct {
		ChangeCounts map[string]int
		Changes      []struct {
			ChangeType, OriginalPath string
			Item                     struct {
				Path, GitObjectType string
				IsFolder, IsSymLink bool
			}
		}
	}
	if err = p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return err
	}
	if len(out.Changes) != 1 || len(out.ChangeCounts) > 16 {
		return ErrRepositoryProposal
	}
	count := 0
	for kind, value := range out.ChangeCounts {
		if value < 0 || value > 1 || (value != 0 && kind != "Edit") {
			return ErrRepositoryProposal
		}
		count += value
	}
	change := out.Changes[0]
	if count != 1 || change.ChangeType != "edit" || change.OriginalPath != "" || change.Item.Path != "/"+in.Proposal.Path || change.Item.GitObjectType != "blob" || change.Item.IsFolder || change.Item.IsSymLink {
		return ErrRepositoryProposal
	}
	return nil
}

func (p *ADOProvider) proposalBranch(ctx context.Context, proposal RepositoryProposal) (string, bool, error) {
	ref, found, err := p.GetBranch(ctx, proposal.Repository, RepositoryProposalBranch(proposal.CommandID))
	if err != nil || !found {
		return "", found, err
	}
	if !ValidSourceCommit(ref.SHA) {
		return "", false, ErrRepositoryProposal
	}
	return ref.SHA, true, nil
}

// ObserveRepositoryProposalPhase does not require the base branch still to be
// current: exact prior effects remain inspectable after source advancement.
func (p *ADOProvider) ObserveRepositoryProposalPhase(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalObservation, error) {
	if err := validateRepositoryProposal(in, ProviderADO); err != nil {
		return RepositoryProposalObservation{}, err
	}
	if in.Proposal.Repository.Owner != p.Organization || in.Proposal.Repository.Project == "" {
		return RepositoryProposalObservation{}, ErrRepositoryProposal
	}
	ctx, cancel := proposalContext(ctx)
	defer cancel()
	switch in.Phase {
	case "branch":
		commit, found, err := p.proposalBranch(ctx, in.Proposal)
		return RepositoryProposalObservation{Found: found, Matches: found && commit == in.Proposal.BaseCommit, CommitID: commit}, err
	case "commit":
		if in.CommitID == "" {
			commit, found, err := p.proposalBranch(ctx, in.Proposal)
			if err != nil || !found || commit == in.Proposal.BaseCommit {
				return RepositoryProposalObservation{Found: found, CommitID: commit}, err
			}
			in.CommitID = commit
		}
		err := p.verifyProposalCommit(ctx, in)
		return RepositoryProposalObservation{Found: err == nil, Matches: err == nil, CommitID: in.CommitID}, err
	case "pull-request":
		return p.observeProposalPR(ctx, in)
	default:
		return RepositoryProposalObservation{}, ErrRepositoryProposal
	}
}
