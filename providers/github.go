package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GitHubProvider implements repo, backlog, and trigger operations for GitHub.
type GitHubProvider struct {
	BaseURL string
	Token   string
	Client  HTTPClient
	Runner  CommandRunner

	// tokenSource, when set, resolves the token per request (issue #14 seam);
	// otherwise Token is used.
	tokenSource TokenSource
	// recorder receives "external ref touched" facts for the run journal.
	recorder MutationRecorder
	// rateObserver receives rate-limit backoff signals for telemetry.
	rateObserver RateLimitObserver
	// maxRetries bounds rate-limit retries on a single request.
	maxRetries int
	// maxRateLimitWait bounds the total time one request spends sleeping on
	// rate-limit backoff before giving up with a typed RateLimitError (#614).
	maxRateLimitWait time.Duration
	// now and sleep are injectable for deterministic rate-limit tests.
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// NewGitHubProvider constructs a GitHub provider with optional overrides.
func NewGitHubProvider(token string, opts ...func(*GitHubProvider)) *GitHubProvider {
	p := &GitHubProvider{
		BaseURL:          "https://api.github.com",
		Token:            token,
		maxRetries:       defaultRateLimitRetries,
		maxRateLimitWait: defaultRateLimitMaxWait,
		now:              time.Now,
		sleep:            contextSleep,
		jitter:           randomJitter,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.Client = httpClientOrDefault(p.Client)
	p.Runner = commandRunnerOrDefault(p.Runner)
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = contextSleep
	}
	if p.jitter == nil {
		p.jitter = randomJitter
	}
	return p
}

// WithTokenSource resolves the access token per request from the given source
// (issue #14 token-source seam) instead of the statically injected token.
func WithTokenSource(source TokenSource) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.tokenSource = source }
}

// WithMutationRecorder records every provider-side mutation as an external-ref
// touched fact for the run journal.
func WithMutationRecorder(recorder MutationRecorder) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.recorder = recorder }
}

// WithRateLimitObserver receives rate-limit backoff signals for telemetry.
func WithRateLimitObserver(observer RateLimitObserver) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.rateObserver = observer }
}

// WithHTTPClient overrides the HTTP client every provider request is sent
// through. It exists so a caller can wrap the default client with a
// conditional-GET (ETag) caching layer that turns unchanged per-tick list GETs
// into zero-quota 304s (#1053). A nil client is ignored so the constructor's
// default still applies; a wrapper is expected to embed its own inner client.
func WithHTTPClient(client HTTPClient) func(*GitHubProvider) {
	return func(p *GitHubProvider) {
		if client != nil {
			p.Client = client
		}
	}
}

// WithMaxRateLimitRetries overrides how many times a rate-limited request is
// retried before the error is surfaced.
func WithMaxRateLimitRetries(n int) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.maxRetries = n }
}

// WithRateLimitMaxWait overrides the total time one request may spend
// sleeping on rate-limit backoff (#614) before giving up with a typed
// RateLimitError. Keep it under the invoking stage's own timeout: a wait
// that outlives the stage gets the whole process killed as "timeout",
// masking the rate-limit cause.
func WithRateLimitMaxWait(d time.Duration) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.maxRateLimitWait = d }
}

// Kind returns the GitHub provider kind.
func (p *GitHubProvider) Kind() ProviderKind {
	return ProviderGitHub
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
// (doStatus's non-2xx path returns a plain fmt.Errorf).
func (p *GitHubProvider) OpenPullRequest(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestResult{}, err
	}
	if existing, ok, err := p.FindPullRequestByBranch(ctx, req.Repository, req.Head, req.Base); err != nil {
		return PullRequestResult{}, err
	} else if ok {
		return p.updatePullRequest(ctx, req, existing.Number)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return PullRequestResult{}, err
	}
	prBody := withRunIDFooter(req.Body, req.RunID)
	body := map[string]interface{}{
		"title": req.Title,
		"body":  prBody,
		"head":  req.Head,
		"base":  req.Base,
		"draft": req.Draft,
	}
	var out githubPullRequest
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, strconv.Itoa(out.Number)),
		URL:       out.HTMLURL,
		Operation: "open",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(req.Title)},
			"body":  {After: digestString(prBody)},
		},
	})
	return PullRequestResult{ID: strconv.Itoa(out.Number), Number: out.Number, URL: out.HTMLURL}, nil
}

// FindPullRequestByBranch looks up an open PR for head/base, returning
// ok=false (not an error) if none exists. Exported (not just OpenPullRequest's
// internal idempotency check) so a caller that already knows a run's stable
// branch name (providers.BranchName) but has no other way to recover that
// run's PR — e.g. `goobers issue-close-out` (#132), which runs as its own
// process several stages after open-pr, with no threaded reference to the PR
// it opened — can rediscover it directly from the provider instead.
func (p *GitHubProvider) FindPullRequestByBranch(ctx context.Context, repo RepositoryRef, head, base string) (PullRequestResult, bool, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls")
	if err != nil {
		return PullRequestResult{}, false, err
	}
	query := url.Values{
		"head":  []string{repo.Owner + ":" + head},
		"state": []string{"open"},
	}
	if base != "" {
		query.Set("base", base)
	}
	endpoint, err = addQuery(endpoint, query)
	if err != nil {
		return PullRequestResult{}, false, err
	}
	var out []githubPullRequest
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return PullRequestResult{}, false, err
	}
	if len(out) == 0 {
		return PullRequestResult{}, false, nil
	}
	pr := out[0]
	return PullRequestResult{ID: strconv.Itoa(pr.Number), Number: pr.Number, URL: pr.HTMLURL}, true, nil
}

// OpenPRSummary is the slim per-PR view the open-PR-count throttle (#353/#986)
// needs: the head-branch ref (to bucket by workflow run-branch namespace) and
// the PR's labels (to exclude human-parked PRs the daemon cannot drain from the
// cap). Deliberately minimal — the throttle never needs the full PR.
type OpenPRSummary struct {
	Head   string
	Labels []string
}

// ListOpenPullRequests returns the head-branch ref and labels of every open PR
// on the repo. Used by the scheduler's open-PR-count throttle (#353) to count
// the loop's own un-merged sibling PRs (those under the goobers/ run-branch
// namespace), excluding human-parked ones (#986), so it can pace dispatch.
// Single page of up to 100 — ample for a dogfood loop; a full paginator is a
// follow-up if a repo ever carries >100 open PRs (at which point the cap has
// long since engaged anyway).
func (p *GitHubProvider) ListOpenPullRequests(ctx context.Context, repo RepositoryRef) ([]OpenPRSummary, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"state":    []string{"open"},
		"per_page": []string{"100"},
	})
	if err != nil {
		return nil, err
	}
	var out []githubPullRequest
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	prs := make([]OpenPRSummary, 0, len(out))
	for _, pr := range out {
		labels := make([]string, 0, len(pr.Labels))
		for _, l := range pr.Labels {
			labels = append(labels, l.Name)
		}
		prs = append(prs, OpenPRSummary{Head: pr.Head.Ref, Labels: labels})
	}
	return prs, nil
}

// updatePullRequest applies title/body edits to an already-open PR (its
// number found by FindPullRequestByBranch) — the repass path: the same run
// branch already has an open PR, so this call updates it in place instead of
// opening a duplicate.
func (p *GitHubProvider) updatePullRequest(ctx context.Context, req PullRequestRequest, existingNumber int) (PullRequestResult, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", strconv.Itoa(existingNumber))
	if err != nil {
		return PullRequestResult{}, err
	}
	prBody := withRunIDFooter(req.Body, req.RunID)
	var out githubPullRequest
	if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"title": req.Title, "body": prBody}, &out); err != nil {
		return PullRequestResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, strconv.Itoa(out.Number)),
		URL:       out.HTMLURL,
		Operation: "update",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(req.Title)},
			"body":  {After: digestString(prBody)},
		},
	})
	return PullRequestResult{ID: strconv.Itoa(out.Number), Number: out.Number, URL: out.HTMLURL}, nil
}

