package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// CreateRepository creates an empty GitHub repository under a user or
// organization owner. It never initializes, commits, or pushes content.
func (p *GitHubProvider) CreateRepository(ctx context.Context, req CreateRepositoryRequest) (CreateRepositoryResult, error) {
	if req.Owner == "" || req.Name == "" {
		return CreateRepositoryResult{}, fmt.Errorf("repository owner and name are required")
	}
	switch req.Visibility {
	case "private", "public", "internal":
	default:
		return CreateRepositoryResult{}, fmt.Errorf("repository visibility must be private, public, or internal")
	}

	var viewer struct {
		Login string `json:"login"`
	}
	userEndpoint, err := joinURL(p.BaseURL, "user")
	if err != nil {
		return CreateRepositoryResult{}, err
	}
	if err := p.do(ctx, http.MethodGet, userEndpoint, nil, &viewer); err != nil {
		return CreateRepositoryResult{}, fmt.Errorf("resolve authenticated GitHub owner: %w", err)
	}

	endpoint := ""
	if strings.EqualFold(viewer.Login, req.Owner) {
		endpoint, err = joinURL(p.BaseURL, "user", "repos")
	} else {
		endpoint, err = joinURL(p.BaseURL, "orgs", req.Owner, "repos")
	}
	if err != nil {
		return CreateRepositoryResult{}, err
	}
	body := map[string]interface{}{
		"name":       req.Name,
		"visibility": req.Visibility,
		"private":    req.Visibility == "private",
		"auto_init":  false,
	}
	var created struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name       string `json:"name"`
		CloneURL   string `json:"clone_url"`
		Visibility string `json:"visibility"`
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &created); err != nil {
		return CreateRepositoryResult{}, fmt.Errorf("create GitHub repository %s/%s: %w", req.Owner, req.Name, err)
	}
	return CreateRepositoryResult{
		Repository: RepositoryRef{
			Provider: ProviderGitHub,
			Owner:    created.Owner.Login,
			Name:     created.Name,
			URL:      created.CloneURL,
		},
		Visibility: created.Visibility,
	}, nil
}

// CloneRepository clones a GitHub repository to a local destination.
func (p *GitHubProvider) CloneRepository(ctx context.Context, req CloneRequest) (CloneResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return CloneResult{}, err
	}
	if req.Destination == "" {
		return CloneResult{}, fmt.Errorf("destination is required")
	}
	cloneURL := req.Repository.URL
	if cloneURL == "" {
		cloneURL = fmt.Sprintf("https://github.com/%s/%s.git", req.Repository.Owner, req.Repository.Name)
	}
	args := []string{"clone"}
	if req.Branch != "" {
		args = append(args, "--branch", req.Branch)
	}
	args = append(args, cloneURL, req.Destination)
	if out, err := p.Runner.Run(ctx, "git", args...); err != nil {
		return CloneResult{}, fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return CloneResult{Path: req.Destination, URL: cloneURL}, nil
}

// CreateBranch creates a branch ref in GitHub.
func (p *GitHubProvider) CreateBranch(ctx context.Context, req BranchRequest) (BranchResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return BranchResult{}, err
	}
	if req.Name == "" {
		return BranchResult{}, fmt.Errorf("branch name is required")
	}
	baseSHA := req.BaseSHA
	if baseSHA == "" {
		baseBranch := req.BaseBranch
		if baseBranch == "" {
			baseBranch = "main"
		}
		ref, err := p.getGitHubRef(ctx, req.Repository, "heads/"+baseBranch)
		if err != nil {
			return BranchResult{}, err
		}
		baseSHA = ref.Object.SHA
	}
	var out githubRef
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "git", "refs")
	if err != nil {
		return BranchResult{}, err
	}
	body := map[string]string{"ref": "refs/heads/" + req.Name, "sha": baseSHA}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return BranchResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, req.Name),
		URL:       out.URL,
		Operation: "branch",
		Fields: map[string]FieldDigest{
			"sha": {After: digestString(out.Object.SHA)},
		},
	})
	return BranchResult{Name: req.Name, SHA: out.Object.SHA, URL: out.URL}, nil
}

