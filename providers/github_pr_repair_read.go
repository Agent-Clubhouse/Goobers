package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

type repairGitHubRepository struct {
	ID       int64
	FullName string `json:"full_name"`
}
type repairGitHubPR struct {
	ID                 int64
	Number             int
	Title, Body, State string
	Draft, Merged      bool
	Head               struct {
		Ref, SHA string
		Repo     *repairGitHubRepository
	}
	Base struct {
		Ref, SHA string
		Repo     *repairGitHubRepository
	}
}

// InspectRepairPullRequest verifies both repository identities. The generic PR
// summary intentionally omits this fork boundary and cannot authorize repair.
func (p *GitHubProvider) InspectRepairPullRequest(ctx context.Context, repo RepositoryRef, id string) (RepairPullRequest, error) {
	var result RepairPullRequest
	if requireOwnerRepo(repo) != nil || repo.Provider != ProviderGitHub || !nativePositiveID(id) {
		return result, ErrPRRepair
	}
	ctx, cancel := repairContext(ctx)
	defer cancel()
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", id)
	if err != nil {
		return result, err
	}
	var pr repairGitHubPR
	if err = p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return result, err
	}
	name := repo.Owner + "/" + repo.Name
	if pr.ID <= 0 || strconv.Itoa(pr.Number) != id || pr.Head.Repo == nil || pr.Base.Repo == nil || pr.Head.Repo.ID <= 0 || pr.Head.Repo.ID != pr.Base.Repo.ID || !strings.EqualFold(pr.Head.Repo.FullName, name) || !strings.EqualFold(pr.Base.Repo.FullName, name) || (pr.State != "open" && pr.State != "closed") {
		return result, ErrPRRepair
	}
	result = RepairPullRequest{Repository: repo, RepositoryID: strconv.FormatInt(pr.Head.Repo.ID, 10), ID: id, StableID: strconv.FormatInt(pr.ID, 10), Title: pr.Title, Body: pr.Body, Head: pr.Head.Ref, Base: pr.Base.Ref, HeadSHA: pr.Head.SHA, BaseSHA: pr.Base.SHA, Open: pr.State == "open" && !pr.Merged, Draft: pr.Draft, URL: "https://github.com/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pull/" + id}
	if !validRepairTarget(result) {
		return RepairPullRequest{}, ErrPRRepair
	}
	return result, nil
}

// ReadRepairFile reads one regular text file at the freshly verified PR head.
func (p *GitHubProvider) ReadRepairFile(ctx context.Context, target RepairPullRequest, name string) (RepairFile, error) {
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
func (p *GitHubProvider) repairFile(ctx context.Context, repo RepositoryRef, commit, name string) (RepairFile, error) {
	result := RepairFile{Commit: commit, Path: name}
	root, err := p.sourceCommitTree(ctx, repo, commit)
	if err != nil {
		return result, err
	}
	entry, found, err := p.repairPathEntry(ctx, repo, root, strings.Split(name, "/"))
	if err != nil || !found {
		return result, err
	}
	file, err := p.ReadRepositorySource(ctx, repo, name, commit)
	if err != nil {
		return result, err
	}
	if !utf8.Valid(file.Content) || strings.ContainsRune(string(file.Content), 0) {
		return result, ErrPRRepair
	}
	result.Present, result.BlobID, result.Mode, result.Content = true, file.BlobID, entry.Mode, string(file.Content)
	return result, nil
}
func (p *GitHubProvider) repairPathEntry(ctx context.Context, repo RepositoryRef, tree string, parts []string) (sourceTreeEntry, bool, error) {
	for index, part := range parts {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "trees", tree)
		if err != nil {
			return sourceTreeEntry{}, false, err
		}
		var out sourceTree
		if err = p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
			return sourceTreeEntry{}, false, err
		}
		if out.SHA != tree || out.Truncated || len(out.Tree) > MaxRepositorySourceTreeEntries {
			return sourceTreeEntry{}, false, ErrPRRepair
		}
		entry, found, err := repairTreeEntry(out.Tree, part, index == len(parts)-1)
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