// PollPullRequest reports mergeability, review decision, combined check state,
// and comments-since for a GitHub pull request (BL-031). A read, so it does not
// emit a mutation event.
func (p *GitHubProvider) PollPullRequest(ctx context.Context, req PullRequestPollRequest) (PullRequestPollResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestPollResult{}, err
	}
	if req.PullID == "" {
		return PullRequestPollResult{}, fmt.Errorf("pull id is required")
	}
	prEndpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID)
	if err != nil {
		return PullRequestPollResult{}, err
	}
	var pr githubPullRequestDetail
	if err := p.do(ctx, http.MethodGet, prEndpoint, nil, &pr); err != nil {
		return PullRequestPollResult{}, err
	}

	decision, requestedChanges, err := p.reviewDecision(ctx, req.Repository, req.PullID)
	if err != nil {
		return PullRequestPollResult{}, err
	}

	checkState, checks, err := p.combinedCheckState(ctx, req.Repository, pr.Head.SHA)
	if err != nil {
		return PullRequestPollResult{}, err
	}

	comments, err := p.pullRequestComments(ctx, req.Repository, req.PullID, req.CommentsSince)
	if err != nil {
		return PullRequestPollResult{}, err
	}

	return PullRequestPollResult{
		Number:           pr.Number,
		Title:            pr.Title,
		State:            pr.State,
		Merged:           pr.Merged,
		Mergeable:        pr.Mergeable,
		MergeableState:   pr.MergeableState,
		Draft:            pr.Draft,
		HeadBranch:       pr.Head.Ref,
		HeadRepository:   githubRepositoryRef(pr.Head.Repo),
		HeadSHA:          pr.Head.SHA,
		BaseSHA:          pr.Base.SHA,
		BaseBranch:       pr.Base.Ref,
		Body:             pr.Body,
		ReviewDecision:   decision,
		RequestedChanges: requestedChanges,
		CheckState:       checkState,
		Checks:           checks,
		CommentsSince:    comments,
		URL:              pr.HTMLURL,
	}, nil
}

func githubRepositoryRef(repo *githubRepository) *RepositoryRef {
	if repo == nil {
		return nil
	}
	return &RepositoryRef{
		Provider: ProviderGitHub,
		Owner:    repo.Owner.Login,
		Name:     repo.Name,
		URL:      repo.HTMLURL,
	}
}

// ClosePullRequest closes a GitHub pull request, detecting merged-vs-closed, and
// optionally leaves a comment.
func (p *GitHubProvider) ClosePullRequest(ctx context.Context, req ClosePullRequestRequest) (ClosePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.PullID == "" {
		return ClosePullRequestResult{}, fmt.Errorf("pull id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID)
	if err != nil {
		return ClosePullRequestResult{}, err
	}
	var out githubPullRequestDetail
	if err := p.do(ctx, http.MethodPatch, endpoint, map[string]string{"state": "closed"}, &out); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.Comment != "" {
		comments, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.PullID, "comments")
		if err != nil {
			return ClosePullRequestResult{}, err
		}
		if err := p.do(ctx, http.MethodPost, comments, map[string]string{"body": req.Comment}, nil); err != nil {
			return ClosePullRequestResult{}, err
		}
	}
	state := "closed"
	operation := "close"
	if out.Merged {
		state = "merged"
		operation = "merge"
	}
	fields := map[string]FieldDigest{"state": {After: digestString(state)}}
	if req.Comment != "" {
		fields["comment"] = FieldDigest{After: digestString(req.Comment)}
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       out.HTMLURL,
		Operation: operation,
		Fields:    fields,
	})
	return ClosePullRequestResult{Number: out.Number, Merged: out.Merged, State: state}, nil
}

// UpdateBranchError is a typed rejection from GitHub's update-branch endpoint.
// StatusCode distinguishes lease/conflict validation failures (422) from
// permission failures (403) without requiring callers to parse error strings.
type UpdateBranchError struct {
	StatusCode int
	Message    string
}

func (e *UpdateBranchError) Error() string {
	return fmt.Sprintf("update pull request branch failed: status %d: %s", e.StatusCode, e.Message)
}

