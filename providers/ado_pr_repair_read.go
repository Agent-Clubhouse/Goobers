package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

type repairADOPR struct {
	PullRequestID                                            int
	Title, Description, Status, SourceRefName, TargetRefName string
	IsDraft                                                  bool
	ForkSource                                               json.RawMessage
	LastMergeSourceCommit                                    struct{ CommitID string }
	LastMergeTargetCommit                                    struct{ CommitID string }
	Repository                                               struct {
		ID, Name, RemoteURL string
		Project             struct{ ID, Name string }
	}
}

// InspectRepairPullRequest verifies native repository and fork ownership.
func (p *ADOProvider) InspectRepairPullRequest(ctx context.Context, repo RepositoryRef, id string) (RepairPullRequest, error) {
	var result RepairPullRequest
	if requireRepo(repo) != nil || repo.Provider != ProviderADO || repo.Owner != p.Organization || !nativePositiveID(id) {
		return result, ErrPRRepair
	}
	ctx, cancel := repairContext(ctx)
	defer cancel()
	endpoint, err := p.repoURL(repo, "pullrequests", id)
	if err != nil {
		return result, err
	}
	var pr repairADOPR
	if err = p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return result, err
	}
	if !repairADOPRScope(pr, repo, id) {
		return result, ErrPRRepair
	}
	result = RepairPullRequest{Repository: repo, RepositoryID: pr.Repository.ID, ID: id, StableID: strconv.Itoa(pr.PullRequestID), Title: pr.Title, Body: pr.Description, URL: p.webURL(repo.Project, "_git", repo.Name, "pullrequest", id), Head: strings.TrimPrefix(pr.SourceRefName, "refs/heads/"), Base: strings.TrimPrefix(pr.TargetRefName, "refs/heads/"), HeadSHA: pr.LastMergeSourceCommit.CommitID, BaseSHA: pr.LastMergeTargetCommit.CommitID, Open: pr.Status == "active", Draft: pr.IsDraft}
	if !validRepairTarget(result) {
		return RepairPullRequest{}, ErrPRRepair
	}
	return result, nil
}
func repairADOPRScope(pr repairADOPR, repo RepositoryRef, id string) bool {
	if strconv.Itoa(pr.PullRequestID) != id || !adoIdentityGUID.MatchString(pr.Repository.ID) || !strings.EqualFold(pr.Repository.Name, repo.Name) || !strings.EqualFold(pr.Repository.Project.Name, repo.Project) || !strings.HasPrefix(pr.SourceRefName, "refs/heads/") || !strings.HasPrefix(pr.TargetRefName, "refs/heads/") {
		return false
	}
	if len(pr.ForkSource) > 0 && string(pr.ForkSource) != "null" {
		return false
	}
	if pr.Status != "active" && pr.Status != "completed" && pr.Status != "abandoned" {
		return false
	}
	remote, err := url.Parse(pr.Repository.RemoteURL)
	if err != nil || remote.Scheme != "https" || remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" {
		return false
	}
	org, project, name, ok := ParseADORemoteURL(pr.Repository.RemoteURL)
	return ok && strings.EqualFold(org, repo.Owner) && strings.EqualFold(project, repo.Project) && strings.EqualFold(name, repo.Name)
}

// ReadRepairFile uses immutable tree custody to distinguish missing paths.
func (p *ADOProvider) ReadRepairFile(ctx context.Context, target RepairPullRequest, name string) (RepairFile, error) {
	var result RepairFile
	if !validRepairPath(name) {
		return result, ErrPRRepair
	}
	ctx, cancel := repairContext(ctx)
	defer cancel()
	if err := freshRepairTarget(ctx, p, target); err != nil {
		return result, err
	}
	return p.repairFile(ctx, target.Repository, target.HeadSHA, name)
}
func (p *ADOProvider) repairFile(ctx context.Context, repo RepositoryRef, commit, name string) (RepairFile, error) {
	result := RepairFile{Commit: commit, Path: name}
	root, err := p.repairCommit(ctx, repo, commit)
	if err != nil {
		return result, err
	}
	entry, found, err := p.repairPathEntry(ctx, repo, root.TreeID, strings.Split(name, "/"))
	if err != nil || !found {
		return result, err
	}
	file, err := p.ReadRepositorySource(ctx, repo, name, commit)
	if err != nil {
		return result, err
	}
	if file.BlobID != entry.SHA || !utf8.Valid(file.Content) || strings.ContainsRune(string(file.Content), 0) {
		return result, ErrPRRepair
	}
	result.Present, result.BlobID, result.Mode, result.Content = true, file.BlobID, entry.Mode, string(file.Content)
	return result, nil
}
func (p *ADOProvider) repairCommit(ctx context.Context, repo RepositoryRef, commit string) (proposalADOCommit, error) {
	var result proposalADOCommit
	endpoint, err := p.repoURL(repo, "commits", commit)
	if err != nil {
		return result, err
	}
	if err = p.do(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return result, err
	}
	if result.CommitID != commit || !ValidSourceCommit(result.TreeID) {
		return result, ErrPRRepair
	}
	return result, nil
}
func (p *ADOProvider) repairPathEntry(ctx context.Context, repo RepositoryRef, tree string, parts []string) (sourceTreeEntry, bool, error) {
	for index, part := range parts {
		entries, err := p.repairTree(ctx, repo, tree)
		if err != nil {
			return sourceTreeEntry{}, false, err
		}
		entry, found, err := repairTreeEntry(entries, part, index == len(parts)-1)
		if err != nil || !found {
			return sourceTreeEntry{}, false, err
		}
		if index == len(parts)-1 {
			return entry, true, nil
		}
		tree = entry.SHA
	}
	return sourceTreeEntry{}, false, ErrPRRepair
}
func (p *ADOProvider) repairTree(ctx context.Context, repo RepositoryRef, tree string) ([]sourceTreeEntry, error) {
	endpoint, err := p.repoURL(repo, "trees", tree)
	if err != nil {
		return nil, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return nil, err
	}
	more := response.Header.Get("x-ms-continuationtoken") != "" || response.Header.Get("Link") != ""
	var out struct {
		ObjectID    string
		TreeEntries []struct {
			RelativePath, Mode, GitObjectType, ObjectID string
			Size                                        int64
		}
	}
	if err = readJSONResponse(response, http.MethodGet, endpoint, &out); err != nil {
		return nil, err
	}
	if more || out.ObjectID != tree || len(out.TreeEntries) > MaxRepositorySourceTreeEntries {
		return nil, ErrPRRepair
	}
	result := make([]sourceTreeEntry, 0, len(out.TreeEntries))
	for _, entry := range out.TreeEntries {
		mode := entry.Mode
		if mode == "40000" {
			mode = "040000"
		}
		result = append(result, sourceTreeEntry{Path: entry.RelativePath, Mode: mode, Type: entry.GitObjectType, SHA: entry.ObjectID, Size: entry.Size})
	}
	return result, nil
}
