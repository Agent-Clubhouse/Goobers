package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// ErrInsufficientRepositoryPermission reports that the credential authenticated
// but GitHub's repository metadata says it lacks read (pull) permission.
var ErrInsufficientRepositoryPermission = errors.New("provider credential lacks repository read permission")

// VerifyRepositoryReadAccess performs a non-mutating authorization health
// check for repo (#5317): it reads the repository metadata and one page of
// issues. A nil return means the credential is valid and can read both. It
// never writes, so it is safe to call before any claim or dispatch.
func (p *GitHubProvider) VerifyRepositoryReadAccess(ctx context.Context, repo RepositoryRef) error {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	var meta struct {
		Permissions *struct {
			Pull bool `json:"pull"`
		} `json:"permissions"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &meta); err != nil {
		return err
	}
	if meta.Permissions != nil && !meta.Permissions.Pull {
		return ErrInsufficientRepositoryPermission
	}
	issues, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues")
	if err != nil {
		return err
	}
	issues, err = addQuery(issues, url.Values{"per_page": []string{"1"}})
	if err != nil {
		return err
	}
	var page []struct{}
	return p.do(ctx, http.MethodGet, issues, nil, &page)
}
