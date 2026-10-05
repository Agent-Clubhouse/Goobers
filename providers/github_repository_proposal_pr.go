package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

type proposalGitHubPR struct {
	Number             int
	Title, Body, State string
	Draft              bool
	Head               struct {
		Ref, SHA string
		Repo     struct {
			FullName string `json:"full_name"`
		}
	}
	Base struct {
		Ref  string
		Repo struct {
			FullName string `json:"full_name"`
		}
	}
}

func (p *GitHubProvider) createProposalPR(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
	head, found, err := p.proposalBranch(ctx, in.Proposal)
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	if !found || head != in.CommitID {
		return RepositoryProposalPhaseResult{}, ErrRepositoryProposal
	}
	if err := p.verifyProposalCommit(ctx, in); err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	proposal, repo := in.Proposal, in.Proposal.Repository
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls")
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	body := map[string]interface{}{"title": proposal.Title, "body": proposal.Body, "head": RepositoryProposalBranch(proposal.CommandID), "base": proposal.BaseBranch, "draft": true}
	result := RepositoryProposalPhaseResult{MutationAttempted: true}
	var out proposalGitHubPR
	if err = p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return result, err
	}
	if !proposalGitHubPRMatches(out, in) || !out.Draft || out.State != "open" {
		return result, ErrRepositoryProposal
	}
	pr := proposalGitHubPRResult(repo, out.Number)
	result.Acknowledged, result.CommitID, result.PullRequest = true, in.CommitID, &pr
	return result, nil
}

func proposalGitHubPRMatches(pr proposalGitHubPR, in RepositoryProposalPhaseInput) bool {
	proposal := in.Proposal
	repo := proposal.Repository.Owner + "/" + proposal.Repository.Name
	return pr.Number > 0 && pr.Title == proposal.Title && pr.Body == proposal.Body && pr.Head.Ref == RepositoryProposalBranch(proposal.CommandID) && pr.Head.SHA == in.CommitID && pr.Head.Repo.FullName == repo && pr.Base.Ref == proposal.BaseBranch && pr.Base.Repo.FullName == repo && (pr.State == "open" || pr.State == "closed")
}

func proposalGitHubPRResult(repo RepositoryRef, number int) PullRequestResult {
	return PullRequestResult{ID: strconv.Itoa(number), Number: number, URL: "https://github.com/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pull/" + strconv.Itoa(number)}
}

func (p *GitHubProvider) observeProposalPR(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalObservation, error) {
	repo := in.Proposal.Repository
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls")
	if err != nil {
		return RepositoryProposalObservation{}, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"head": {repo.Owner + ":" + RepositoryProposalBranch(in.Proposal.CommandID)}, "base": {in.Proposal.BaseBranch}, "state": {"all"}, "per_page": {"2"}})
	if err != nil {
		return RepositoryProposalObservation{}, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RepositoryProposalObservation{}, err
	}
	link := response.Header.Get("Link")
	var out []proposalGitHubPR
	if err = readJSONResponse(response, http.MethodGet, endpoint, &out); err != nil {
		return RepositoryProposalObservation{}, err
	}
	if len(out) > 1 || link != "" {
		return RepositoryProposalObservation{}, ErrRepositoryProposal
	}
	if len(out) == 0 {
		return RepositoryProposalObservation{}, nil
	}
	if !proposalGitHubPRMatches(out[0], in) {
		return RepositoryProposalObservation{Found: true}, ErrRepositoryProposal
	}
	pr := proposalGitHubPRResult(repo, out[0].Number)
	return RepositoryProposalObservation{Found: true, Matches: true, CommitID: in.CommitID, PullRequest: &pr}, nil
}