// ListBranches returns a bounded lexicographic page of remote refs matching a
// prefix. It follows GitHub pagination until the requested page is full; After
// makes repeated bounded sweeps progress without depending on page numbers that
// shift when an earlier branch is deleted.
func (p *GitHubProvider) ListBranches(ctx context.Context, req ListBranchesRequest) ([]BranchSummary, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	if req.Prefix == "" {
		return nil, fmt.Errorf("branch prefix is required")
	}
	if req.Limit < 1 {
		return nil, fmt.Errorf("branch limit must be positive")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "git", "matching-refs", "heads", req.Prefix)
	if err != nil {
		return nil, err
	}
	const headPrefix = "refs/heads/"
	branches := make([]BranchSummary, 0, req.Limit)
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var refs []githubRef
		if err := json.Unmarshal(page, &refs); err != nil {
			return fmt.Errorf("decode branch refs: %w", err)
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref.Ref, headPrefix) {
				continue
			}
			name := strings.TrimPrefix(ref.Ref, headPrefix)
			if !strings.HasPrefix(name, req.Prefix) || (req.After != "" && name <= req.After) {
				continue
			}
			branches = append(branches, BranchSummary{Name: name, SHA: ref.Object.SHA, URL: ref.URL})
		}
		sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
		if len(branches) >= req.Limit {
			return errStopPaging
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(branches) > req.Limit {
		branches = branches[:req.Limit]
	}
	return branches, nil
}

// GetBranch reads one exact branch ref and its latest repository activity for
// reconciliation's pre-delete staleness check. A missing ref is reported
// separately from provider failure.
func (p *GitHubProvider) GetBranch(ctx context.Context, repo RepositoryRef, name string) (BranchSummary, bool, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return BranchSummary{}, false, err
	}
	if name == "" {
		return BranchSummary{}, false, fmt.Errorf("branch name is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "ref", "heads", name)
	if err != nil {
		return BranchSummary{}, false, err
	}
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return BranchSummary{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return BranchSummary{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return BranchSummary{}, false, fmt.Errorf("GET %s failed: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ref githubRef
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		return BranchSummary{}, false, fmt.Errorf("decode branch ref: %w", err)
	}
	const headPrefix = "refs/heads/"
	if ref.Ref != headPrefix+name {
		return BranchSummary{}, false, fmt.Errorf("provider returned branch ref %q for %q", ref.Ref, name)
	}
	activityEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "activity")
	if err != nil {
		return BranchSummary{}, false, err
	}
	activityEndpoint, err = addQuery(activityEndpoint, url.Values{
		"direction": []string{"desc"},
		"per_page":  []string{"1"},
		"ref":       []string{headPrefix + name},
	})
	if err != nil {
		return BranchSummary{}, false, err
	}
	var activities []githubRepositoryActivity
	if err := p.do(ctx, http.MethodGet, activityEndpoint, nil, &activities); err != nil {
		return BranchSummary{}, false, err
	}
	branch := BranchSummary{Name: name, SHA: ref.Object.SHA, URL: ref.URL}
	if len(activities) > 0 {
		if activities[0].Ref != headPrefix+name {
			return BranchSummary{}, false, fmt.Errorf("provider returned activity for ref %q instead of %q", activities[0].Ref, headPrefix+name)
		}
		branch.LastActivityAt = &activities[0].Timestamp
	}
	return branch, true, nil
}

// DeleteBranch removes a GitHub branch ref. ExpectedSHA opts into an atomic
// force-with-lease deletion; callers without a lease retain the idempotent REST
// deletion used by post-merge cleanup.
func (p *GitHubProvider) DeleteBranch(ctx context.Context, req DeleteBranchRequest) (DeleteBranchResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return DeleteBranchResult{}, err
	}
	if req.Name == "" {
		return DeleteBranchResult{}, fmt.Errorf("branch name is required")
	}
	if req.ExpectedSHA != "" {
		return p.deleteBranchWithLease(ctx, req)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "git", "refs", "heads", req.Name)
	if err != nil {
		return DeleteBranchResult{}, err
	}
	resp, err := p.send(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return DeleteBranchResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return DeleteBranchResult{}, fmt.Errorf("DELETE %s failed: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	deleted := resp.StatusCode != http.StatusNotFound
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, req.Name),
		Operation: "delete",
	})
	return DeleteBranchResult{Deleted: deleted}, nil
}

