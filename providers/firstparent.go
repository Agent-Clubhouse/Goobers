package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// firstParentHistoryPage bounds the one page of history FirstParentHistory
// reads: the provider's largest page.
const firstParentHistoryPage = 100

type historyCommit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// FirstParentHistory returns the first-parent chain of branch history from
// tip back to, but excluding, stop, newest first (#7071). These are the
// commits the branch itself pointed at, not the commits of branches merged
// into it. It reads a single page of history listed from tip and returns the
// chain only when following first parents reaches stop within limit commits
// on that page; otherwise, as on a long history or when stop is not itself
// on the first-parent chain, it returns none, so no commit older than stop
// is ever named. A read, so it emits no mutation event.
func (p *GitHubProvider) FirstParentHistory(ctx context.Context, repo RepositoryRef, tip, stop string, limit int) ([]string, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if tip == "" {
		return nil, fmt.Errorf("tip is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"sha": {tip}, "per_page": {strconv.Itoa(firstParentHistoryPage)}})
	if err != nil {
		return nil, err
	}
	var commits []historyCommit
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &commits); err != nil {
		return nil, err
	}
	return walkFirstParents(commits, tip, stop, limit), nil
}

// FirstParentHistory is GitHubProvider.FirstParentHistory over Gitea's
// commit listing, which caps its page at the server's configured maximum.
func (p *GiteaProvider) FirstParentHistory(ctx context.Context, repo RepositoryRef, tip, stop string, limit int) ([]string, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if tip == "" {
		return nil, fmt.Errorf("tip is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"sha":          {tip},
		"limit":        {strconv.Itoa(firstParentHistoryPage)},
		"stat":         {"false"},
		"verification": {"false"},
		"files":        {"false"},
	})
	if err != nil {
		return nil, err
	}
	var commits []historyCommit
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &commits); err != nil {
		return nil, err
	}
	return walkFirstParents(commits, tip, stop, limit), nil
}

// walkFirstParents follows first parents from tip through commits, returning
// the chain before stop, or nil when stop is not reached within limit
// commits held by commits.
func walkFirstParents(commits []historyCommit, tip, stop string, limit int) []string {
	byID := make(map[string]historyCommit, len(commits))
	for _, commit := range commits {
		byID[commit.SHA] = commit
	}
	var chain []string
	for sha := tip; sha != stop; {
		commit, ok := byID[sha]
		if !ok || len(chain) == limit {
			return nil
		}
		chain = append(chain, sha)
		sha = ""
		if len(commit.Parents) > 0 {
			sha = commit.Parents[0].SHA
		}
	}
	return chain
}
