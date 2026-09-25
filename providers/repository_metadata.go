package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// RepositoryMetadata is the canonical identity read from a configured route.
// ServiceRoot is separate from the repository's browser or clone URL.
type RepositoryMetadata struct {
	ServiceRoot string
	Repository  RepositoryRef
	ProjectID   string
}

// ReadRepository reads canonical metadata through the provider's configured API.
func (p *GitHubProvider) ReadRepository(ctx context.Context, repo RepositoryRef) (RepositoryMetadata, error) {
	root := strings.TrimSuffix(strings.TrimRight(p.BaseURL, "/"), "/api/v3")
	if root == "https://api.github.com" {
		root = "https://github.com"
	}
	return readRESTRepository(ctx, p, p.BaseURL, root, ProviderGitHub, repo)
}

// ReadRepository reads canonical metadata through the provider's configured API.
func (p *GiteaProvider) ReadRepository(ctx context.Context, repo RepositoryRef) (RepositoryMetadata, error) {
	return readRESTRepository(ctx, p, p.BaseURL, p.RootURL, ProviderGitea, repo)
}

func readRESTRepository(ctx context.Context, client restDoer, apiRoot, serviceRoot string, kind ProviderKind, repo RepositoryRef) (RepositoryMetadata, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return RepositoryMetadata{}, err
	}
	endpoint, err := joinURL(apiRoot, "repos", repo.Owner, repo.Name)
	if err != nil {
		return RepositoryMetadata{}, err
	}
	var payload struct {
		ID      json.Number `json:"id"`
		Name    string      `json:"name"`
		HTMLURL string      `json:"html_url"`
		Owner   githubUser  `json:"owner"`
	}
	if err := client.do(ctx, http.MethodGet, endpoint, nil, &payload); err != nil {
		return RepositoryMetadata{}, err
	}
	return RepositoryMetadata{ServiceRoot: serviceRoot, Repository: RepositoryRef{
		Provider: kind, Owner: payload.Owner.Login, Name: payload.Name, ID: payload.ID.String(), URL: payload.HTMLURL,
	}}, nil
}

// ReadRepository resolves a configured repository name or native ID.
func (p *ADOProvider) ReadRepository(ctx context.Context, repo RepositoryRef) (RepositoryMetadata, error) {
	if err := requireRepo(repo); err != nil {
		return RepositoryMetadata{}, err
	}
	endpoint, err := p.repoURL(repo)
	if err != nil {
		return RepositoryMetadata{}, err
	}
	var payload struct {
		adoRepository
		RemoteURL string `json:"remoteUrl"`
		WebURL    string `json:"webUrl"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &payload); err != nil {
		return RepositoryMetadata{}, err
	}
	repositoryURL := payload.WebURL
	if repositoryURL == "" {
		repositoryURL = payload.RemoteURL
	}
	return RepositoryMetadata{ServiceRoot: strings.TrimRight(p.BaseURL, "/"), ProjectID: payload.Project.ID, Repository: RepositoryRef{
		Provider: ProviderADO, Owner: p.Organization, Project: payload.Project.Name,
		Name: payload.Name, ID: payload.ID, URL: repositoryURL,
	}}, nil
}
