package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

func (p *ADOProvider) createProposalPR(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error) {
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
	proposal := in.Proposal
	endpoint, err := p.repoURL(proposal.Repository, "pullrequests")
	if err != nil {
		return RepositoryProposalPhaseResult{}, err
	}
	body := map[string]interface{}{"title": proposal.Title, "description": proposal.Body, "sourceRefName": "refs/heads/" + RepositoryProposalBranch(proposal.CommandID), "targetRefName": "refs/heads/" + proposal.BaseBranch, "isDraft": true}
	result := RepositoryProposalPhaseResult{MutationAttempted: true}
	var out adoPullRequestDetail
	if err = p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return result, err
	}
	if !proposalADOPRMatches(out, in) || !out.IsDraft || out.Status != "active" {
		return result, ErrRepositoryProposal
	}
	pr := p.proposalADOPRResult(proposal.Repository, out.PullRequestID)
	result.Acknowledged, result.CommitID, result.PullRequest = true, in.CommitID, &pr
	return result, nil
}

func proposalADOPRMatches(pr adoPullRequestDetail, in RepositoryProposalPhaseInput) bool {
	proposal := in.Proposal
	return pr.PullRequestID > 0 && pr.Title == proposal.Title && pr.Description == proposal.Body && pr.SourceRefName == "refs/heads/"+RepositoryProposalBranch(proposal.CommandID) && pr.TargetRefName == "refs/heads/"+proposal.BaseBranch && pr.LastMergeSourceCommit.CommitID == in.CommitID && pr.Repository.Name == proposal.Repository.Name && pr.Repository.Project.Name == proposal.Repository.Project && (pr.Status == "active" || pr.Status == "completed" || pr.Status == "abandoned")
}

func (p *ADOProvider) proposalADOPRResult(repo RepositoryRef, number int) PullRequestResult {
	return PullRequestResult{ID: strconv.Itoa(number), Number: number, URL: p.webURL(repo.Project, "_git", repo.Name, "pullrequest", strconv.Itoa(number))}
}

func (p *ADOProvider) observeProposalPR(ctx context.Context, in RepositoryProposalPhaseInput) (RepositoryProposalObservation, error) {
	proposal := in.Proposal
	endpoint, err := p.repoURL(proposal.Repository, "pullrequests")
	if err != nil {
		return RepositoryProposalObservation{}, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"searchCriteria.sourceRefName": {"refs/heads/" + RepositoryProposalBranch(proposal.CommandID)}, "searchCriteria.targetRefName": {"refs/heads/" + proposal.BaseBranch}, "searchCriteria.status": {"all"}, "$top": {"2"}})
	if err != nil {
		return RepositoryProposalObservation{}, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return RepositoryProposalObservation{}, err
	}
	continuation := response.Header.Get("x-ms-continuationtoken")
	var out struct{ Value []adoPullRequestDetail }
	if err = readJSONResponse(response, http.MethodGet, endpoint, &out); err != nil {
		return RepositoryProposalObservation{}, err
	}
	if len(out.Value) > 1 || continuation != "" {
		return RepositoryProposalObservation{}, ErrRepositoryProposal
	}
	if len(out.Value) == 0 {
		return RepositoryProposalObservation{}, nil
	}
	if !proposalADOPRMatches(out.Value[0], in) {
		return RepositoryProposalObservation{Found: true}, ErrRepositoryProposal
	}
	pr := p.proposalADOPRResult(proposal.Repository, out.Value[0].PullRequestID)
	return RepositoryProposalObservation{Found: true, Matches: true, CommitID: in.CommitID, PullRequest: &pr}, nil
}
