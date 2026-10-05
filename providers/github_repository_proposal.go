package providers

import (
	"context"
	"net/http"
	"strings"
)

// ApplyRepositoryProposalPhase sends at most one GitHub mutation request. It
// never calls the broader Contents or find-and-update pull request helpers.
func (p *GitHubProvider) ApplyRepositoryProposalPhase(ctx context.Context, input RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	if err := validateRepositoryProposal(input, ProviderGitHub); err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	ctx, cancel := proposalContext(ctx)
	defer cancel()
	if err := proposalSourceMatches(ctx, p, input.Proposal); err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	switch input.Phase {
	case "tree":
		return p.createProposalTree(ctx, input)
	case "commit":
		return p.createProposalCommit(ctx, input)
	case "branch":
		return p.createProposalBranch(ctx, input)
	case "pull-request":
		return p.createProposalPR(ctx, input)
	default:
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
}

func (p *GitHubProvider) createProposalTree(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	if in.TreeID != "" {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	repo, proposal := in.Proposal.Repository, in.Proposal
	root, err := p.sourceCommitTree(ctx, repo, proposal.BaseCommit)
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	entry, err := p.sourcePathEntry(ctx, repo, root, strings.Split(proposal.Path, "/"))
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	if entry.SHA != proposal.PreviousBlob {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "trees")
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	body := map[string]interface{}{"base_tree": root, "tree": []map[string]string{{"path": proposal.Path, "mode": entry.Mode, "type": "blob", "content": string(proposal.Content)}}}
	result := RepositoryProposalPhaseResult{MutationAttempted: true}
	var out struct{ SHA string }
	if err = p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return result, err
	}
	if !ValidSourceCommit(out.SHA) {
		return result, ErrRepositoryProposal
	}
	result.Acknowledged, result.TreeID = true, out.SHA
	return result, nil
}

type proposalGitHubCommit struct {
	SHA, Message string
	Tree         struct{ SHA string }
	Parents      []struct{ SHA string }
}

func (p *GitHubProvider) createProposalCommit(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	if in.CommitID != "" {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	repo, proposal := in.Proposal.Repository, in.Proposal
	entry, err := p.sourcePathEntry(ctx, repo, in.TreeID, strings.Split(proposal.Path, "/"))
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	if entry.SHA != sourceBlobID(proposal.Content) {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "commits")
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	body := map[string]interface{}{"message": proposal.Message, "tree": in.TreeID, "parents": []string{proposal.BaseCommit}}
	result := RepositoryProposalPhaseResult{MutationAttempted: true}
	var out proposalGitHubCommit
	if err = p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return result, err
	}
	if !proposalGitHubCommitMatches(out, in) {
		return result, ErrRepositoryProposal
	}
	result.Acknowledged, result.CommitID, result.TreeID = true, out.SHA, out.Tree.SHA
	return result, nil
}

func proposalGitHubCommitMatches(out proposalGitHubCommit, in RepositoryProposalPhaseInput) bool {
	return ValidSourceCommit(out.SHA) && out.Tree.SHA == in.TreeID && len(out.Parents) == 1 && out.Parents[0].SHA == in.Proposal.BaseCommit && strings.TrimSuffix(out.Message, "\n") == strings.TrimSuffix(in.Proposal.Message, "\n")
}

func (p *GitHubProvider) createProposalBranch(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	if err := p.verifyProposalCommit(ctx, in); err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	repo := in.Proposal.Repository
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "refs")
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	ref := "refs/heads/" + RepositoryProposalBranch(in.Proposal.CommandID)
	result := RepositoryProposalPhaseResult{MutationAttempted: true}
	var out githubRef
	if err = p.do(ctx, http.MethodPost, endpoint, map[string]string{"ref": ref, "sha": in.CommitID}, &out); err != nil {
		return result, err
	}
	if out.Ref != ref || out.Object.SHA != in.CommitID {
		return result, ErrRepositoryProposal
	}
	result.Acknowledged, result.CommitID = true, out.Object.SHA
	return result, nil
}

func (p *GitHubProvider) verifyProposalCommit(ctx context.Context, in RepositoryProposalPhaseInput) error {
	repo := in.Proposal.Repository
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "commits", in.CommitID)
	if err != nil {
		return err
	}
	var out proposalGitHubCommit
	if err = p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return err
	}
	if out.SHA != in.CommitID || !proposalGitHubCommitMatches(out, in) {
		return ErrRepositoryProposal
	}
	return proposalContentMatches(ctx, p, in.Proposal, in.CommitID)
}

func (p *GitHubProvider) proposalBranch(ctx context.Context, proposal RepositoryProposal) (string, bool, error) {
	repo := proposal.Repository
	name := RepositoryProposalBranch(proposal.CommandID)
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "ref", "heads", name)
	if err != nil {
		return "", false, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	if response.StatusCode == http.StatusNotFound {
		_ = response.Body.Close()
		return "", false, nil
	}
	var out githubRef
	if err = readJSONResponse(response, http.MethodGet, endpoint, &out); err != nil {
		return "", false, err
	}
	if out.Ref != "refs/heads/"+name || !ValidSourceCommit(out.Object.SHA) {
		return "", false, ErrRepositoryProposal
	}
	return out.Object.SHA, true, nil
}

// ObserveRepositoryProposalPhase reads only exact retained objects. Unknown
// immutable tree/commit creation cannot be reconstructed through a fresh write.
func (p *GitHubProvider) ObserveRepositoryProposalPhase(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalObservation, error) {
	if err := validateRepositoryProposal(in, ProviderGitHub); err != nil {
		return RepositoryProposalObservation{}, err
	}
	ctx, cancel := proposalContext(ctx)
	defer cancel()
	switch in.Phase {
	case "tree":
		if in.TreeID == "" {
			return RepositoryProposalObservation{}, nil
		}
		entry, err := p.sourcePathEntry(ctx, in.Proposal.Repository, in.TreeID, strings.Split(in.Proposal.Path, "/"))
		return RepositoryProposalObservation{Found: err == nil, Matches: err == nil && entry.SHA == sourceBlobID(in.Proposal.Content), TreeID: in.TreeID}, err
	case "commit":
		if in.CommitID == "" {
			return RepositoryProposalObservation{}, nil
		}
		err := p.verifyProposalCommit(ctx, in)
		return RepositoryProposalObservation{Found: err == nil, Matches: err == nil, CommitID: in.CommitID, TreeID: in.TreeID}, err
	case "branch":
		commit, found, err := p.proposalBranch(ctx, in.Proposal)
		return RepositoryProposalObservation{Found: found, Matches: found && commit == in.CommitID, CommitID: commit}, err
	case "pull-request":
		return p.observeProposalPR(ctx, in)
	default:
		return RepositoryProposalObservation{}, ErrRepositoryProposal
	}
}