func (p *GitHubProvider) deleteBranchWithLease(ctx context.Context, req DeleteBranchRequest) (DeleteBranchResult, error) {
	runner, ok := p.Runner.(environmentCommandRunner)
	if !ok {
		return DeleteBranchResult{}, fmt.Errorf("conditional branch deletion requires an environment-capable command runner")
	}
	gitDir, err := os.MkdirTemp("", "goobers-delete-branch-*")
	if err != nil {
		return DeleteBranchResult{}, fmt.Errorf("create temporary git directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(gitDir) }()

	if out, err := p.Runner.Run(ctx, "git", "init", "--bare", "--quiet", gitDir); err != nil {
		return DeleteBranchResult{}, fmt.Errorf("initialize temporary git directory: %w: %s", err, strings.TrimSpace(string(out)))
	}
	token, err := p.resolveToken(ctx)
	if err != nil {
		return DeleteBranchResult{}, err
	}
	remoteURL := req.Repository.URL
	if remoteURL == "" {
		remoteURL = fmt.Sprintf("https://github.com/%s/%s.git", req.Repository.Owner, req.Repository.Name)
	}
	ref := "refs/heads/" + req.Name
	args := []string{
		"--git-dir=" + gitDir,
		"push",
		"--porcelain",
		"--force-with-lease=" + ref + ":" + req.ExpectedSHA,
		remoteURL,
		":" + ref,
	}
	out, err := runner.RunWithEnv(ctx, githubGitAuthEnv(token), "git", args...)
	if err != nil {
		if rateLimitErr := githubGitPushRateLimitError(req.Repository, out); rateLimitErr != nil {
			return DeleteBranchResult{}, rateLimitErr
		}
		if strings.Contains(string(out), "(stale info)") {
			_, found, lookupErr := p.GetBranch(ctx, req.Repository, req.Name)
			if lookupErr != nil {
				return DeleteBranchResult{}, fmt.Errorf("resolve conditional branch deletion rejection: %w", lookupErr)
			}
			if !found {
				return DeleteBranchResult{}, nil
			}
			return DeleteBranchResult{}, &BranchTipChangedError{Name: req.Name, ExpectedSHA: req.ExpectedSHA}
		}
		return DeleteBranchResult{}, fmt.Errorf("delete branch with lease: %w: %s", err, strings.TrimSpace(string(out)))
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, req.Name),
		Operation: "delete",
	})
	return DeleteBranchResult{Deleted: true}, nil
}

func githubGitPushRateLimitError(repo RepositoryRef, output []byte) *RateLimitError {
	message := strings.ToLower(string(output))
	secondary := strings.Contains(message, "secondary rate limit") ||
		strings.Contains(message, "abuse detection") ||
		strings.Contains(message, "abuse rate limit")
	status := 0
	switch {
	case strings.Contains(message, "error: 429"),
		strings.Contains(message, "http 429"),
		strings.Contains(message, "status 429"),
		strings.Contains(message, "too many requests"):
		status = http.StatusTooManyRequests
	case secondary, strings.Contains(message, "rate limit exceeded"):
		status = http.StatusForbidden
	default:
		return nil
	}
	return &RateLimitError{
		Endpoint:  fmt.Sprintf("git push %s/%s", repo.Owner, repo.Name),
		Status:    status,
		Secondary: secondary,
	}
}

func githubGitAuthEnv(token string) []string {
	if token == "" {
		return os.Environ()
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth,
	)
}