// UpdateBranch merges a pull request's current base into its head through
// GitHub's native update-branch endpoint. expected_head_sha is always sent:
// omitting the lease would allow a stale selector to update a head it never
// inspected.
func (p *GitHubProvider) UpdateBranch(ctx context.Context, req UpdateBranchRequest) (UpdateBranchResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return UpdateBranchResult{}, err
	}
	if req.PullID == "" {
		return UpdateBranchResult{}, fmt.Errorf("pull id is required")
	}
	if req.ExpectedHeadSHA == "" {
		return UpdateBranchResult{}, fmt.Errorf("expected head SHA is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "update-branch")
	if err != nil {
		return UpdateBranchResult{}, err
	}
	resp, err := p.send(ctx, http.MethodPut, endpoint, map[string]string{"expected_head_sha": req.ExpectedHeadSHA})
	if err != nil {
		return UpdateBranchResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return UpdateBranchResult{}, fmt.Errorf("read update-branch response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := strings.TrimSpace(string(body))
		var failure struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Message != "" {
			message = failure.Message
		}
		return UpdateBranchResult{}, &UpdateBranchError{
			StatusCode: resp.StatusCode,
			Message:    message,
		}
	}
	var out struct {
		Message string `json:"message"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return UpdateBranchResult{}, fmt.Errorf("decode update-branch response: %w", err)
	}
	number, _ := strconv.Atoi(req.PullID)
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       out.URL,
		Operation: "update-branch",
		Fields: map[string]FieldDigest{
			"headSha": {Before: digestString(req.ExpectedHeadSHA)},
		},
	})
	return UpdateBranchResult{Number: number, Message: out.Message, URL: out.URL}, nil
}

// MergePullRequest merges a GitHub pull request (issue #360) via the
// dedicated merge endpoint (PUT .../pulls/{number}/merge) — distinct from
// ClosePullRequest's PATCH state=closed, which merely closes without
// merging. GitHub refuses the request server-side (non-2xx, surfaced as an
// error by p.do) if req.ExpectedHeadSHA is set and no longer matches the
// PR's actual current head (405/409), or if the PR is not mergeable at all
// (draft, blocked by branch protection, merge conflict) — this method
// performs no policy check of its own; see MergePullRequestRequest's doc.
func (p *GitHubProvider) MergePullRequest(ctx context.Context, req MergePullRequestRequest) (MergePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return MergePullRequestResult{}, err
	}
	if req.PullID == "" {
		return MergePullRequestResult{}, fmt.Errorf("pull id is required")
	}
	if req.MergeMethod != "" && !req.MergeMethod.IsValid() {
		return MergePullRequestResult{}, fmt.Errorf("unsupported merge method %q", req.MergeMethod)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "merge")
	if err != nil {
		return MergePullRequestResult{}, err
	}
	body := map[string]interface{}{}
	if req.ExpectedHeadSHA != "" {
		body["sha"] = req.ExpectedHeadSHA
	}
	if req.CommitTitle != "" {
		body["commit_title"] = req.CommitTitle
	}
	if req.CommitMessage != "" {
		body["commit_message"] = req.CommitMessage
	}
	if req.MergeMethod != "" {
		body["merge_method"] = string(req.MergeMethod)
	}
	var out githubMergeResult
	if err := p.do(ctx, http.MethodPut, endpoint, body, &out); err != nil {
		return MergePullRequestResult{}, err
	}
	number, convErr := strconv.Atoi(req.PullID)
	if convErr != nil {
		number = 0
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		Operation: "merge",
		Fields:    map[string]FieldDigest{"state": {After: digestString("merged")}},
	})
	return MergePullRequestResult{Number: number, Merged: out.Merged, MergeSHA: out.SHA, Message: out.Message}, nil
}

// DetectMergePolicy reports req.Branch's active merge policy (issue #758)
// via GitHub's "get rules for a branch" endpoint (GET .../rules/branches/
// {branch}), which returns every ruleset rule that actually applies to the
// branch, regardless of which ruleset(s) define them. A "merge_queue"-typed
// rule present in that list means GitHub requires the merge queue for this
// branch; its absence means direct-merge (today's behavior, and classic
// branch-protection repos that have no rulesets at all). A read, so it does
// not emit a mutation event.
func (p *GitHubProvider) DetectMergePolicy(ctx context.Context, req RepoMergePolicyRequest) (RepoMergePolicyResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return RepoMergePolicyResult{}, err
	}
	if req.Branch == "" {
		return RepoMergePolicyResult{}, fmt.Errorf("branch is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "rules", "branches", req.Branch)
	if err != nil {
		return RepoMergePolicyResult{}, err
	}
	var rules []githubBranchRule
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &rules); err != nil {
		return RepoMergePolicyResult{}, err
	}
	for _, rule := range rules {
		if rule.Type == "merge_queue" {
			return RepoMergePolicyResult{Policy: MergePolicyMergeQueue}, nil
		}
	}
	return RepoMergePolicyResult{Policy: MergePolicyDirect}, nil
}

// enqueuePullRequestLookupQuery resolves the GraphQL node ID the enqueue
// mutation requires from the pull request number the rest of the codebase
// carries, and in the same round trip reads the two states that make the
// enqueue a no-op: already merged, and already sitting in the queue.
const enqueuePullRequestLookupQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      id
      merged
      mergeCommit{ oid }
      mergeQueueEntry{ state position }
    }
  }
}`

// enqueuePullRequestMutation is GitHub's only supported way to add a pull
// request to a merge queue. expectedHeadOid is the same optimistic-
// concurrency guard the REST merge endpoint spells "sha".
const enqueuePullRequestMutation = `mutation($pullRequestId:ID!,$expectedHeadOid:GitObjectID){
  enqueuePullRequest(input:{pullRequestId:$pullRequestId,expectedHeadOid:$expectedHeadOid}){
    mergeQueueEntry{ state position }
  }
}`

// EnqueuePullRequest adds a GitHub pull request to its repo's merge queue
// (issue #758) via the GraphQL enqueuePullRequest mutation.
//
// It previously used the REST merge endpoint (PUT .../pulls/{number}/merge)
// on the assumption that GitHub converts that call into an enqueue when the
// base branch requires a merge queue. That assumption is wrong, and issue
// #882 is the live evidence: against a queue-required branch, with the
// ruleset's own required merge method sent correctly, GitHub rejected the
// call outright — 405 "Repository rule violations found / Changes must be
// made through the merge queue" — rather than queuing anything. There is no
// REST endpoint for this operation at all; the GraphQL mutation is the only
// one, which is why `gh pr merge` reaches for it too.
//
// Two states make the mutation unnecessary, and both are checked first so a
// retried stage attempt is idempotent rather than an error:
//
//   - already merged (the queue landed it between attempts) — reported as
//     Merged=true with the real merge commit, which internal/mergepolicy's
//     enqueueLander maps to Outcome=merged;
//   - already enqueued — reported as a successful no-op enqueue, since the
//     desired end state already holds.
//
// Merged=true on the mutation path is impossible by construction: unlike
// the REST endpoint, enqueueing never merges inline, so a queue with
// nothing ahead of this pull request still yields an entry to poll rather
// than an immediate merge.
//
// req.MergeMethod is deliberately not sent. The queue's merge method comes
// from the repository ruleset's merge_queue rule, not from the enqueue
// call, and the mutation accepts no such field — #877's fix remains correct
// for the direct-merge path and is simply inapplicable here.
func (p *GitHubProvider) EnqueuePullRequest(ctx context.Context, req EnqueuePullRequestRequest) (EnqueuePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return EnqueuePullRequestResult{}, err
	}
	if req.PullID == "" {
		return EnqueuePullRequestResult{}, fmt.Errorf("pull id is required")
	}
	number, err := strconv.Atoi(req.PullID)
	if err != nil {
		// The GraphQL lookup takes the number as an Int, so a non-numeric
		// pull id cannot be resolved to a node at all — fail with that
		// reason rather than sending a request GitHub will reject opaquely.
		return EnqueuePullRequestResult{}, fmt.Errorf("pull id %q must be a pull request number: %w", req.PullID, err)
	}

	var lookup struct {
		Repository struct {
			PullRequest *struct {
				ID          string `json:"id"`
				Merged      bool   `json:"merged"`
				MergeCommit *struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
				MergeQueueEntry *struct {
					State    string `json:"state"`
					Position int    `json:"position"`
				} `json:"mergeQueueEntry"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := p.graphql(ctx, enqueuePullRequestLookupQuery, map[string]interface{}{
		"owner":  req.Repository.Owner,
		"name":   req.Repository.Name,
		"number": number,
	}, &lookup); err != nil {
		return EnqueuePullRequestResult{}, err
	}
	pr := lookup.Repository.PullRequest
	if pr == nil || pr.ID == "" {
		return EnqueuePullRequestResult{}, fmt.Errorf("pull request %s/%s#%d not found", req.Repository.Owner, req.Repository.Name, number)
	}

	if pr.Merged {
		mergeSHA := ""
		if pr.MergeCommit != nil {
			mergeSHA = pr.MergeCommit.OID
		}
		return EnqueuePullRequestResult{
			Number:   number,
			Merged:   true,
			MergeSHA: mergeSHA,
			Message:  "pull request is already merged",
		}, nil
	}
	if pr.MergeQueueEntry != nil {
		p.recordEnqueue(ctx, req.Repository, req.PullID)
		return EnqueuePullRequestResult{
			Number:  number,
			Message: fmt.Sprintf("pull request is already enqueued (state %s, position %d)", pr.MergeQueueEntry.State, pr.MergeQueueEntry.Position),
		}, nil
	}

	variables := map[string]interface{}{"pullRequestId": pr.ID}
	if req.ExpectedHeadSHA != "" {
		variables["expectedHeadOid"] = req.ExpectedHeadSHA
	}
	var mutation struct {
		EnqueuePullRequest struct {
			MergeQueueEntry *struct {
				State    string `json:"state"`
				Position int    `json:"position"`
			} `json:"mergeQueueEntry"`
		} `json:"enqueuePullRequest"`
	}
	if err := p.graphql(ctx, enqueuePullRequestMutation, variables, &mutation); err != nil {
		return EnqueuePullRequestResult{}, err
	}

	p.recordEnqueue(ctx, req.Repository, req.PullID)
	message := "pull request enqueued"
	if entry := mutation.EnqueuePullRequest.MergeQueueEntry; entry != nil {
		message = fmt.Sprintf("pull request enqueued (state %s, position %d)", entry.State, entry.Position)
	}
	return EnqueuePullRequestResult{Number: number, Message: message}, nil
}

// recordEnqueue journals the enqueue as a mutation of the pull request's
// external ref, so a queued-but-not-yet-merged pull request is as visible
// in the run journal as a merged one.
func (p *GitHubProvider) recordEnqueue(ctx context.Context, repo RepositoryRef, pullID string) {
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(repo, pullID),
		Operation: "enqueue",
		Fields:    map[string]FieldDigest{"state": {After: digestString("enqueued")}},
	})
}

// pollMergeQueueEntryQuery reads the pull request's own state and its live
// merge queue entry in one round trip. The entry is the only surface that
// distinguishes "still queued" from "no longer queued" — REST exposes
// nothing equivalent.
const pollMergeQueueEntryQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      state
      merged
      mergeCommit{ oid }
      mergeQueueEntry{ state position }
    }
  }
}`

// PollMergeQueueEntry reports whether the merge queue has since merged or
// evicted a pull request previously enqueued via EnqueuePullRequest (issue
// #758), by reading the pull request's live merge queue entry over GraphQL.
//
// This previously re-polled the pull request over REST and classified on
// pr.State == "closed" alone. That never fires for a real eviction: GitHub
// leaves an evicted pull request OPEN and simply removes its queue entry,
// so every eviction reported Pending until the caller's poll timed out, and
// mergeQueuePollEvicted's goobers:needs-remediation routing — #758's own
// acceptance criterion — could never run (issue #885). The old doc flagged
// exactly this as needing re-validation "once #759 actually enables the
// queue live"; it since has, and REST turned out to be the wrong surface.
//
// Classification:
//
//   - merged: unambiguous, and the merge commit is reported as such. Never
//     the head SHA — under the squash method a merge queue requires, the
//     commit that lands on the base branch is a brand-new SHA that can
//     never equal the pull request's head.
//   - closed without merging: evicted-and-closed, still first-class.
//   - open, unmerged, entry present: pending; the caller's own bounded poll
//     loop keeps watching.
//   - open, unmerged, NO entry: absent. For a pull request the caller
//     enqueued, that is what an eviction looks like — but it is also what
//     the moments right after a successful enqueue look like, so the
//     distinction is left to the caller, which is the only party that
//     knows whether it has already seen an entry. See
//     MergeQueueEntryAbsent's doc.
//
// A read, so it does not emit a mutation event.
func (p *GitHubProvider) PollMergeQueueEntry(ctx context.Context, req PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PollMergeQueueEntryResult{}, err
	}
	if req.PullID == "" {
		return PollMergeQueueEntryResult{}, fmt.Errorf("pull id is required")
	}
	number, err := strconv.Atoi(req.PullID)
	if err != nil {
		return PollMergeQueueEntryResult{}, fmt.Errorf("pull id %q must be a pull request number: %w", req.PullID, err)
	}
	var out struct {
		Repository struct {
			PullRequest *struct {
				State       string `json:"state"`
				Merged      bool   `json:"merged"`
				MergeCommit *struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
				MergeQueueEntry *struct {
					State    string `json:"state"`
					Position int    `json:"position"`
				} `json:"mergeQueueEntry"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := p.graphql(ctx, pollMergeQueueEntryQuery, map[string]interface{}{
		"owner":  req.Repository.Owner,
		"name":   req.Repository.Name,
		"number": number,
	}, &out); err != nil {
		return PollMergeQueueEntryResult{}, err
	}
	pr := out.Repository.PullRequest
	if pr == nil {
		return PollMergeQueueEntryResult{}, fmt.Errorf("pull request %s/%s#%d not found", req.Repository.Owner, req.Repository.Name, number)
	}
	if pr.Merged {
		mergeSHA := ""
		if pr.MergeCommit != nil {
			mergeSHA = pr.MergeCommit.OID
		}
		return PollMergeQueueEntryResult{State: MergeQueueEntryMerged, MergeSHA: mergeSHA}, nil
	}
	if strings.EqualFold(pr.State, "closed") {
		return PollMergeQueueEntryResult{State: MergeQueueEntryEvicted}, nil
	}
	if pr.MergeQueueEntry == nil {
		return PollMergeQueueEntryResult{State: MergeQueueEntryAbsent}, nil
	}
	return PollMergeQueueEntryResult{State: MergeQueueEntryPending, QueueState: pr.MergeQueueEntry.State, QueuePosition: pr.MergeQueueEntry.Position}, nil
}

// ListPullRequests lists open pull requests targeting req.Base, filtered
// client-side to those whose head branch starts with req.HeadPrefix —
// merge-review's selection stage and sibling-set context gathering (issue
// #359), and #361's post-merge fan-out (find every other open PR targeting
// the branch a just-merged PR targeted). GitHub's pulls-list API has no
// server-side prefix match on head (only an exact head=owner:branch filter,
// which FindPullRequestByBranch already uses for the single-branch case), so
// the prefix filter is applied here instead. A read, so it does not emit a
// mutation event.
func (p *GitHubProvider) ListPullRequests(ctx context.Context, req ListPullRequestsRequest) ([]PullRequestSummary, error) {
	return p.listPullRequests(ctx, req, "open", time.Time{})
}

// GetPullRequest returns one pull request's current state and metadata without
// resolving reviews, comments, or check runs.
func (p *GitHubProvider) GetPullRequest(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestSummary, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return PullRequestSummary{}, err
	}
	if pullID == "" {
		return PullRequestSummary{}, fmt.Errorf("pull id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID)
	if err != nil {
		return PullRequestSummary{}, err
	}
	var pr githubPullRequestDetail
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return PullRequestSummary{}, err
	}
	return summarizePullRequest(pr, ""), nil
}

// ListRecentlyClosedPullRequests lists pull requests closed or merged since
// updatedSince. It is the bounded terminal-PR complement to ListPullRequests
// used when a workflow needs current state for recently relevant siblings.
func (p *GitHubProvider) ListRecentlyClosedPullRequests(ctx context.Context, req ListPullRequestsRequest, updatedSince time.Time) ([]PullRequestSummary, error) {
	if updatedSince.IsZero() {
		return nil, fmt.Errorf("updatedSince is required")
	}
	req.SkipCheckState = true
	return p.listPullRequests(ctx, req, "closed", updatedSince)
}

func (p *GitHubProvider) listPullRequests(ctx context.Context, req ListPullRequestsRequest, state string, updatedSince time.Time) ([]PullRequestSummary, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return nil, err
	}
	values := url.Values{"state": []string{state}}
	if req.Base != "" {
		values.Set("base", req.Base)
	}
	if !updatedSince.IsZero() {
		values.Set("sort", "updated")
		values.Set("direction", "desc")
	}
	endpoint, err = addQuery(endpoint, values)
	if err != nil {
		return nil, err
	}

	var prs []githubPullRequestDetail
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageOut []githubPullRequestDetail
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode pulls page: %w", err)
		}
		for _, pr := range pageOut {
			if !updatedSince.IsZero() && pr.UpdatedAt.Before(updatedSince) {
				return errStopPaging
			}
			if !updatedSince.IsZero() {
				closedRecently := pr.ClosedAt != nil && !pr.ClosedAt.Before(updatedSince)
				mergedRecently := pr.MergedAt != nil && !pr.MergedAt.Before(updatedSince)
				if !closedRecently && !mergedRecently {
					continue
				}
			}
			prs = append(prs, pr)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	out := make([]PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		if req.HeadPrefix != "" && !strings.HasPrefix(pr.Head.Ref, req.HeadPrefix) {
			continue
		}
		var checkState CheckState
		if !req.SkipCheckState {
			checkState, _, err = p.combinedCheckState(ctx, req.Repository, pr.Head.SHA)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, summarizePullRequest(pr, checkState))
	}
	return out, nil
}

func summarizePullRequest(pr githubPullRequestDetail, checkState CheckState) PullRequestSummary {
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, l.Name)
	}
	return PullRequestSummary{
		ID:         strconv.Itoa(pr.Number),
		Number:     pr.Number,
		URL:        pr.HTMLURL,
		State:      pr.State,
		Merged:     pr.Merged || pr.MergedAt != nil,
		Head:       pr.Head.Ref,
		Base:       pr.Base.Ref,
		HeadSHA:    pr.Head.SHA,
		BaseSHA:    pr.Base.SHA,
		Draft:      pr.Draft,
		Labels:     labels,
		CheckState: checkState,
		UpdatedAt:  pr.UpdatedAt,
		Body:       pr.Body,
	}
}

// PullRequestFiles lists the files pullID touches — merge-review's
// sibling-set context gathering (issue #359): what does the OTHER open PR
// change, for cross-PR conflict/drift detection. A read, so it does not
// emit a mutation event.
func (p *GitHubProvider) PullRequestFiles(ctx context.Context, repo RepositoryRef, pullID string) ([]ChangedFile, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, fmt.Errorf("pull id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "files")
	if err != nil {
		return nil, err
	}
	var files []githubPullRequestFile
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageOut []githubPullRequestFile
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode pull files page: %w", err)
		}
		files = append(files, pageOut...)
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]ChangedFile, 0, len(files))
	for _, f := range files {
		out = append(out, ChangedFile{Path: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch})
	}
	return out, nil
}

// RepositoryFileContent returns one file's contents at ref.
func (p *GitHubProvider) RepositoryFileContent(ctx context.Context, repo RepositoryRef, path, ref string) ([]byte, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("file path is required")
	}
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "contents", path)
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"ref": []string{ref}})
	if err != nil {
		return nil, err
	}
	resp, err := p.sendWithAccept(ctx, http.MethodGet, endpoint, nil, "application/vnd.github.raw+json")
	if err != nil {
		return nil, err
	}
	content, _, err := readPage(resp, http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// CompareCommits reports base and head's common ancestor plus the
// file-level diff between them (issue #718) via GitHub's three-dot compare
// endpoint — the same computation GitHub itself performs for a PR's own
// "files" view (pulls/{n}/files is exactly compare(base...head) for that
// PR's current head/base), exposed here for arbitrary ref/SHA pairs so a
// caller can also ask "what changed on base between two points in its own
// history" (merge-review's cache re-keying, merge-pr's delta-aware SHA-pin
// check) without either point being a PR's live head. A read, so it does
// not emit a mutation event.
func (p *GitHubProvider) CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return CompareResult{}, err
	}
	if base == "" || head == "" {
		return CompareResult{}, fmt.Errorf("base and head are both required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "compare", base+"..."+head)
	if err != nil {
		return CompareResult{}, err
	}
	var mergeBaseSHA string
	var files []githubPullRequestFile
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var resp githubCompareResponse
		if err := json.Unmarshal(page, &resp); err != nil {
			return fmt.Errorf("decode compare page: %w", err)
		}
		if mergeBaseSHA == "" {
			mergeBaseSHA = resp.MergeBaseCommit.SHA
		}
		files = append(files, resp.Files...)
		return nil
	}); err != nil {
		return CompareResult{}, err
	}
	out := CompareResult{MergeBaseSHA: mergeBaseSHA, Files: make([]ChangedFile, 0, len(files))}
	for _, f := range files {
		out.Files = append(out.Files, ChangedFile{Path: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch})
	}
	return out, nil
}

// PullRequestMergeable resolves pullID's current GitHub-computed mergeable
// flag via a single-PR detail GET — issue #715's post-merge triage needs
// exactly this one field per sibling, not the review-decision/check-state/
// comments PollPullRequest also resolves (three extra requests it has no use
// for here). Returns nil when GitHub reports null (mergeability still being
// computed asynchronously — a normal, common state right after a merge
// changes a sibling's target, not an error): the caller must treat "unknown"
// as distinct from "known conflicted", since treating a computing-in-progress
// PR as conflicted would false-positive-label a PR that turns out clean once
// GitHub finishes. A read, so it does not emit a mutation event.
func (p *GitHubProvider) PullRequestMergeable(ctx context.Context, repo RepositoryRef, pullID string) (*bool, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, fmt.Errorf("pull id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID)
	if err != nil {
		return nil, err
	}
	var pr struct {
		Mergeable *bool `json:"mergeable"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return nil, err
	}
	return pr.Mergeable, nil
}

// reviewDecision aggregates a PR's review list into a single decision: the
// latest review per author wins, and any outstanding CHANGES_REQUESTED beats
// any APPROVED (BL-031 review-decision normalization).
func (p *GitHubProvider) reviewDecision(ctx context.Context, repo RepositoryRef, pullID string) (ReviewDecision, int, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "reviews")
	if err != nil {
		return "", 0, err
	}
	var reviews []githubReview
	// Follow pagination: a CHANGES_REQUESTED review on page 2+ would otherwise
	// be invisible and a truly-blocked PR would read as Approved (#139).
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageItems []githubReview
		if err := json.Unmarshal(page, &pageItems); err != nil {
			return fmt.Errorf("decode reviews page: %w", err)
		}
		reviews = append(reviews, pageItems...)
		return nil
	}); err != nil {
		return "", 0, err
	}
	latest := map[string]string{}
	order := map[string]int{}
	for i, review := range reviews {
		login := review.User.Login
		state := strings.ToUpper(review.State)
		if state == "COMMENTED" {
			continue // comment-only reviews carry no verdict
		}
		if prev, ok := order[login]; !ok || i > prev {
			latest[login] = state
			order[login] = i
		}
	}
	requestedChanges := 0
	approved := false
	for _, state := range latest {
		switch state {
		case "CHANGES_REQUESTED":
			requestedChanges++
		case "APPROVED":
			approved = true
		}
	}
	switch {
	case requestedChanges > 0:
		return ReviewDecisionChangesRequested, requestedChanges, nil
	case approved:
		return ReviewDecisionApproved, 0, nil
	default:
		return ReviewDecisionPending, 0, nil
	}
}

// RefCheckState resolves ref's combined check state on demand — the
// per-candidate resolution ListPullRequests does by default, exposed for
// callers that list with SkipCheckState and then resolve only the
// candidates whose state they cannot reuse from a prior gather (issue
// #523's sibling-context cache). A read, so it does not emit a mutation
// event.
func (p *GitHubProvider) RefCheckState(ctx context.Context, repo RepositoryRef, ref string) (CheckState, error) {
	state, _, err := p.combinedCheckState(ctx, repo, ref)
	return state, err
}

// combinedCheckState normalizes GitHub's legacy combined status plus check-runs
// into a single CheckState + per-check detail refs (BL-031).
func (p *GitHubProvider) combinedCheckState(ctx context.Context, repo RepositoryRef, ref string) (CheckState, []CheckDetail, error) {
	if ref == "" {
		return CheckStatePending, nil, nil
	}
	var details []CheckDetail
	failing, pending := false, false

	statusEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "status")
	if err != nil {
		return "", nil, err
	}
	var statuses []githubStatus
	if err := p.getAllPages(ctx, statusEndpoint, func(page []byte) error {
		var pageOut githubCombinedStatus
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode combined status page: %w", err)
		}
		statuses = append(statuses, pageOut.Statuses...)
		return nil
	}); err != nil {
		return "", nil, err
	}
	for _, status := range statuses {
		state := normalizeCombinedStatusState(status.State)
		details = append(details, CheckDetail{Name: status.Context, State: state, URL: status.TargetURL, Summary: status.Description})
		switch state {
		case CheckStateFailing:
			failing = true
		case CheckStatePending:
			pending = true
		}
	}

	runsEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "check-runs")
	if err != nil {
		return "", nil, err
	}
	var checkRuns []githubCheckRun
	// The single biggest silent failure in the cluster: a failing check-run on
	// page 2+ would be unseen and the ci-gate would pass a red PR (#139).
	if err := p.getAllPages(ctx, runsEndpoint, func(page []byte) error {
		var pageOut githubCheckRunsResponse
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode check-runs page: %w", err)
		}
		checkRuns = append(checkRuns, pageOut.CheckRuns...)
		return nil
	}); err != nil {
		return "", nil, err
	}
	for _, run := range checkRuns {
		state := normalizeCheckRunState(run.Status, run.Conclusion)
		details = append(details, CheckDetail{Name: run.Name, State: state, URL: run.HTMLURL, Summary: run.Output.Summary})
		switch state {
		case CheckStateFailing:
			failing = true
		case CheckStatePending:
			pending = true
		}
	}

	switch {
	case failing:
		return CheckStateFailing, details, nil
	case pending || len(details) == 0:
		return CheckStatePending, details, nil
	default:
		return CheckStatePassing, details, nil
	}
}

func (p *GitHubProvider) pullRequestComments(ctx context.Context, repo RepositoryRef, pullID string, since *time.Time) ([]PullRequestComment, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", pullID, "comments")
	if err != nil {
		return nil, err
	}
	if since != nil {
		endpoint, err = addQuery(endpoint, url.Values{"since": []string{since.UTC().Format(time.RFC3339)}})
		if err != nil {
			return nil, err
		}
	}
	comments := make([]PullRequestComment, 0)
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var raw []githubIssueComment
		if err := json.Unmarshal(page, &raw); err != nil {
			return fmt.Errorf("decode pull request comments page: %w", err)
		}
		for _, c := range raw {
			comments = append(comments, PullRequestComment{ID: c.ID, Author: c.User.Login, Body: c.Body, URL: c.HTMLURL, CreatedAt: c.CreatedAt})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return comments, nil
}

func normalizeCombinedStatusState(state string) CheckState {
	switch strings.ToLower(state) {
	case "success":
		return CheckStatePassing
	case "failure", "error":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
}

func normalizeCheckRunState(status, conclusion string) CheckState {
	if strings.ToLower(status) != "completed" {
		return CheckStatePending
	}
	switch strings.ToLower(conclusion) {
	case "success", "neutral", "skipped":
		return CheckStatePassing
	case "failure", "timed_out", "cancelled", "action_required", "stale", "startup_failure":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
}

// RequestReview requests GitHub reviewers for a pull request.
func (p *GitHubProvider) RequestReview(ctx context.Context, req ReviewRequest) error {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return err
	}
	if req.PullID == "" {
		return fmt.Errorf("pull id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "requested_reviewers")
	if err != nil {
		return err
	}
	body := map[string][]string{"reviewers": req.Reviewers}
	if err := p.do(ctx, http.MethodPost, endpoint, body, nil); err != nil {
		return err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		Operation: "request-review",
		Fields: map[string]FieldDigest{
			"reviewers": {After: digestString(strings.Join(req.Reviewers, ","))},
		},
	})
	return nil
}

// SubmitPullRequestReview publishes a SHA-pinned native GitHub review. GitHub
// associates the review with commit_id, allowing branch-protection
// stale-dismissal to invalidate an approval when the pull request moves.
func (p *GitHubProvider) SubmitPullRequestReview(ctx context.Context, req PullRequestReviewRequest) (PullRequestReviewResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestReviewResult{}, err
	}
	if req.PullID == "" {
		return PullRequestReviewResult{}, fmt.Errorf("pull id is required")
	}
	if req.CommitSHA == "" {
		return PullRequestReviewResult{}, fmt.Errorf("commit sha is required")
	}
	if req.Body == "" {
		return PullRequestReviewResult{}, fmt.Errorf("review body is required")
	}

	var event string
	switch req.Decision {
	case ReviewDecisionApproved:
		event = "APPROVE"
	case ReviewDecisionChangesRequested:
		event = "REQUEST_CHANGES"
	default:
		return PullRequestReviewResult{}, fmt.Errorf("unsupported review decision %q", req.Decision)
	}

	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "reviews")
	if err != nil {
		return PullRequestReviewResult{}, err
	}
	body := map[string]string{
		"body":      req.Body,
		"commit_id": req.CommitSHA,
		"event":     event,
	}
	var out struct {
		ID       int64  `json:"id"`
		HTMLURL  string `json:"html_url"`
		CommitID string `json:"commit_id"`
		State    string `json:"state"`
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestReviewResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       out.HTMLURL,
		Operation: "review",
		Fields: map[string]FieldDigest{
			"body":      {After: digestString(req.Body)},
			"commitSha": {After: digestString(req.CommitSHA)},
			"decision":  {After: digestString(string(req.Decision))},
		},
	})
	return PullRequestReviewResult{
		ID:        out.ID,
		URL:       out.HTMLURL,
		CommitSHA: req.CommitSHA,
		Decision:  req.Decision,
	}, nil
}

// ListWorkItems lists GitHub issues as unified work items.
func (p *GitHubProvider) ListWorkItems(ctx context.Context, req ListWorkItemsRequest) ([]WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
	if err != nil {
		return nil, err
	}
	values := url.Values{"state": []string{"all"}}
	if req.State != "" {
		values.Set("state", req.State)
	}
	if len(req.Labels) > 0 {
		values.Set("labels", strings.Join(req.Labels, ","))
	}
	if req.Assignee != "" {
		values.Set("assignee", req.Assignee)
	}
	if req.UpdatedSince != nil {
		values.Set("since", req.UpdatedSince.UTC().Format(time.RFC3339))
	}
	if req.OldestFirst {
		// GitHub's issues list defaults to newest-first (sort=created,
		// direction=desc — undocumented but confirmed live, #532). An explicit
		// ascending sort makes the fetch itself FIFO, so a Limit-truncated read
		// drops the newest items (still reachable after older ones drain)
		// instead of permanently starving the oldest.
		values.Set("sort", "created")
		values.Set("direction", "asc")
	}
	if req.Page > 0 {
		// An explicit Page means the caller drives pagination itself: honor it
		// as a single-page read (its own per_page, no Link following).
		if req.Limit > 0 {
			values.Set("per_page", strconv.Itoa(req.Limit))
		}
		values.Set("page", strconv.Itoa(req.Page))
		endpoint, err = addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		var issues []githubIssue
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &issues); err != nil {
			return nil, err
		}
		return issuesToWorkItems(issues, req.Limit), nil
	}

	endpoint, err = addQuery(endpoint, values)
	if err != nil {
		return nil, err
	}
	// Follow pagination and accumulate up to Limit NON-PR items. The issues
	// endpoint also returns pull requests (excluded — PRs are the repo
	// provider's surface, #13); filtering them out of a single Limit-sized page
	// silently returned fewer than Limit real issues (#139).
	var items []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode issues page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest != nil {
				continue
			}
			items = append(items, mapGitHubIssue(issue))
			if req.Limit > 0 && len(items) >= req.Limit {
				return errStopPaging
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// issuesToWorkItems maps a page of GitHub issues to WorkItems, skipping pull
// requests, and truncates to limit (0 = no cap).
func issuesToWorkItems(issues []githubIssue, limit int) []WorkItem {
	items := make([]WorkItem, 0, len(issues))
	for _, issue := range issues {
		if issue.PullRequest != nil {
			continue
		}
		items = append(items, mapGitHubIssue(issue))
		if limit > 0 && len(items) >= limit {
			break
		}
	}
	return items
}

// GetWorkItem reads a GitHub issue as a unified work item.
func (p *GitHubProvider) GetWorkItem(ctx context.Context, repo RepositoryRef, id string) (WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return WorkItem{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id)
	if err != nil {
		return WorkItem{}, err
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &issue); err != nil {
		return WorkItem{}, err
	}
	return mapGitHubIssue(issue), nil
}

// ListWorkItemChildren returns the provider-native sub-issues of a GitHub issue.
func (p *GitHubProvider) ListWorkItemChildren(ctx context.Context, repo RepositoryRef, id string) ([]WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("issue id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "sub_issues")
	if err != nil {
		return nil, err
	}
	var items []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode sub-issues page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest == nil {
				items = append(items, mapGitHubIssue(issue))
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// HasOpenWorkItemBlocker reports whether a GitHub issue has a native blocker
// that is still open.
func (p *GitHubProvider) HasOpenWorkItemBlocker(ctx context.Context, repo RepositoryRef, id string) (bool, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return false, err
	}
	if id == "" {
		return false, fmt.Errorf("issue id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "dependencies", "blocked_by")
	if err != nil {
		return false, err
	}
	open := false
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode blocked-by dependencies page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest == nil && strings.EqualFold(issue.State, "open") {
				open = true
				return errStopPaging
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	return open, nil
}

// CreateWorkItem creates a GitHub issue from a unified work item request.
func (p *GitHubProvider) CreateWorkItem(ctx context.Context, req CreateWorkItemRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
	if err != nil {
		return WorkItem{}, err
	}
	// Idempotency (#140): a prior attempt's POST may have committed on the
	// server before its response reached us (a timeout), so a policy retry must
	// not file a duplicate. When the caller supplies a RunID we stamp a
	// run-id footer into the body and, before creating, search for an existing
	// item carrying it — returning that instead. Best-effort: GitHub's search
	// index is eventually consistent, so a retry within a second or two of the
	// original may still miss it; the footer at least makes any duplicate
	// traceable and recordExternalRef journals every create.
	itemBody := withRunIDFooter(req.Body, req.RunID)
	if req.RunID != "" {
		if existing, found, err := p.findRunItem(ctx, req.Repository, req.RunID); err != nil {
			return WorkItem{}, err
		} else if found {
			return existing, nil
		}
	}
	labels := replaceStatusLabel(req.Labels, req.Status)
	body := map[string]interface{}{
		"title":  req.Title,
		"body":   itemBody,
		"labels": labels,
	}
	if req.Assignee != "" {
		body["assignees"] = []string{req.Assignee}
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodPost, endpoint, body, &issue); err != nil {
		return WorkItem{}, err
	}
	item := mapGitHubIssue(issue)
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, strconv.Itoa(issue.Number)),
		URL:       item.URL,
		Operation: "create",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(req.Title)},
			"body":  {After: digestString(itemBody)},
		},
	})
	return item, nil
}

// findRunItem searches the repo for an issue whose body carries the run-id
// footer for runID, used by CreateWorkItem for idempotency (#140). The search
// term is fuzzy, so the match is confirmed against the exact footer before
// returning.
func (p *GitHubProvider) findRunItem(ctx context.Context, repo RepositoryRef, runID string) (WorkItem, bool, error) {
	endpoint, err := joinURL(p.BaseURL, "search", "issues")
	if err != nil {
		return WorkItem{}, false, err
	}
	query := fmt.Sprintf(`repo:%s/%s in:body type:issue "%s"`, repo.Owner, repo.Name, runFooter(runID))
	endpoint, err = addQuery(endpoint, url.Values{"q": {query}, "per_page": {"20"}})
	if err != nil {
		return WorkItem{}, false, err
	}
	var out struct {
		Items []githubIssue `json:"items"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return WorkItem{}, false, err
	}
	for _, issue := range out.Items {
		if strings.Contains(issue.Body, runFooter(runID)) {
			return mapGitHubIssue(issue), true, nil
		}
	}
	return WorkItem{}, false, nil
}

// UpdateWorkItemStatus mirrors Goobers processing status to GitHub labels.
func (p *GitHubProvider) UpdateWorkItemStatus(ctx context.Context, req UpdateWorkItemStatusRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	// Swap only the status label via the label sub-API (add new, remove any
	// stale status labels) rather than PATCHing the whole label set. A
	// read-modify-write of all labels would silently clobber a label a human or
	// the curator added between our GET above and the write — the status mirror
	// has no business overwriting unrelated labels (#140).
	newLabel := statusLabel(req.Status)
	var remove []string
	for _, l := range current.Labels {
		if strings.HasPrefix(l, statusLabelPrefix) && l != newLabel {
			remove = append(remove, l)
		}
	}
	if err := p.applyLabelChanges(ctx, req.Repository, req.ID, []string{newLabel}, remove); err != nil {
		return WorkItem{}, err
	}
	if req.Status == WorkItemStatusDone {
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID)
		if err != nil {
			return WorkItem{}, err
		}
		// state-only PATCH — labels are handled above, so closing never
		// round-trips (and races) the label set.
		if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"state": "closed"}, nil); err != nil {
			return WorkItem{}, err
		}
	}
	if req.Comment != "" {
		comments, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID, "comments")
		if err != nil {
			return WorkItem{}, err
		}
		if err := p.do(ctx, http.MethodPost, comments, map[string]string{"body": req.Comment}, nil); err != nil {
			return WorkItem{}, err
		}
	}
	item, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.ID),
		URL:       item.URL,
		Operation: "status",
		Fields: map[string]FieldDigest{
			"status": {Before: digestString(string(statusFromLabels(current.Labels, current.State))), After: digestString(string(req.Status))},
		},
	})
	return item, nil
}

// Subscribe emits GitHub backlog item availability events.
//
// NOT WIRED YET — banner per #140 item 5. Two issues to resolve before anyone
// depends on this: (1) the poll loop silently swallows ListWorkItems errors
// (a persistent failure looks like an empty backlog forever, not an error);
// (2) the `seen` map grows unbounded for the process lifetime. At V0 the
// scheduler triggers via cron backlog-query stages, not this in-process
// subscription, so it has no live caller; fix both before it gets one.
func (p *GitHubProvider) Subscribe(ctx context.Context, sub TriggerSubscription) (<-chan WorkItemEvent, error) {
	if sub.Kind != TriggerPolling {
		return nil, fmt.Errorf("github provider supports polling subscriptions in-process; webhook delivery is configured externally")
	}
	interval := sub.PollInterval
	if interval <= 0 {
		interval = time.Minute
	}
	events := make(chan WorkItemEvent, 1)
	go func() {
		defer close(events)
		seen := map[string]time.Time{}
		for {
			items, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: sub.Repository, State: "open", Limit: 100})
			if err == nil {
				for _, item := range items {
					if !shouldEmitWorkItem(seen, item) {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case events <- WorkItemEvent{Provider: ProviderGitHub, Kind: TriggerPolling, Item: item, Action: "available"}:
					}
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return events, nil
}

func (p *GitHubProvider) contentSHA(ctx context.Context, endpoint string) (string, bool, error) {
	// Routed through send so this read gets the same rate-limit/5xx/transport
	// retries as every other request — it previously issued a raw Do and a
	// single blip failed the caller outright (#139). The 404 = "no such
	// content" semantic below is preserved (send does not retry 404).
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", false, newProviderResponseError(resp, http.MethodGet, endpoint, body)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, fmt.Errorf("decode response: %w", err)
	}
	return out.SHA, out.SHA != "", nil
}

// BranchTipSHA resolves the current commit SHA at the tip of branch — the
// live base-branch head. The merge-escalated self-heal check (#1052) needs
// this because GitHub's pull_request.base.sha is a PINNED commit: it does not
// advance when the base branch does (only when the PR head is synchronized),
// so an escalation snapshot must compare against this live tip — not the PR's
// own BaseSHA — to detect a sibling merge having advanced the base.
func (p *GitHubProvider) BranchTipSHA(ctx context.Context, repo RepositoryRef, branch string) (string, error) {
	ref, err := p.getGitHubRef(ctx, repo, "heads/"+branch)
	if err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

func (p *GitHubProvider) getGitHubRef(ctx context.Context, repo RepositoryRef, ref string) (githubRef, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "git", "ref", ref)
	if err != nil {
		return githubRef{}, err
	}
	var out githubRef
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return githubRef{}, err
	}
	return out, nil
}

func (p *GitHubProvider) do(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	return p.doStatus(ctx, method, endpoint, body, out, nil)
}

// send issues one GitHub request, retrying transient failures — rate limits,
// 5xx server errors, and transport errors — with bounded backoff (up to
// p.maxRetries attempts and, for rate limits, up to maxRateLimitWait total
// sleep, honoring X-RateLimit-Reset/Retry-After). It returns the final
// response for the caller to consume and close; a nil error guarantees a
// non-nil response. A rate limit that cannot be absorbed within those
// budgets returns a typed *RateLimitError (#614) rather than the response,
// so no caller ever folds it into a generic non-2xx string error. Callers
// that only need a decoded body should use doStatus; getAllPages uses send
// directly so it can read the Link header for pagination (#139).
func (p *GitHubProvider) send(ctx context.Context, method, endpoint string, body interface{}) (*http.Response, error) {
	return p.sendWithAccept(ctx, method, endpoint, body, "application/vnd.github+json")
}

func (p *GitHubProvider) sendWithAccept(ctx context.Context, method, endpoint string, body interface{}, accept string) (*http.Response, error) {
	maxWait := p.maxRateLimitWait
	if maxWait <= 0 {
		maxWait = defaultRateLimitMaxWait
	}
	var rateLimitWaited time.Duration
	for attempt := 0; ; attempt++ {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		token, err := p.resolveToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClientOrDefault(p.Client).Do(req)
		if err != nil {
			// Transport error (connection reset, DNS blip, timeout): retry with
			// backoff rather than fail the stage on a single network hiccup
			// (#139). No response to close on this path.
			if attempt < p.maxRetries {
				if serr := p.sleep(ctx, backoffDuration(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, fmt.Errorf("send request: %w", err)
		}
		if isRateLimited(resp) {
			wait, ev := p.rateLimitPlan(resp, endpoint, attempt)
			_ = resp.Body.Close()
			if attempt >= p.maxRetries || wait > maxWait-rateLimitWaited {
				// Waiting can't help within this request's budget — the
				// retry allowance is spent, or the reset is further out than
				// the wait budget allows (#614). Fail FAST with the typed
				// error so the caller (and the run journal) sees "rate
				// limited, resets at <t>" instead of a generic 403 string,
				// and no time is burned sleeping toward a wait that cannot
				// reach the reset anyway.
				ev.Outcome = RateLimitOutcomeExhausted
				p.observeRateLimit(ctx, ev)
				return nil, rateLimitErrorFrom(ev)
			}
			if err := p.sleep(ctx, wait); err != nil {
				ev.Outcome = RateLimitOutcomeCanceled
				p.observeRateLimit(ctx, ev)
				return nil, err
			}
			ev.Outcome = RateLimitOutcomeRetry
			p.observeRateLimit(ctx, ev)
			rateLimitWaited += wait
			continue
		}
		if resp.StatusCode >= 500 && attempt < p.maxRetries {
			// Server-side error: retry with backoff. GitHub 5xx is usually
			// transient; without this a single blip fails the stage attempt.
			_ = resp.Body.Close()
			if err := p.sleep(ctx, backoffDuration(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
}

// doStatus performs a GitHub request with transient-failure retries (see send).
// Status codes in allowStatus are treated as success (used to tolerate a 404
// when removing a label that is not present); the response body is not decoded
// for those.
func (p *GitHubProvider) doStatus(ctx context.Context, method, endpoint string, body, out interface{}, allowStatus []int) error {
	resp, err := p.send(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	for _, code := range allowStatus {
		if resp.StatusCode == code {
			_ = resp.Body.Close()
			return nil
		}
	}
	return readJSONResponse(resp, method, endpoint, out)
}

// getAllPages issues GET requests against endpoint with per_page maximized,
// following the response Link header's rel="next" until the result set is
// exhausted, and invokes onPage with each page's raw JSON body. This is the
// shared paginator (#139): before it, every list/read site consumed only the
// first (default 30-item) page, so a claim breadcrumb, failing check, or
// changes-requested review beyond page 1 was silently invisible.
func (p *GitHubProvider) getAllPages(ctx context.Context, endpoint string, onPage func([]byte) error) error {
	next, err := withPerPage(endpoint, maxPerPage)
	if err != nil {
		return err
	}
	for next != "" {
		resp, err := p.send(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		body, nextLink, err := readPage(resp, http.MethodGet, next)
		if err != nil {
			return err
		}
		if err := onPage(body); err != nil {
			if errors.Is(err, errStopPaging) {
				return nil
			}
			return err
		}
		next = nextLink
	}
	return nil
}

// resolveToken returns the per-request token from the token source when configured,
// falling back to the statically injected token.
func (p *GitHubProvider) resolveToken(ctx context.Context) (string, error) {
	if p.tokenSource != nil {
		return p.tokenSource.Token(ctx)
	}
	return p.Token, nil
}

func (p *GitHubProvider) recordExternalRef(ctx context.Context, ref ExternalRef) {
	if p.recorder != nil {
		p.recorder.RecordExternalRef(ctx, ref)
	}
}

func (p *GitHubProvider) observeRateLimit(ctx context.Context, ev RateLimitEvent) {
	if p.rateObserver != nil {
		p.rateObserver.ObserveRateLimit(ctx, ev)
	}
}

type githubIssue struct {
	ID                       int64                           `json:"id"`
	Number                   int                             `json:"number"`
	Title                    string                          `json:"title"`
	Body                     string                          `json:"body"`
	State                    string                          `json:"state"`
	HTMLURL                  string                          `json:"html_url"`
	Labels                   []githubLabel                   `json:"labels"`
	Assignees                []githubUser                    `json:"assignees"`
	Milestone                *githubNode                     `json:"milestone"`
	CreatedAt                *time.Time                      `json:"created_at"`
	UpdatedAt                *time.Time                      `json:"updated_at"`
	IssueDependenciesSummary *githubIssueDependenciesSummary `json:"issue_dependencies_summary,omitempty"`
	// PullRequest is non-nil when this "issue" is actually a pull request.
	PullRequest *githubPullRequestLink `json:"pull_request"`
}

type githubIssueDependenciesSummary struct {
	TotalBlockedBy int `json:"total_blocked_by"`
}

// githubPullRequestLink marks an issues-endpoint entry as a pull request.
type githubPullRequestLink struct {
	URL string `json:"url"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubNode struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
}

type githubRef struct {
	Ref    string `json:"ref"`
	URL    string `json:"url"`
	Object struct {
		SHA string `json:"sha"`
		URL string `json:"url"`
	} `json:"object"`
}

type githubRepositoryActivity struct {
	Ref       string    `json:"ref"`
	Timestamp time.Time `json:"timestamp"`
}

type githubContentResponse struct {
	Commit struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	} `json:"commit"`
}

type githubPullRequest struct {
	ID      int    `json:"id"`
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Labels []githubLabel `json:"labels"`
}

type githubPullRequestDetail struct {
	ID             int64         `json:"id"`
	Number         int           `json:"number"`
	Title          string        `json:"title"`
	State          string        `json:"state"`
	Merged         bool          `json:"merged"`
	MergedAt       *time.Time    `json:"merged_at"`
	ClosedAt       *time.Time    `json:"closed_at"`
	Mergeable      *bool         `json:"mergeable"`
	MergeableState string        `json:"mergeable_state"`
	Draft          bool          `json:"draft"`
	Body           string        `json:"body"`
	HTMLURL        string        `json:"html_url"`
	Labels         []githubLabel `json:"labels"`
	UpdatedAt      time.Time     `json:"updated_at"`
	Head           struct {
		Ref  string            `json:"ref"`
		SHA  string            `json:"sha"`
		Repo *githubRepository `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

type githubRepository struct {
	Name    string     `json:"name"`
	HTMLURL string     `json:"html_url"`
	Owner   githubUser `json:"owner"`
}

type githubMergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// githubBranchRule is one entry in GET .../rules/branches/{branch}'s
// response array — every ruleset rule that actually applies to the branch.
// Only Type is read (DetectMergePolicy checks for "merge_queue"); the
// per-type Parameters shape varies by rule and is not modeled here.
type githubBranchRule struct {
	Type string `json:"type"`
}

type githubPullRequestFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// githubCompareResponse is the shape of GET .../compare/{base}...{head}.
// GitHub windows the top-level "files" array past a few hundred entries the
// same way it windows pulls/{n}/files, advertised via the same Link
// response header — CompareCommits follows it with getAllPages exactly
// like PullRequestFiles does, re-decoding this same struct per page (the
// mergeBaseCommit is identical on every page; only Files differs).
type githubCompareResponse struct {
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Files []githubPullRequestFile `json:"files"`
}

type githubReview struct {
	State string     `json:"state"`
	User  githubUser `json:"user"`
}

type githubCombinedStatus struct {
	State    string         `json:"state"`
	Statuses []githubStatus `json:"statuses"`
}

type githubStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
}

type githubCheckRunsResponse struct {
	CheckRuns []githubCheckRun `json:"check_runs"`
}

type githubCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Output     struct {
		Summary string `json:"summary"`
	} `json:"output"`
}

type githubIssueComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	User      githubUser `json:"user"`
	CreatedAt time.Time  `json:"created_at"`
}

func mapGitHubIssue(issue githubIssue) WorkItem {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.Name)
	}
	links := []Link{{Rel: "self", URL: issue.HTMLURL}}
	var parent *WorkItemRef
	hierarchy := map[string]interface{}{}
	if issue.Milestone != nil {
		parent = &WorkItemRef{Provider: ProviderGitHub, ID: strconv.Itoa(issue.Milestone.Number), URL: issue.Milestone.HTMLURL, Type: "milestone"}
		hierarchy["milestone"] = issue.Milestone
	}
	assignee := ""
	if len(issue.Assignees) > 0 {
		assignee = issue.Assignees[0].Login
	}
	blockedByCount := 0
	if issue.IssueDependenciesSummary != nil {
		blockedByCount = issue.IssueDependenciesSummary.TotalBlockedBy
	}
	return WorkItem{
		Provider:       ProviderGitHub,
		ID:             strconv.Itoa(issue.Number),
		ExternalID:     strconv.FormatInt(issue.ID, 10),
		Type:           "issue",
		Title:          issue.Title,
		Body:           issue.Body,
		Labels:         labels,
		State:          issue.State,
		Status:         statusFromLabels(labels, issue.State),
		Assignee:       assignee,
		Links:          links,
		Parent:         parent,
		Hierarchy:      hierarchy,
		URL:            issue.HTMLURL,
		CreatedAt:      issue.CreatedAt,
		UpdatedAt:      issue.UpdatedAt,
		BlockedByCount: blockedByCount,
		Raw:            issue,
	}
}
