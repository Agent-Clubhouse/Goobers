package providers

import (
	"fmt"
	"strconv"
	"strings"
)

// WithADOPollScope binds a pod poller to its admitted organization/project/repo.
// A stage has no instance config from which to infer missing identity. Strict
// validation occurs before requests and before trusting returned policy scope.
func WithADOPollScope(repo RepositoryRef) func(*ADOProvider) {
	return func(p *ADOProvider) { p.pollScope = &repo }
}

func (p *ADOProvider) validatePollScope(req PullRequestPollRequest) error {
	if p.pollScope == nil {
		return nil
	}
	scope := *p.pollScope
	if scope.Owner == "" || scope.Project == "" || scope.Name == "" ||
		!strings.EqualFold(p.Organization, scope.Owner) || !strings.EqualFold(p.Project, scope.Project) ||
		!strings.EqualFold(req.Repository.Owner, scope.Owner) || !strings.EqualFold(req.Repository.Name, scope.Name) ||
		req.Repository.Project != "" && !strings.EqualFold(req.Repository.Project, scope.Project) {
		return fmt.Errorf("ADO ci-poll request does not match its admitted organization/project/repository")
	}
	if id, err := strconv.Atoi(req.PullID); err != nil || id <= 0 {
		return fmt.Errorf("ADO ci-poll requires a positive pull request number")
	}
	return nil
}

func (p *ADOProvider) validatePolledIdentity(req PullRequestPollRequest, pr adoPullRequestDetail) error {
	if p.pollScope == nil {
		return nil
	}
	id, _ := strconv.Atoi(req.PullID) // validatePollScope checked it before the request.
	repo := pr.Repository
	if pr.PullRequestID != id || repo.ID == "" || repo.Name == "" || repo.Project.ID == "" || repo.Project.Name == "" ||
		!adoScopeIdentityMatches(p.pollScope.Name, repo.Name, repo.ID) || !adoScopeIdentityMatches(p.pollScope.Project, repo.Project.Name, repo.Project.ID) ||
		p.pollScope.ID != "" && !strings.EqualFold(repo.ID, p.pollScope.ID) {
		return fmt.Errorf("ADO ci-poll response does not match its admitted pull request/repository/project")
	}
	switch strings.ToLower(pr.Status) {
	case "active", "completed", "abandoned":
		return nil
	default:
		return fmt.Errorf("ADO ci-poll response has no recognized pull request lifecycle")
	}
}

func adoScopeIdentityMatches(admitted, name, id string) bool {
	return strings.EqualFold(admitted, name) || strings.EqualFold(admitted, id)
}
