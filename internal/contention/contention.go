// Package contention queries open pull-request file touches and ranks backlog
// items against those touches.
package contention

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// PullRequestTouch identifies one open pull request and the files it changes.
type PullRequestTouch struct {
	Number int
	Files  []string
}

// PullRequestProvider is the provider surface needed to query file touches.
type PullRequestProvider interface {
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	PullRequestFiles(context.Context, providers.RepositoryRef, string) ([]providers.ChangedFile, error)
}

var sourceFileRefPattern = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:go|ya?ml|json|tsx?|jsx?|md|sh|toml)\b`)

// OpenPullRequestTouches lists namespaced open pull requests and their changed
// files in provider order. branchNamespace is supplied by the caller so this
// domain query does not depend on process configuration.
func OpenPullRequestTouches(
	ctx context.Context,
	provider PullRequestProvider,
	repo providers.RepositoryRef,
	base string,
	branchNamespace string,
) ([]PullRequestTouch, error) {
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: branchNamespace, SkipCheckState: true,
	})
	if err != nil {
		return nil, err
	}
	touches := make([]PullRequestTouch, 0, len(prs))
	for _, pr := range prs {
		files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(pr.Number))
		if err != nil {
			return nil, err
		}
		paths := make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.Path)
		}
		touches = append(touches, PullRequestTouch{Number: pr.Number, Files: paths})
	}
	return touches, nil
}

// StablePartition returns items with disjoint items before contested items,
// preserving input order within both groups. The returned item slice does not
// alias the input. minPullRequests values below one are treated as one.
func StablePartition(
	items []providers.WorkItem,
	touches []PullRequestTouch,
	minPullRequests int,
) ([]providers.WorkItem, []string) {
	if minPullRequests < 1 {
		minPullRequests = 1
	}
	clean := make([]providers.WorkItem, 0, len(items))
	contested := make([]providers.WorkItem, 0)
	var contestedIDs []string
	for _, item := range items {
		refs := referencedFilePaths(item.Title + "\n" + item.Body)
		if distinctPullRequestsTouching(refs, touches) >= minPullRequests {
			contested = append(contested, item)
			contestedIDs = append(contestedIDs, item.ID)
			continue
		}
		clean = append(clean, item)
	}
	return append(clean, contested...), contestedIDs
}

func referencedFilePaths(text string) []string {
	matches := sourceFileRefPattern.FindAllString(text, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		ref := strings.TrimPrefix(match, "./")
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

func distinctPullRequestsTouching(refs []string, touches []PullRequestTouch) int {
	if len(refs) == 0 {
		return 0
	}
	count := 0
	for _, touch := range touches {
		if pullRequestTouchesAnyRef(touch.Files, refs) {
			count++
		}
	}
	return count
}

func pullRequestTouchesAnyRef(files, refs []string) bool {
	for _, file := range files {
		for _, ref := range refs {
			if file == ref || strings.HasSuffix(file, "/"+ref) {
				return true
			}
		}
	}
	return false
}
