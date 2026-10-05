package providers

import (
	"context"
	"net/http"
	"net/url"
)

// ReadSourceBranch reuses ADO's bounded exact-ref observation (never prefix-only
// acceptance). It does not substitute an adjacent branch or default branch.
func (p *ADOProvider) ReadSourceBranch(ctx context.Context, repo RepositoryRef, branch string) (string, error) {
	if repo.Owner != p.Organization || !sourceBranchName(branch) {
		return "", ErrRepositorySource
	}
	ref, found, err := p.GetBranch(WithResponseBodyLimit(ctx, MaxRepositorySourceResponseBytes), repo, branch)
	if err != nil {
		return "", err
	}
	if !found || ref.Name != branch || !ValidSourceCommit(ref.SHA) {
		return "", ErrRepositorySource
	}
	return ref.SHA, nil
}

// ReadRepositorySource reads one literal ADO regular blob at an exact commit.
// LFS expansion and rendered content are disabled; Git blob identity is verified.
// https://learn.microsoft.com/en-us/rest/api/azure/devops/git/items/get?view=azure-devops-rest-7.1
func (p *ADOProvider) ReadRepositorySource(ctx context.Context, repo RepositoryRef, name, commit string) (RepositorySourceFile, error) {
	if err := requireRepo(repo); err != nil {
		return RepositorySourceFile{}, err
	}
	if repo.Owner != p.Organization || !ValidRepositorySourcePath(name) || !ValidSourceCommit(commit) {
		return RepositorySourceFile{}, ErrRepositorySource
	}
	endpoint, err := p.repoURL(repo, "items")
	if err != nil {
		return RepositorySourceFile{}, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"path": {"/" + name}, "versionDescriptor.version": {commit}, "versionDescriptor.versionType": {"commit"}, "versionDescriptor.versionOptions": {"none"}, "includeContent": {"true"}, "resolveLfs": {"false"}, "recursionLevel": {"none"}})
	if err != nil {
		return RepositorySourceFile{}, err
	}
	response, err := p.send(WithResponseBodyLimit(ctx, MaxRepositorySourceResponseBytes), http.MethodGet, endpoint, nil, "")
	if err != nil {
		return RepositorySourceFile{}, err
	}
	etag := sourceETag(response.Header.Get("ETag"))
	var item struct {
		CommitID, ObjectID, Path, GitObjectType string
		Content                                 *string
		IsFolder, IsSymLink                     bool
	}
	if err = readJSONResponse(response, http.MethodGet, endpoint, &item); err != nil {
		return RepositorySourceFile{}, err
	}
	if item.Content == nil || item.Path != "/"+name || item.CommitID != commit || item.GitObjectType != "blob" || item.IsFolder || item.IsSymLink {
		return RepositorySourceFile{}, ErrRepositorySource
	}
	file := RepositorySourceFile{Commit: commit, Path: name, BlobID: item.ObjectID, ETag: etag, Content: []byte(*item.Content)}
	return file, ValidateRepositorySourceFile(file, name, commit)
}
