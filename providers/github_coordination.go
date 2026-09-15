package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ReleaseObservation distinguishes publication of an exact release tag from
// issue closure and PR merge. SHA resolves the tag, not target_commitish.
type ReleaseObservation struct {
	Tag            string
	SHA            string
	Published      bool
	IncludesCommit bool
}

// GetCoordinationRelease reads a published release, resolves its tag to a commit,
// and verifies that the required merged commit is included. It never creates a
// release or deploys. Missing releases remain provider errors, not success.
func (p *GitHubProvider) GetCoordinationRelease(ctx context.Context, repo RepositoryRef, tag, mergedCommit string) (ReleaseObservation, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return ReleaseObservation{}, err
	}
	if tag == "" || !sharedGitSHA(mergedCommit) {
		return ReleaseObservation{}, fmt.Errorf("exact release tag and merged commit are required")
	}
	root, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name)
	if err != nil {
		return ReleaseObservation{}, err
	}
	var release struct {
		Tag         string     `json:"tag_name"`
		Draft       bool       `json:"draft"`
		PublishedAt *time.Time `json:"published_at"`
	}
	if err := p.do(ctx, http.MethodGet, root+"/releases/tags/"+url.PathEscape(tag), nil, &release); err != nil {
		return ReleaseObservation{}, err
	}
	out := ReleaseObservation{Tag: release.Tag, Published: !release.Draft && release.PublishedAt != nil}
	if !out.Published || out.Tag != tag {
		return out, nil
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := p.do(ctx, http.MethodGet, root+"/commits/"+url.PathEscape("refs/tags/"+tag), nil, &commit); err != nil {
		return out, err
	}
	if !sharedGitSHA(commit.SHA) {
		return out, fmt.Errorf("release tag did not resolve to a full commit SHA")
	}
	out.SHA = commit.SHA
	var comparison struct {
		Status string `json:"status"`
	}
	if err := p.do(ctx, http.MethodGet, root+"/compare/"+mergedCommit+"..."+out.SHA, nil, &comparison); err != nil {
		return out, err
	}
	out.IncludesCommit = comparison.Status == "ahead" || comparison.Status == "identical"
	return out, nil
}
