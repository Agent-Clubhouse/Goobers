package providers

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
)

// ReadSourceBranch observes an exact GitHub branch without ancillary activity.
func (p *GitHubProvider) ReadSourceBranch(ctx context.Context, repo RepositoryRef, branch string) (string, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return "", err
	}
	if !sourceBranchName(branch) {
		return "", ErrRepositorySource
	}
	ref, err := p.getGitHubRef(WithResponseBodyLimit(ctx, MaxRepositorySourceResponseBytes), repo, "heads/"+branch)
	if err != nil {
		return "", err
	}
	if ref.Ref != "refs/heads/"+branch || !ValidSourceCommit(ref.Object.SHA) {
		return "", ErrRepositorySource
	}
	return ref.Object.SHA, nil
}

type sourceTreeEntry struct {
	Path, Mode, Type, SHA string
	Size                  int64
}
type sourceTree struct {
	SHA       string
	Truncated bool
	Tree      []sourceTreeEntry
}

// ReadRepositorySource verifies tree modes before fetching a GitHub blob. The
// Contents API is intentionally unsuitable: it dereferences regular symlinks.
// https://docs.github.com/en/rest/repos/contents#get-repository-content
func (p *GitHubProvider) ReadRepositorySource(ctx context.Context, repo RepositoryRef, name, commit string) (RepositorySourceFile, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return RepositorySourceFile{}, err
	}
	if !ValidRepositorySourcePath(name) || !ValidSourceCommit(commit) {
		return RepositorySourceFile{}, ErrRepositorySource
	}
	ctx = WithResponseBodyLimit(ctx, MaxRepositorySourceResponseBytes)
	root, err := p.sourceCommitTree(ctx, repo, commit)
	if err != nil {
		return RepositorySourceFile{}, err
	}
	entry, err := p.sourcePathEntry(ctx, repo, root, strings.Split(name, "/"))
	if err != nil {
		return RepositorySourceFile{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "blobs", entry.SHA)
	if err != nil {
		return RepositorySourceFile{}, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RepositorySourceFile{}, err
	}
	etag := sourceETag(response.Header.Get("ETag"))
	var blob struct {
		SHA, Encoding, Content string
		Size                   int64
	}
	if err = readJSONResponse(response, http.MethodGet, endpoint, &blob); err != nil {
		return RepositorySourceFile{}, err
	}
	if blob.SHA != entry.SHA || blob.Encoding != "base64" || blob.Size != entry.Size || blob.Size < 0 || blob.Size > MaxRepositorySourceBytes {
		return RepositorySourceFile{}, ErrRepositorySource
	}
	content, err := base64.StdEncoding.DecodeString(blob.Content)
	if err != nil || int64(len(content)) != blob.Size {
		return RepositorySourceFile{}, ErrRepositorySource
	}
	file := RepositorySourceFile{Commit: commit, Path: name, BlobID: blob.SHA, ETag: etag, Content: content}
	return file, ValidateRepositorySourceFile(file, name, commit)
}

func (p *GitHubProvider) sourceCommitTree(ctx context.Context, repo RepositoryRef, commit string) (string, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "commits", commit)
	if err != nil {
		return "", err
	}
	var out struct {
		SHA  string
		Tree struct{ SHA string }
	}
	if err = p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return "", err
	}
	if out.SHA != commit || !ValidSourceCommit(out.Tree.SHA) {
		return "", ErrRepositorySource
	}
	return out.Tree.SHA, nil
}

func (p *GitHubProvider) sourcePathEntry(ctx context.Context, repo RepositoryRef, tree string, parts []string) (sourceTreeEntry, error) {
	for index, part := range parts {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "trees", tree)
		if err != nil {
			return sourceTreeEntry{}, err
		}
		var out sourceTree
		if err = p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
			return sourceTreeEntry{}, err
		}
		if out.SHA != tree || out.Truncated || len(out.Tree) > MaxRepositorySourceTreeEntries {
			return sourceTreeEntry{}, ErrRepositorySource
		}
		entry, err := exactSourceEntry(out.Tree, part)
		if err != nil {
			return sourceTreeEntry{}, err
		}
		if index == len(parts)-1 {
			if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || entry.Size < 0 || entry.Size > MaxRepositorySourceBytes {
				return sourceTreeEntry{}, ErrRepositorySource
			}
			return entry, nil
		}
		if entry.Type != "tree" || entry.Mode != "040000" {
			return sourceTreeEntry{}, ErrRepositorySource
		}
		tree = entry.SHA
	}
	return sourceTreeEntry{}, ErrRepositorySource
}

func exactSourceEntry(entries []sourceTreeEntry, name string) (sourceTreeEntry, error) {
	var result sourceTreeEntry
	for _, entry := range entries {
		if entry.Path != name {
			continue
		}
		if result.SHA != "" || !ValidSourceCommit(entry.SHA) {
			return sourceTreeEntry{}, ErrRepositorySource
		}
		result = entry
	}
	if result.SHA == "" {
		return result, ErrRepositorySource
	}
	return result, nil
}