// GitHubGitAuthEnvironment resolves a GitHub token into a child-process-only Git
// environment that authenticates clone/fetch of remoteURL as x-access-token via a
// URL-scoped http.extraheader — the same mechanism githubGitAuthEnv uses for the
// provider's own git ops, hardened for reuse by the worktree layer (MGV-11
// #1286): it strips any inherited GIT_CONFIG_*/terminal-prompt vars so a caller's
// ambient git config can't alter auth, disables credential helpers, and scopes
// the header to remoteURL so a git process that touches another host never sends
// this token. The token (and its base64 auth form) are registered with registrar
// so they are scrubbed from anything written to rest. The returned environment
// must never be persisted or journaled. An empty token returns a hardened env
// with no auth header (an anonymous clone of a public repo still works).
func GitHubGitAuthEnvironment(token, remoteURL string, registrar SecretRegistrar) []string {
	base := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if upper == "GIT_CONFIG_COUNT" || upper == "GIT_TERMINAL_PROMPT" ||
			strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") {
			continue
		}
		base = append(base, entry)
	}
	if strings.TrimSpace(token) == "" {
		return append(base, "GIT_TERMINAL_PROMPT=0")
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	if registrar != nil {
		registrar.Register([]byte(token))
		registrar.Register([]byte(auth))
	}
	scopedURL := strings.TrimRight(remoteURL, "/") + "/"
	return append(base,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http."+scopedURL+".extraheader",
		"GIT_CONFIG_VALUE_1=AUTHORIZATION: basic "+auth,
		"GIT_TERMINAL_PROMPT=0",
	)
}

// Commit writes file changes to a GitHub branch.
func (p *GitHubProvider) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return CommitResult{}, err
	}
	if req.Branch == "" {
		return CommitResult{}, fmt.Errorf("branch is required")
	}
	if req.Message == "" {
		return CommitResult{}, fmt.Errorf("message is required")
	}
	if len(req.Files) == 0 {
		return CommitResult{}, fmt.Errorf("at least one file is required")
	}

	var last githubContentResponse
	for _, file := range req.Files {
		if file.Path == "" {
			return CommitResult{}, fmt.Errorf("file path is required")
		}
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "contents", file.Path)
		if err != nil {
			return CommitResult{}, err
		}
		endpoint, err = addQuery(endpoint, url.Values{"ref": []string{req.Branch}})
		if err != nil {
			return CommitResult{}, err
		}
		sha, exists, err := p.contentSHA(ctx, endpoint)
		if err != nil {
			return CommitResult{}, err
		}
		changeType, err := normalizeCommitChange(file.ChangeType, exists)
		if err != nil {
			return CommitResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		body := map[string]string{
			"message": req.Message,
			"branch":  req.Branch,
		}
		if exists {
			body["sha"] = sha
		}
		method := http.MethodPut
		if changeType == CommitChangeDelete {
			method = http.MethodDelete
		} else {
			body["content"] = base64.StdEncoding.EncodeToString([]byte(file.Content))
		}
		if err := p.do(ctx, method, endpoint, body, &last); err != nil {
			return CommitResult{}, err
		}
	}
	result := CommitResult{SHA: last.Commit.SHA, URL: last.Commit.HTMLURL}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, result.SHA),
		URL:       result.URL,
		Operation: "commit",
		Fields: map[string]FieldDigest{
			"sha":   {After: digestString(result.SHA)},
			"files": {After: digestString(strconv.Itoa(len(req.Files)))},
		},
	})
	// NOTE(#140 item 4): this still commits one Contents-API call per file, so a
	// mid-loop failure can strand a half-committed branch and CommitResult
	// reports only the last file's SHA. Making it atomic needs the Git data API
	// (blobs -> tree -> commit -> update-ref); tracked as a follow-up so it can
	// land with its own multi-file atomicity test rather than ride this PR.
	return result, nil
}

// OpenPullRequest opens a GitHub pull request — idempotent on repass (#132):
// a workflow's open-pr stage reuses the same stable run branch
// (providers.BranchName) on every repass through it, so a second call here
// must find and update the PR it already opened rather than attempting a
// duplicate POST (which GitHub 422s on, since a PR already exists for that
// head/base). Checking first, rather than POSTing and catching the 422, also
// sidesteps this package's lack of a typed HTTP-status error to match against
