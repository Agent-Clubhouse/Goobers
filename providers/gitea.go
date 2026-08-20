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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// ErrGiteaMergeQueueUnsupported is the typed sentinel EnqueuePullRequest and
// PollMergeQueueEntry wrap: Gitea has no merge queue, so those mandatory
// RepoProvider methods are unsupported by construction. Degradation is safe —
// DetectMergePolicy always reports MergePolicyDirect, so internal/mergepolicy's
// Land never routes to the enqueue lander; a direct caller gets a clear
// non-retryable error rather than a panic or an HTTP request.
var ErrGiteaMergeQueueUnsupported = errors.New("gitea: merge queue is not supported")

// GiteaProvider implements repo, backlog, and trigger operations for a
// self-hosted Gitea forge. Gitea's REST API is deliberately GitHub-v3
// compatible, so this reuses the shared unexported plumbing (joinURL, addQuery,
// getAllPages-style pagination, the decode structs, the rate-limit machinery)
// and defines gitea-specific structs only where Gitea diverges.
type GiteaProvider struct {
	// BaseURL is the API root (forge root + /api/v1); RootURL is the forge root
	// used to build git clone URLs.
	BaseURL string
	RootURL string
	Token   string
	Client  HTTPClient
	Runner  CommandRunner

	// initErr is a deferred constructor error (empty baseURL) surfaced on the
	// first call rather than panicking at construction, mirroring http.Client's
	// error-store pattern.
	initErr error

	// tokenSource, when set, resolves the token per request; otherwise Token is used.
	tokenSource TokenSource
	// recorder receives "external ref touched" facts for the run journal.
	recorder MutationRecorder
	// registrar receives credential forms that must be scrubbed from journals.
	registrar SecretRegistrar
	// rateLimitObserver receives rate-limit decisions. Mostly inert on Gitea
	// (no X-RateLimit headers) but wired for 429/Retry-After.
	rateLimitObserver RateLimitObserver

	// maxRetries bounds transport and server-error retries on a single request.
	maxRetries          int
	maxRateLimitRetries int
	// maxRateLimitWait bounds the total time one request spends sleeping on
	// rate-limit backoff before giving up.
	maxRateLimitWait time.Duration
	// now, sleep, jitter are injectable for deterministic rate-limit tests.
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// NewGiteaProvider constructs a Gitea provider. baseURL is the forge root (e.g.
// https://gitea.example.com); the constructor trims a trailing '/', preserves
// it as RootURL for git clone URLs, and derives BaseURL = RootURL + "/api/v1"
// (appended only when not already present). An empty baseURL is stored as a
// deferred error surfaced on first use.
func NewGiteaProvider(baseURL, token string, opts ...func(*GiteaProvider)) *GiteaProvider {
	p := &GiteaProvider{
		Token:               token,
		maxRetries:          defaultRateLimitRetries,
		maxRateLimitRetries: defaultRateLimitRetries,
		maxRateLimitWait:    defaultRateLimitMaxWait,
		now:                 time.Now,
		sleep:               contextSleep,
		jitter:              randomJitter,
	}
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		p.initErr = fmt.Errorf("gitea base URL is required")
	} else {
		p.RootURL = strings.TrimSuffix(trimmed, "/api/v1")
		if strings.HasSuffix(trimmed, "/api/v1") {
			p.BaseURL = trimmed
		} else {
			p.BaseURL = trimmed + "/api/v1"
		}
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
	if p.registrar != nil && p.Token != "" {
		p.registrar.Register([]byte(p.Token))
		p.registrar.Register([]byte(base64.StdEncoding.EncodeToString([]byte(p.Token + ":"))))
	}
	return p
}

// WithGiteaTokenSource resolves the access token per request from source
// instead of the statically injected token.
func WithGiteaTokenSource(source TokenSource) func(*GiteaProvider) {
	return func(p *GiteaProvider) { p.tokenSource = source }
}

// WithGiteaMutationRecorder records every provider-side mutation as an
// external-ref touched fact for the run journal.
func WithGiteaMutationRecorder(recorder MutationRecorder) func(*GiteaProvider) {
	return func(p *GiteaProvider) { p.recorder = recorder }
}

// WithGiteaRateLimitObserver receives rate-limit backoff signals for telemetry.
func WithGiteaRateLimitObserver(observer RateLimitObserver) func(*GiteaProvider) {
	return func(p *GiteaProvider) { p.rateLimitObserver = observer }
}

// WithGiteaSecretRegistrar registers the raw token and its git basic-auth form
// with the scrubber used by journals and telemetry.
func WithGiteaSecretRegistrar(registrar SecretRegistrar) func(*GiteaProvider) {
	return func(p *GiteaProvider) { p.registrar = registrar }
}

// WithGiteaHTTPClient overrides the HTTP client every provider request is sent
// through. A nil client is ignored so the constructor's default still applies.
func WithGiteaHTTPClient(client HTTPClient) func(*GiteaProvider) {
	return func(p *GiteaProvider) {
		if client != nil {
			p.Client = client
		}
	}
}

// WithGiteaMaxRateLimitRetries overrides how many times a rate-limited request
// is retried before the error is surfaced.
func WithGiteaMaxRateLimitRetries(n int) func(*GiteaProvider) {
	return func(p *GiteaProvider) { p.maxRateLimitRetries = n }
}

// WithGiteaMaxTransientRetries overrides how many times a request with a
// transport failure or 5xx response is retried before the error is surfaced.
func WithGiteaMaxTransientRetries(n int) func(*GiteaProvider) {
	return func(p *GiteaProvider) { p.maxRetries = n }
}

// WithGiteaRateLimitMaxWait overrides the total time one request may spend
// sleeping on rate-limit backoff before giving up.
func WithGiteaRateLimitMaxWait(d time.Duration) func(*GiteaProvider) {
	return func(p *GiteaProvider) { p.maxRateLimitWait = d }
}

// Kind returns the Gitea provider kind.
func (p *GiteaProvider) Kind() ProviderKind {
	return ProviderGitea
}

// Capabilities declares Gitea's current truth (design doc
// docs/design/provider-contract-conformance.md §3.2, CONF-1 #2074). Gitea
// is a community adapter (§2), not the blessed tier — it declares only
// what it genuinely implements: no native merge queue (EnqueuePullRequest/
// PollMergeQueueEntry return ErrGiteaMergeQueueUnsupported) and no repo-policy
// read. Review threads are declared (CapPRReviewThreads) but degrade: Gitea's
// REST review-comment payload does not expose thread resolution/outdatedness
// (see ListPullRequestReviewThreads in gitea_review_threads.go).
func (p *GiteaProvider) Capabilities() CapabilitySet {
	return mandatoryCapabilities().With(
		CapPRCompare,
		CapPRReviewSubmit,
		CapPRReviewThreads,
		CapPRMerge, CapPRLandingDetectPolicy,
		CapPRUpdateBranch, CapBranchDelete,
		CapPRStatusPublish,
		CapBacklogBlockers,
	)
}

// ready surfaces the deferred constructor error (empty baseURL).
func (p *GiteaProvider) ready() error {
	return p.initErr
}

func (p *GiteaProvider) cloneURL(repo RepositoryRef) string {
	if repo.URL != "" {
		return repo.URL
	}
	return fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(p.RootURL, "/"), repo.Owner, repo.Name)
}

// giteaGitAuthEnv returns a child-process-only Git environment that
// authenticates smart-HTTP git operations as the access token via an
// http.extraheader (Gitea accepts the token as the basic-auth username with an
// empty password). Mirrors githubGitAuthEnv.
func giteaGitAuthEnv(token string) []string {
	if token == "" {
		return os.Environ()
	}
	auth := base64.StdEncoding.EncodeToString([]byte(token + ":"))
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth,
	)
}

// GiteaGitAuthEnvironment resolves a Gitea token into a child-process-only Git
// environment that authenticates clone/fetch of remoteURL via a URL-scoped
// http.extraheader (the token as basic-auth username with empty password),
// hardened for reuse by the worktree layer exactly like GitHubGitAuthEnvironment:
// it strips inherited GIT_CONFIG_*/terminal-prompt vars, disables credential
// helpers, scopes the header to remoteURL, and registers the token and its
// base64 form with registrar for scrubbing. An empty token returns a hardened
// env with no auth header. The returned environment must never be persisted.
func GiteaGitAuthEnvironment(token, remoteURL string, registrar SecretRegistrar) []string {
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
	auth := base64.StdEncoding.EncodeToString([]byte(token + ":"))
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

// CloneRepository clones a Gitea repository to a local destination.
func (p *GiteaProvider) CloneRepository(ctx context.Context, req CloneRequest) (CloneResult, error) {
	if err := p.ready(); err != nil {
		return CloneResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return CloneResult{}, err
	}
	if req.Destination == "" {
		return CloneResult{}, fmt.Errorf("destination is required")
	}
	cloneURL := p.cloneURL(req.Repository)
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

// CreateBranch creates a branch ref in Gitea via git-shell: it resolves the
// base SHA (from req.BaseSHA, or `git ls-remote` of the base branch) and pushes
// it to refs/heads/<name> from a temporary bare repository.
func (p *GiteaProvider) CreateBranch(ctx context.Context, req BranchRequest) (BranchResult, error) {
	if err := p.ready(); err != nil {
		return BranchResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return BranchResult{}, err
	}
	if req.Name == "" {
		return BranchResult{}, fmt.Errorf("branch name is required")
	}
	runner, ok := p.Runner.(environmentCommandRunner)
	if !ok {
		return BranchResult{}, fmt.Errorf("gitea branch creation requires an environment-capable command runner")
	}
	token, err := p.resolveToken(ctx)
	if err != nil {
		return BranchResult{}, err
	}
	env := giteaGitAuthEnv(token)
	remoteURL := p.cloneURL(req.Repository)

	baseSHA := req.BaseSHA
	if baseSHA == "" {
		baseBranch := req.BaseBranch
		if baseBranch == "" {
			baseBranch = "main"
		}
		out, err := runner.RunWithEnv(ctx, env, "git", "ls-remote", remoteURL, "refs/heads/"+baseBranch)
		if err != nil {
			return BranchResult{}, fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(string(out)))
		}
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) == 0 {
			return BranchResult{}, fmt.Errorf("base branch %q not found on %s/%s", baseBranch, req.Repository.Owner, req.Repository.Name)
		}
		baseSHA = fields[0]
	}

	gitDir, err := os.MkdirTemp("", "goobers-gitea-branch-*")
	if err != nil {
		return BranchResult{}, fmt.Errorf("create temporary git directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(gitDir) }()
	if out, err := p.Runner.Run(ctx, "git", "init", "--bare", "--quiet", gitDir); err != nil {
		return BranchResult{}, fmt.Errorf("initialize temporary git directory: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := runner.RunWithEnv(ctx, env, "git", "--git-dir="+gitDir, "fetch", remoteURL, baseSHA); err != nil {
		return BranchResult{}, fmt.Errorf("git fetch base: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := runner.RunWithEnv(ctx, env, "git", "--git-dir="+gitDir, "push", remoteURL, baseSHA+":refs/heads/"+req.Name); err != nil {
		return BranchResult{}, fmt.Errorf("git push branch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, req.Name),
		Operation: "branch",
		Fields: map[string]FieldDigest{
			"sha": {After: digestString(baseSHA)},
		},
	})
	return BranchResult{Name: req.Name, SHA: baseSHA}, nil
}

// DeleteBranch removes a Gitea branch ref via git-shell. ExpectedSHA opts into
// a force-with-lease deletion (mirroring the GitHub lease semantics, including
// '(stale info)' -> GetBranch re-check -> BranchTipChangedError); without a
// lease it is a plain delete push.
func (p *GiteaProvider) DeleteBranch(ctx context.Context, req DeleteBranchRequest) (DeleteBranchResult, error) {
	if err := p.ready(); err != nil {
		return DeleteBranchResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return DeleteBranchResult{}, err
	}
	if req.Name == "" {
		return DeleteBranchResult{}, fmt.Errorf("branch name is required")
	}
	runner, ok := p.Runner.(environmentCommandRunner)
	if !ok {
		return DeleteBranchResult{}, fmt.Errorf("gitea branch deletion requires an environment-capable command runner")
	}
	token, err := p.resolveToken(ctx)
	if err != nil {
		return DeleteBranchResult{}, err
	}
	gitDir, err := os.MkdirTemp("", "goobers-gitea-delete-branch-*")
	if err != nil {
		return DeleteBranchResult{}, fmt.Errorf("create temporary git directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(gitDir) }()
	if out, err := p.Runner.Run(ctx, "git", "init", "--bare", "--quiet", gitDir); err != nil {
		return DeleteBranchResult{}, fmt.Errorf("initialize temporary git directory: %w: %s", err, strings.TrimSpace(string(out)))
	}
	remoteURL := p.cloneURL(req.Repository)
	ref := "refs/heads/" + req.Name
	args := []string{"--git-dir=" + gitDir, "push", "--porcelain"}
	if req.ExpectedSHA != "" {
		args = append(args, "--force-with-lease="+ref+":"+req.ExpectedSHA)
	}
	args = append(args, remoteURL, ":"+ref)

	out, err := runner.RunWithEnv(ctx, giteaGitAuthEnv(token), "git", args...)
	if err != nil {
		if req.ExpectedSHA != "" && strings.Contains(string(out), "(stale info)") {
			_, found, lookupErr := p.GetBranch(ctx, req.Repository, req.Name)
			if lookupErr != nil {
				return DeleteBranchResult{}, fmt.Errorf("resolve conditional branch deletion rejection: %w", lookupErr)
			}
			if !found {
				return DeleteBranchResult{}, nil
			}
			return DeleteBranchResult{}, &BranchTipChangedError{Name: req.Name, ExpectedSHA: req.ExpectedSHA}
		}
		return DeleteBranchResult{}, fmt.Errorf("delete branch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, req.Name),
		Operation: "delete",
	})
	return DeleteBranchResult{Deleted: true}, nil
}

// Commit writes file changes to a Gitea branch as one atomic git commit — a
// temp shallow clone, applied CommitFiles, a single commit, and a push (with
// --force-with-lease when BaseSHA is set). Being atomic, it also avoids the
// GitHub Contents-API per-file non-atomicity for this backend.
func (p *GiteaProvider) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	if err := p.ready(); err != nil {
		return CommitResult{}, err
	}
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
	runner, ok := p.Runner.(environmentCommandRunner)
	if !ok {
		return CommitResult{}, fmt.Errorf("gitea commit requires an environment-capable command runner")
	}
	token, err := p.resolveToken(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	env := giteaGitAuthEnv(token)
	remoteURL := p.cloneURL(req.Repository)

	workDir, err := os.MkdirTemp("", "goobers-gitea-commit-*")
	if err != nil {
		return CommitResult{}, fmt.Errorf("create temporary work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	if out, err := runner.RunWithEnv(ctx, env, "git", "clone", "--depth", "1", "--branch", req.Branch, remoteURL, workDir); err != nil {
		return CommitResult{}, fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, file := range req.Files {
		if file.Path == "" {
			return CommitResult{}, fmt.Errorf("file path is required")
		}
		abs := filepath.Join(workDir, filepath.FromSlash(file.Path))
		exists := false
		if _, statErr := os.Stat(abs); statErr == nil {
			exists = true
		}
		changeType, err := normalizeCommitChange(file.ChangeType, exists)
		if err != nil {
			return CommitResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		if changeType == CommitChangeDelete {
			if out, err := p.Runner.Run(ctx, "git", "-C", workDir, "rm", "--", file.Path); err != nil {
				return CommitResult{}, fmt.Errorf("git rm %s: %w: %s", file.Path, err, strings.TrimSpace(string(out)))
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return CommitResult{}, fmt.Errorf("create directory for %s: %w", file.Path, err)
		}
		if err := os.WriteFile(abs, []byte(file.Content), 0o644); err != nil {
			return CommitResult{}, fmt.Errorf("write %s: %w", file.Path, err)
		}
		if out, err := p.Runner.Run(ctx, "git", "-C", workDir, "add", "--", file.Path); err != nil {
			return CommitResult{}, fmt.Errorf("git add %s: %w: %s", file.Path, err, strings.TrimSpace(string(out)))
		}
	}
	commitArgs := []string{
		"-C", workDir,
		"-c", "user.name=goobers",
		"-c", "user.email=goobers@users.noreply.localhost",
		"commit", "-m", req.Message,
	}
	if out, err := p.Runner.Run(ctx, "git", commitArgs...); err != nil {
		return CommitResult{}, fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	pushArgs := []string{"-C", workDir, "push"}
	if req.BaseSHA != "" {
		pushArgs = append(pushArgs, "--force-with-lease="+req.Branch+":"+req.BaseSHA)
	}
	pushArgs = append(pushArgs, remoteURL, "HEAD:refs/heads/"+req.Branch)
	if out, err := runner.RunWithEnv(ctx, env, "git", pushArgs...); err != nil {
		return CommitResult{}, fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	shaOut, err := p.Runner.Run(ctx, "git", "-C", workDir, "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, fmt.Errorf("git rev-parse: %w: %s", err, strings.TrimSpace(string(shaOut)))
	}
	sha := strings.TrimSpace(string(shaOut))
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, sha),
		Operation: "commit",
		Fields: map[string]FieldDigest{
			"sha":   {After: digestString(sha)},
			"files": {After: digestString(strconv.Itoa(len(req.Files)))},
		},
	})
	return CommitResult{SHA: sha}, nil
}

// OpenPullRequest opens a Gitea pull request, idempotent on repass: a second
// call for the same head/base finds and PATCHes the existing PR rather than
// POSTing a duplicate. Gitea has no draft flag, so req.Draft maps to a "WIP: "
// title prefix.
func (p *GiteaProvider) OpenPullRequest(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if err := p.ready(); err != nil {
		return PullRequestResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestResult{}, err
	}
	title := req.Title
	if req.Draft {
		title = "WIP: " + title
	}
	prBody := withRunIDFooter(req.Body, req.RunID)
	if existing, ok, err := p.FindPullRequestByBranch(ctx, req.Repository, req.Head, req.Base); err != nil {
		return PullRequestResult{}, err
	} else if ok {
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", strconv.Itoa(existing.Number))
		if err != nil {
			return PullRequestResult{}, err
		}
		var out giteaPull
		if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"title": title, "body": prBody}, &out); err != nil {
			return PullRequestResult{}, err
		}
		p.recordExternalRef(ctx, ExternalRef{
			Provider:  ProviderGitea,
			Ref:       issueRef(req.Repository, strconv.Itoa(out.Number)),
			URL:       out.HTMLURL,
			Operation: "update",
			RunID:     req.RunID,
			Fields: map[string]FieldDigest{
				"title": {After: digestString(title)},
				"body":  {After: digestString(prBody)},
			},
		})
		return PullRequestResult{ID: strconv.Itoa(out.Number), Number: out.Number, URL: out.HTMLURL}, nil
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return PullRequestResult{}, err
	}
	body := map[string]interface{}{
		"title": title,
		"body":  prBody,
		"head":  req.Head,
		"base":  req.Base,
	}
	var out giteaPull
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, strconv.Itoa(out.Number)),
		URL:       out.HTMLURL,
		Operation: "open",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(title)},
			"body":  {After: digestString(prBody)},
		},
	})
	return PullRequestResult{ID: strconv.Itoa(out.Number), Number: out.Number, URL: out.HTMLURL}, nil
}

// FindPullRequestByBranch looks up an open PR for head/base, returning ok=false
// (not an error) if none exists. Gitea's pulls list has no head= query filter,
// so the head/base match is applied client-side.
func (p *GiteaProvider) FindPullRequestByBranch(ctx context.Context, repo RepositoryRef, head, base string) (PullRequestResult, bool, error) {
	if err := p.ready(); err != nil {
		return PullRequestResult{}, false, err
	}
	wantHead := stripOwnerPrefix(head)
	pulls, err := p.listOpenPulls(ctx, repo, base)
	if err != nil {
		return PullRequestResult{}, false, err
	}
	for _, pr := range pulls {
		if stripOwnerPrefix(pr.Head.Ref) != wantHead {
			continue
		}
		if base != "" && pr.Base.Ref != stripOwnerPrefix(base) {
			continue
		}
		return PullRequestResult{ID: strconv.Itoa(pr.Number), Number: pr.Number, URL: pr.HTMLURL}, true, nil
	}
	return PullRequestResult{}, false, nil
}

func stripOwnerPrefix(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	if idx := strings.Index(ref, ":"); idx >= 0 {
		return ref[idx+1:]
	}
	return ref
}

// listOpenPulls returns every open pull request on repo, optionally filtered
// server-side by base branch, following page/limit pagination.
func (p *GiteaProvider) listOpenPulls(ctx context.Context, repo RepositoryRef, base string) ([]giteaPull, error) {
	var all []giteaPull
	const limit = 50
	for page := 1; ; page++ {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls")
		if err != nil {
			return nil, err
		}
		values := url.Values{
			"state": []string{"open"},
			"page":  []string{strconv.Itoa(page)},
			"limit": []string{strconv.Itoa(limit)},
		}
		if base != "" {
			values.Set("base", stripOwnerPrefix(base))
		}
		endpoint, err = addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		var pageOut []giteaPull
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &pageOut); err != nil {
			return nil, err
		}
		all = append(all, pageOut...)
		if len(pageOut) < limit {
			break
		}
	}
	return all, nil
}

// RequestReview requests Gitea reviewers for a pull request.
func (p *GiteaProvider) RequestReview(ctx context.Context, req ReviewRequest) error {
	if err := p.ready(); err != nil {
		return err
	}
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
	if err := p.do(ctx, http.MethodPost, endpoint, map[string][]string{"reviewers": req.Reviewers}, nil); err != nil {
		return err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, req.PullID),
		Operation: "request-review",
		Fields: map[string]FieldDigest{
			"reviewers": {After: digestString(strings.Join(req.Reviewers, ","))},
		},
	})
	return nil
}

// PollPullRequest reports review decision, combined check state, mergeability
// signals, and comments-since for a Gitea pull request. Draft is derived from a
// "WIP:" title prefix; MergeableState is left empty (no Gitea equivalent). A
// read, so it emits no mutation event.
func (p *GiteaProvider) PollPullRequest(ctx context.Context, req PullRequestPollRequest) (PullRequestPollResult, error) {
	if err := p.ready(); err != nil {
		return PullRequestPollResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestPollResult{}, err
	}
	if req.PullID == "" {
		return PullRequestPollResult{}, fmt.Errorf("pull id is required")
	}
	pr, err := p.getPull(ctx, req.Repository, req.PullID)
	if err != nil {
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
	labels := giteaLabelNames(pr.Labels)
	return PullRequestPollResult{
		Number:           pr.Number,
		Title:            pr.Title,
		State:            pr.State,
		Merged:           pr.Merged,
		MergedAt:         pr.MergedAt,
		Mergeable:        pr.Mergeable,
		Draft:            isWIPTitle(pr.Title),
		Labels:           labels,
		HeadBranch:       pr.Head.Ref,
		HeadRepository:   giteaRepositoryRef(pr.Head.Repo),
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
		Integrity:        apiintegrity.Unapproved,
	}, nil
}

func isWIPTitle(title string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(title)), "WIP:")
}

func giteaRepositoryRef(repo *giteaRepository) *RepositoryRef {
	if repo == nil {
		return nil
	}
	return &RepositoryRef{
		Provider: ProviderGitea,
		Owner:    repo.Owner.Login,
		Name:     repo.Name,
		URL:      repo.HTMLURL,
	}
}

func (p *GiteaProvider) getPull(ctx context.Context, repo RepositoryRef, pullID string) (giteaPull, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID)
	if err != nil {
		return giteaPull{}, err
	}
	var pr giteaPull
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return giteaPull{}, err
	}
	return pr, nil
}

// PullRequestMergeable resolves pullID's current Gitea-computed mergeability.
func (p *GiteaProvider) PullRequestMergeable(ctx context.Context, repo RepositoryRef, pullID string) (*bool, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, fmt.Errorf("pull id is required")
	}
	pr, err := p.getPull(ctx, repo, pullID)
	if err != nil {
		return nil, err
	}
	return pr.Mergeable, nil
}

// reviewDecision aggregates a Gitea PR's reviews into a single decision: the
// latest review per reviewer wins, and any outstanding REQUEST_CHANGES beats
// any APPROVED.
func (p *GiteaProvider) reviewDecision(ctx context.Context, repo RepositoryRef, pullID string) (ReviewDecision, int, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "reviews")
	if err != nil {
		return "", 0, err
	}
	var reviews []giteaReview
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &reviews); err != nil {
		return "", 0, err
	}
	latest := map[string]string{}
	order := map[string]int{}
	for i, review := range reviews {
		login := review.User.Login
		state := strings.ToUpper(review.State)
		if state == "COMMENT" || state == "COMMENTED" || state == "PENDING" {
			continue
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
		case "REQUEST_CHANGES":
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

// combinedCheckState reads Gitea's combined commit status for ref and maps it
// to a normalized CheckState plus per-check detail. An empty statuses list is
// treated as passing, following the GitHub convention.
func (p *GiteaProvider) combinedCheckState(ctx context.Context, repo RepositoryRef, ref string) (CheckState, []CheckDetail, error) {
	if ref == "" {
		return CheckStatePending, nil, nil
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "status")
	if err != nil {
		return "", nil, err
	}
	var combined giteaCombinedStatus
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &combined); err != nil {
		return "", nil, err
	}
	details := make([]CheckDetail, 0, len(combined.Statuses))
	failing, pending := false, false
	for _, status := range combined.Statuses {
		state := giteaStatusToCheckState(status.Status)
		details = append(details, CheckDetail{
			Name: status.Context, State: state, Conclusion: status.Status,
			URL: status.TargetURL, Summary: status.Description,
		})
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
	case len(details) == 0:
		return CheckStatePassing, details, nil
	case pending:
		return CheckStatePending, details, nil
	default:
		return CheckStatePassing, details, nil
	}
}

func giteaStatusToCheckState(state string) CheckState {
	switch strings.ToLower(state) {
	case "success":
		return CheckStatePassing
	case "failure", "error":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
}

func (p *GiteaProvider) pullRequestComments(ctx context.Context, repo RepositoryRef, pullID string, since *time.Time) ([]PullRequestComment, error) {
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
			comments = append(comments, PullRequestComment{
				ID: c.ID, Author: c.User.Login, Body: c.Body, URL: c.HTMLURL,
				CreatedAt: c.CreatedAt, Integrity: apiintegrity.Unapproved,
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return comments, nil
}

// ClosePullRequest closes a Gitea pull request, detecting merged-vs-closed, and
// optionally leaves a comment.
func (p *GiteaProvider) ClosePullRequest(ctx context.Context, req ClosePullRequestRequest) (ClosePullRequestResult, error) {
	if err := p.ready(); err != nil {
		return ClosePullRequestResult{}, err
	}
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
	var out giteaPull
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
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       out.HTMLURL,
		Operation: operation,
		Fields:    fields,
	})
	return ClosePullRequestResult{Number: out.Number, Merged: out.Merged, State: state}, nil
}

// MergePullRequest merges a Gitea pull request via POST .../pulls/{index}/merge.
// Gitea spells the method field Do, the SHA guard head_commit_id (it 409s on
// mismatch), and returns 200 with an empty body — so MergeSHA is read from a
// follow-up pull GET.
func (p *GiteaProvider) MergePullRequest(ctx context.Context, req MergePullRequestRequest) (MergePullRequestResult, error) {
	if err := p.ready(); err != nil {
		return MergePullRequestResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return MergePullRequestResult{}, err
	}
	if req.PullID == "" {
		return MergePullRequestResult{}, fmt.Errorf("pull id is required")
	}
	if req.MergeMethod != "" && !req.MergeMethod.IsValid() {
		return MergePullRequestResult{}, fmt.Errorf("unsupported merge method %q", req.MergeMethod)
	}
	do := "merge"
	if req.MergeMethod != "" {
		do = string(req.MergeMethod)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "merge")
	if err != nil {
		return MergePullRequestResult{}, err
	}
	body := map[string]interface{}{"Do": do}
	if req.CommitTitle != "" {
		body["MergeTitleField"] = req.CommitTitle
	}
	if req.CommitMessage != "" {
		body["MergeMessageField"] = req.CommitMessage
	}
	if req.ExpectedHeadSHA != "" {
		body["head_commit_id"] = req.ExpectedHeadSHA
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, nil); err != nil {
		return MergePullRequestResult{}, err
	}
	number, convErr := strconv.Atoi(req.PullID)
	if convErr != nil {
		number = 0
	}
	mergeSHA := ""
	if pr, err := p.getPull(ctx, req.Repository, req.PullID); err == nil {
		mergeSHA = pr.MergeCommitSHA
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, req.PullID),
		Operation: "merge",
		Fields:    map[string]FieldDigest{"state": {After: digestString("merged")}},
	})
	return MergePullRequestResult{Number: number, Merged: true, MergeSHA: mergeSHA}, nil
}

// DetectMergePolicy always reports MergePolicyDirect with no HTTP call: Gitea
// has no merge queue, so direct is always the truthful detection. This is the
// keystone of graceful degradation — internal/mergepolicy.Land therefore never
// selects the enqueue lander.
func (p *GiteaProvider) DetectMergePolicy(ctx context.Context, req RepoMergePolicyRequest) (RepoMergePolicyResult, error) {
	if err := p.ready(); err != nil {
		return RepoMergePolicyResult{}, err
	}
	return RepoMergePolicyResult{Policy: MergePolicyDirect}, nil
}

// EnqueuePullRequest is unsupported on Gitea (no merge queue): it returns a
// typed ErrGiteaMergeQueueUnsupported wrap without panicking or issuing HTTP.
// Unreachable in practice via DetectMergePolicy=direct.
func (p *GiteaProvider) EnqueuePullRequest(ctx context.Context, req EnqueuePullRequestRequest) (EnqueuePullRequestResult, error) {
	return EnqueuePullRequestResult{}, fmt.Errorf("enqueue pull request %s: %w", req.PullID, ErrGiteaMergeQueueUnsupported)
}

// PollMergeQueueEntry is unsupported on Gitea (no merge queue): same typed
// ErrGiteaMergeQueueUnsupported wrap, no HTTP, no panic.
func (p *GiteaProvider) PollMergeQueueEntry(ctx context.Context, req PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error) {
	return PollMergeQueueEntryResult{}, fmt.Errorf("poll merge queue entry %s: %w", req.PullID, ErrGiteaMergeQueueUnsupported)
}

// ListPullRequests lists open pull requests targeting req.Base, filtered
// client-side to those whose head branch starts with req.HeadPrefix. Per
// candidate check state is resolved unless req.SkipCheckState is set.
func (p *GiteaProvider) ListPullRequests(ctx context.Context, req ListPullRequestsRequest) ([]PullRequestSummary, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	pulls, err := p.listOpenPulls(ctx, req.Repository, req.Base)
	if err != nil {
		return nil, err
	}
	out := make([]PullRequestSummary, 0, len(pulls))
	for _, pr := range pulls {
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
		out = append(out, summarizeGiteaPull(pr, checkState))
	}
	return out, nil
}

// summarizeGiteaPull maps a decoded Gitea pull request to the provider-neutral
// PullRequestSummary, pairing it with an already-resolved check state (empty
// when the caller skips check-state resolution). The Gitea analog of
// summarizePullRequest.
func summarizeGiteaPull(pr giteaPull, checkState CheckState) PullRequestSummary {
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
		MergeSHA:   pr.MergeCommitSHA,
		Draft:      isWIPTitle(pr.Title),
		Labels:     giteaLabelNames(pr.Labels),
		CheckState: checkState,
		UpdatedAt:  pr.UpdatedAt,
		Body:       pr.Body,
		Integrity:  apiintegrity.Unapproved,
	}
}

// GetPullRequest returns one pull request's current state and metadata without
// resolving reviews, comments, or check runs.
func (p *GiteaProvider) GetPullRequest(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestSummary, error) {
	if err := p.ready(); err != nil {
		return PullRequestSummary{}, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return PullRequestSummary{}, err
	}
	if pullID == "" {
		return PullRequestSummary{}, fmt.Errorf("pull id is required")
	}
	pr, err := p.getPull(ctx, repo, pullID)
	if err != nil {
		return PullRequestSummary{}, err
	}
	return summarizeGiteaPull(pr, ""), nil
}

// RefCheckState resolves a ref's combined commit-status state on demand.
func (p *GiteaProvider) RefCheckState(ctx context.Context, repo RepositoryRef, ref string) (CheckState, error) {
	state, _, err := p.combinedCheckState(ctx, repo, ref)
	return state, err
}

// RefCheckStates resolves combined check state for each requested ref.
func (p *GiteaProvider) RefCheckStates(ctx context.Context, repo RepositoryRef, refs []string) (map[string]CheckState, error) {
	states := make(map[string]CheckState, len(refs))
	for _, ref := range refs {
		state, err := p.RefCheckState(ctx, repo, ref)
		if err != nil {
			return nil, err
		}
		states[ref] = state
	}
	return states, nil
}

// ListRecentlyClosedPullRequests lists pull requests closed or merged since
// updatedSince. It is the bounded terminal-PR complement to ListPullRequests
// used when a workflow needs current state for recently relevant siblings.
// Gitea's pulls listing is paged (page/limit) and sorted most-recently-updated
// first (sort=recentupdate), so the scan stops at the first item updated before
// the window. Gitea has no Check Runs, so check state is never resolved here.
func (p *GiteaProvider) ListRecentlyClosedPullRequests(ctx context.Context, req ListPullRequestsRequest, updatedSince time.Time) ([]PullRequestSummary, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	if updatedSince.IsZero() {
		return nil, fmt.Errorf("updatedSince is required")
	}
	out := make([]PullRequestSummary, 0)
	const limit = 50
	for page := 1; ; page++ {
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
		if err != nil {
			return nil, err
		}
		values := url.Values{
			"state": []string{"closed"},
			"sort":  []string{"recentupdate"},
			"page":  []string{strconv.Itoa(page)},
			"limit": []string{strconv.Itoa(limit)},
		}
		if req.Base != "" {
			values.Set("base", stripOwnerPrefix(req.Base))
		}
		endpoint, err = addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		var pageOut []giteaPull
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &pageOut); err != nil {
			return nil, err
		}
		stop := false
		for _, pr := range pageOut {
			if pr.UpdatedAt.Before(updatedSince) {
				stop = true
				break
			}
			closedRecently := pr.ClosedAt != nil && !pr.ClosedAt.Before(updatedSince)
			mergedRecently := pr.MergedAt != nil && !pr.MergedAt.Before(updatedSince)
			if !closedRecently && !mergedRecently {
				continue
			}
			if req.HeadPrefix != "" && !strings.HasPrefix(pr.Head.Ref, req.HeadPrefix) {
				continue
			}
			out = append(out, summarizeGiteaPull(pr, ""))
		}
		if stop || len(pageOut) < limit {
			break
		}
	}
	return out, nil
}

// CIFailures returns the failing checks for ref as CI failure evidence.
//
// Documented degradation: Gitea exposes only the legacy commit-status model (no
// Check Runs), so there are no annotations to fetch — Annotations is always the
// empty (non-nil) slice, mirroring what GitHub's own legacy statuses produce.
// gather-ci-failures degrades to name/conclusion/URL/summary evidence only,
// which the brief schema fully permits.
func (p *GiteaProvider) CIFailures(ctx context.Context, repo RepositoryRef, ref string) ([]CIFailureDetail, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	_, checks, err := p.combinedCheckState(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	failures := make([]CIFailureDetail, 0)
	for _, check := range checks {
		if check.State != CheckStateFailing {
			continue
		}
		failures = append(failures, CIFailureDetail{
			CheckDetail: check,
			Annotations: []CheckAnnotation{},
			Integrity:   apiintegrity.Unapproved,
		})
	}
	return failures, nil
}

// BranchTipSHA resolves the current commit SHA at the tip of branch via a direct
// branch read. It uses p.do (not GetBranch), so a missing branch surfaces as an
// error rather than GetBranch's found=false swallow — matching GitHub, where the
// merge-escalated self-heal check needs a deleted base branch to fail loudly.
func (p *GiteaProvider) BranchTipSHA(ctx context.Context, repo RepositoryRef, branch string) (string, error) {
	if err := p.ready(); err != nil {
		return "", err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return "", err
	}
	if branch == "" {
		return "", fmt.Errorf("branch name is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "branches", branch)
	if err != nil {
		return "", err
	}
	var b giteaBranch
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &b); err != nil {
		return "", err
	}
	return b.Commit.ID, nil
}

// RepositoryFileContent returns one file's contents at ref. Gitea serves raw
// bytes from repos/{owner}/{repo}/raw/{path}?ref={ref} rather than GitHub's
// base64-in-JSON contents payload, so this reads the body directly instead of
// going through do/readJSONResponse. A read, so it emits no mutation event.
func (p *GiteaProvider) RepositoryFileContent(ctx context.Context, repo RepositoryRef, path, ref string) ([]byte, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("file path is required")
	}
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "raw", path)
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"ref": []string{ref}})
	if err != nil {
		return nil, err
	}
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	content, _, err := readPage(resp, http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// PullRequestFiles lists the files a pull request touches. Gitea returns no
// patch text, so ChangedFile.Patch stays empty (permitted by the field's
// contract). A read, so it emits no mutation event.
func (p *GiteaProvider) PullRequestFiles(ctx context.Context, repo RepositoryRef, pullID string) ([]ChangedFile, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
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
		out = append(out, ChangedFile{
			Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions,
			Integrity: apiintegrity.Unapproved,
		})
	}
	return out, nil
}

// CompareCommits reports base and head's common ancestor plus the file-level
// diff between them (issue #718). Gitea's REST compare returns commits but no
// per-file patches, and the verdict-cache re-keying needs MergeBaseSHA + file
// patches, so this uses a temp bare repo and git-shell (fetch, merge-base, diff
// -M --patch). A read, so it emits no mutation event.
func (p *GiteaProvider) CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error) {
	if err := p.ready(); err != nil {
		return CompareResult{}, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return CompareResult{}, err
	}
	if base == "" || head == "" {
		return CompareResult{}, fmt.Errorf("base and head are both required")
	}
	runner, ok := p.Runner.(environmentCommandRunner)
	if !ok {
		return CompareResult{}, fmt.Errorf("gitea commit comparison requires an environment-capable command runner")
	}
	token, err := p.resolveToken(ctx)
	if err != nil {
		return CompareResult{}, err
	}
	env := giteaGitAuthEnv(token)
	remoteURL := p.cloneURL(repo)

	gitDir, err := os.MkdirTemp("", "goobers-gitea-compare-*")
	if err != nil {
		return CompareResult{}, fmt.Errorf("create temporary git directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(gitDir) }()
	if out, err := p.Runner.Run(ctx, "git", "init", "--bare", "--quiet", gitDir); err != nil {
		return CompareResult{}, fmt.Errorf("initialize temporary git directory: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := runner.RunWithEnv(ctx, env, "git", "--git-dir="+gitDir, "fetch", remoteURL, base, head); err != nil {
		return CompareResult{}, fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mbOut, err := p.Runner.Run(ctx, "git", "--git-dir="+gitDir, "merge-base", base, head)
	if err != nil {
		return CompareResult{}, fmt.Errorf("git merge-base: %w: %s", err, strings.TrimSpace(string(mbOut)))
	}
	mergeBase := strings.TrimSpace(string(mbOut))
	diffOut, err := p.Runner.Run(ctx, "git", "--git-dir="+gitDir, "diff", "-M", "--patch", mergeBase, head)
	if err != nil {
		return CompareResult{}, fmt.Errorf("git diff: %w: %s", err, strings.TrimSpace(string(diffOut)))
	}
	return CompareResult{
		MergeBaseSHA: mergeBase,
		Files:        giteaParseDiff(string(diffOut)),
		Integrity:    apiintegrity.Unapproved,
	}, nil
}

// giteaParseDiff parses `git diff -M --patch` output into ChangedFiles. Each
// file block starts at a "diff --git a/… b/…" line; the Patch is the hunk text
// from the first "@@" onward, and additions/deletions are counted from the
// hunk +/- lines.
func giteaParseDiff(out string) []ChangedFile {
	lines := strings.Split(out, "\n")
	var files []ChangedFile
	var current *ChangedFile
	inHunk := false
	var patch strings.Builder
	flush := func() {
		if current == nil {
			return
		}
		current.Patch = strings.TrimRight(patch.String(), "\n")
		files = append(files, *current)
		current = nil
		patch.Reset()
		inHunk = false
	}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			path := parseDiffGitPath(line)
			current = &ChangedFile{Path: path, Status: "modified", Integrity: apiintegrity.Unapproved}
		case current == nil:
			continue
		case strings.HasPrefix(line, "new file mode"):
			current.Status = "added"
		case strings.HasPrefix(line, "deleted file mode"):
			current.Status = "removed"
		case strings.HasPrefix(line, "rename from "):
			current.Status = "renamed"
			current.PreviousPath = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			current.Status = "renamed"
			current.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "+++ b/"):
			current.Path = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "--- a/") && current.Status == "removed":
			current.PreviousPath = strings.TrimPrefix(line, "--- a/")
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			patch.WriteString(line)
			patch.WriteString("\n")
		case inHunk:
			patch.WriteString(line)
			patch.WriteString("\n")
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				current.Additions++
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				current.Deletions++
			}
		}
	}
	flush()
	return files
}

// parseDiffGitPath extracts the b-side path from a "diff --git a/X b/Y" header.
func parseDiffGitPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	idx := strings.Index(rest, " b/")
	if idx < 0 {
		return strings.TrimPrefix(rest, "a/")
	}
	return rest[idx+3:]
}

// SubmitPullRequestReview publishes a SHA-pinned native Gitea review.
func (p *GiteaProvider) SubmitPullRequestReview(ctx context.Context, req PullRequestReviewRequest) (PullRequestReviewResult, error) {
	if err := p.ready(); err != nil {
		return PullRequestReviewResult{}, err
	}
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
		event = "APPROVED"
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
		"event":     event,
		"body":      req.Body,
		"commit_id": req.CommitSHA,
	}
	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestReviewResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
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

// UpdateBranch incorporates a pull request's base branch into its head through
// Gitea's update endpoint. Gitea has no server-side expected_head_sha, so the
// lease is a client-side check-then-act: the current head must match
// req.ExpectedHeadSHA (mismatch -> *UpdateBranchError{StatusCode: 422}) before
// the update is issued. A small TOCTOU window remains; the caller re-polls (D6).
func (p *GiteaProvider) UpdateBranch(ctx context.Context, req UpdateBranchRequest) (UpdateBranchResult, error) {
	if err := p.ready(); err != nil {
		return UpdateBranchResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return UpdateBranchResult{}, err
	}
	if req.PullID == "" {
		return UpdateBranchResult{}, fmt.Errorf("pull id is required")
	}
	if req.ExpectedHeadSHA == "" {
		return UpdateBranchResult{}, fmt.Errorf("expected head SHA is required")
	}
	pr, err := p.getPull(ctx, req.Repository, req.PullID)
	if err != nil {
		return UpdateBranchResult{}, err
	}
	if pr.Head.SHA != req.ExpectedHeadSHA {
		return UpdateBranchResult{}, &UpdateBranchError{
			StatusCode: http.StatusUnprocessableEntity,
			Message:    fmt.Sprintf("pull request head %s does not match expected %s", pr.Head.SHA, req.ExpectedHeadSHA),
		}
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "update")
	if err != nil {
		return UpdateBranchResult{}, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"style": []string{"merge"}})
	if err != nil {
		return UpdateBranchResult{}, err
	}
	if err := p.do(ctx, http.MethodPost, endpoint, nil, nil); err != nil {
		return UpdateBranchResult{}, err
	}
	number, _ := strconv.Atoi(req.PullID)
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, req.PullID),
		URL:       pr.HTMLURL,
		Operation: "update-branch",
		Fields: map[string]FieldDigest{
			"headSha": {Before: digestString(req.ExpectedHeadSHA)},
		},
	})
	return UpdateBranchResult{Number: number, URL: pr.HTMLURL}, nil
}

// PublishPullRequestStatus posts a Gitea commit status a status-check branch
// policy can gate on (#772), resolving the head SHA first.
func (p *GiteaProvider) PublishPullRequestStatus(ctx context.Context, req PullRequestStatusRequest) (PullRequestStatusResult, error) {
	if err := p.ready(); err != nil {
		return PullRequestStatusResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestStatusResult{}, err
	}
	if req.PullID == "" {
		return PullRequestStatusResult{}, fmt.Errorf("pull id is required")
	}
	if req.Name == "" {
		return PullRequestStatusResult{}, fmt.Errorf("status name is required")
	}
	pr, err := p.getPull(ctx, req.Repository, req.PullID)
	if err != nil {
		return PullRequestStatusResult{}, err
	}
	if pr.Head.SHA == "" {
		return PullRequestStatusResult{}, fmt.Errorf("pull request %s has no head sha", req.PullID)
	}
	genre := req.Genre
	if genre == "" {
		genre = "goobers"
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "statuses", pr.Head.SHA)
	if err != nil {
		return PullRequestStatusResult{}, err
	}
	body := map[string]interface{}{
		"context": genre + "/" + req.Name,
		"state":   giteaStatusState(req.State),
	}
	if req.Description != "" {
		body["description"] = req.Description
	}
	if req.TargetURL != "" {
		body["target_url"] = req.TargetURL
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestStatusResult{}, err
	}
	return PullRequestStatusResult{ID: out.ID}, nil
}

func giteaStatusState(state CheckState) string {
	switch state {
	case CheckStatePassing:
		return "success"
	case CheckStateFailing:
		return "failure"
	default:
		return "pending"
	}
}

// ListBranches returns a bounded lexicographic page of remote branches matching
// req.Prefix, with client-side prefix/After filtering (Gitea has no
// matching-refs equivalent).
func (p *GiteaProvider) ListBranches(ctx context.Context, req ListBranchesRequest) ([]BranchSummary, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	if req.Prefix == "" {
		return nil, fmt.Errorf("branch prefix is required")
	}
	if req.Limit < 1 {
		return nil, fmt.Errorf("branch limit must be positive")
	}
	branches := make([]BranchSummary, 0, req.Limit)
	const limit = 50
	for page := 1; ; page++ {
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "branches")
		if err != nil {
			return nil, err
		}
		endpoint, err = addQuery(endpoint, url.Values{
			"page":  []string{strconv.Itoa(page)},
			"limit": []string{strconv.Itoa(limit)},
		})
		if err != nil {
			return nil, err
		}
		var pageOut []giteaBranch
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &pageOut); err != nil {
			return nil, err
		}
		for _, b := range pageOut {
			if !strings.HasPrefix(b.Name, req.Prefix) || (req.After != "" && b.Name <= req.After) {
				continue
			}
			branches = append(branches, BranchSummary{Name: b.Name, SHA: b.Commit.ID})
		}
		if len(pageOut) < limit {
			break
		}
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
	if len(branches) > req.Limit {
		branches = branches[:req.Limit]
	}
	return branches, nil
}

// GetBranch reads one exact branch ref. A missing ref is reported as
// found=false with a nil error; commit.timestamp fills LastActivityAt.
func (p *GiteaProvider) GetBranch(ctx context.Context, repo RepositoryRef, name string) (BranchSummary, bool, error) {
	if err := p.ready(); err != nil {
		return BranchSummary{}, false, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return BranchSummary{}, false, err
	}
	if name == "" {
		return BranchSummary{}, false, fmt.Errorf("branch name is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "branches", name)
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
	var b giteaBranch
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return BranchSummary{}, false, fmt.Errorf("decode branch: %w", err)
	}
	summary := BranchSummary{Name: b.Name, SHA: b.Commit.ID}
	if b.Commit.Timestamp != nil {
		summary.LastActivityAt = b.Commit.Timestamp
	}
	return summary, true, nil
}

// --- HTTP plumbing (standalone client, mirrors github.go's send/do/getAllPages) ---

func (p *GiteaProvider) do(ctx context.Context, method, endpoint string, body, out interface{}) error {
	resp, err := p.send(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	return readJSONResponse(resp, method, endpoint, out)
}

// doStatus performs a request treating any status in allowStatus as success
// (used to tolerate a 404 when removing a label that is not present).
func (p *GiteaProvider) doStatus(ctx context.Context, method, endpoint string, body, out interface{}, allowStatus []int) error {
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

// send issues one Gitea request, retrying transport errors and 5xx responses
// with a bounded budget and honoring 429 + Retry-After when a reverse proxy
// emits it. Gitea sends no X-RateLimit headers, so the rate-limit machinery is
// mostly inert. Every request carries `Authorization: token <token>`, Gitea's
// native scheme.
func (p *GiteaProvider) send(ctx context.Context, method, endpoint string, body interface{}) (*http.Response, error) {
	maxWait := p.maxRateLimitWait
	if maxWait <= 0 {
		maxWait = defaultRateLimitMaxWait
	}
	var rateLimitWaited time.Duration
	var rateLimitRetries, transientRetries int
	for {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		token, err := p.resolveToken(ctx)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "token "+token)
		}
		resp, err := httpClientOrDefault(p.Client).Do(req)
		if err != nil {
			if transientRetries < p.maxRetries {
				if serr := p.sleep(ctx, backoffDuration(transientRetries)); serr != nil {
					return nil, serr
				}
				transientRetries++
				continue
			}
			return nil, fmt.Errorf("send request: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait, ev := p.rateLimitPlan(resp, endpoint, rateLimitRetries)
			if rateLimitRetries >= p.maxRateLimitRetries || wait > maxWait-rateLimitWaited {
				ev.Outcome = RateLimitOutcomeExhausted
				p.observeRateLimit(ctx, ev)
				return resp, nil
			}
			_ = resp.Body.Close()
			if serr := p.sleep(ctx, wait); serr != nil {
				ev.Outcome = RateLimitOutcomeCanceled
				p.observeRateLimit(ctx, ev)
				return nil, serr
			}
			ev.Outcome = RateLimitOutcomeRetry
			p.observeRateLimit(ctx, ev)
			rateLimitWaited += wait
			rateLimitRetries++
			continue
		}
		if resp.StatusCode >= 500 && transientRetries < p.maxRetries {
			_ = resp.Body.Close()
			if serr := p.sleep(ctx, backoffDuration(transientRetries)); serr != nil {
				return nil, serr
			}
			transientRetries++
			continue
		}
		return resp, nil
	}
}

// getAllPages follows the Link header's rel="next" until exhausted, invoking
// onPage with each page's raw JSON body. Gitea emits Link headers exactly like
// GitHub.
func (p *GiteaProvider) getAllPages(ctx context.Context, endpoint string, onPage func([]byte) error) error {
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

func (p *GiteaProvider) rateLimitPlan(resp *http.Response, endpoint string, attempt int) (time.Duration, RateLimitEvent) {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	serverDelay, directed := retryAfterDelay(raw, p.now())
	wait := serverDelay
	if !directed || serverDelay <= 0 {
		wait = fallbackBackoff(attempt, p.jitter)
	} else {
		wait += rateLimitResetSlack
	}
	return wait, RateLimitEvent{
		Provider:      ProviderGitea,
		Scope:         rateLimitScope(endpoint),
		Delay:         wait,
		Endpoint:      endpoint,
		Status:        resp.StatusCode,
		RetryAfter:    serverDelay,
		RetryAfterRaw: raw,
		Attempt:       attempt,
	}
}

func (p *GiteaProvider) resolveToken(ctx context.Context) (string, error) {
	if p.tokenSource != nil {
		return p.tokenSource.Token(ctx)
	}
	return p.Token, nil
}

func (p *GiteaProvider) recordExternalRef(ctx context.Context, ref ExternalRef) {
	if p.recorder != nil {
		p.recorder.RecordExternalRef(ctx, ref)
	}
}

func (p *GiteaProvider) observeRateLimit(ctx context.Context, ev RateLimitEvent) {
	if p.rateLimitObserver != nil {
		p.rateLimitObserver.ObserveRateLimit(ctx, ev)
	}
}

// --- Gitea-specific decode structs (defined only where Gitea diverges) ---

type giteaPull struct {
	Number         int           `json:"number"`
	Title          string        `json:"title"`
	Body           string        `json:"body"`
	State          string        `json:"state"`
	HTMLURL        string        `json:"html_url"`
	Merged         bool          `json:"merged"`
	MergedAt       *time.Time    `json:"merged_at"`
	ClosedAt       *time.Time    `json:"closed_at"`
	Mergeable      *bool         `json:"mergeable"`
	MergeCommitSHA string        `json:"merge_commit_sha"`
	Labels         []giteaLabel  `json:"labels"`
	UpdatedAt      time.Time     `json:"updated_at"`
	Head           giteaPRBranch `json:"head"`
	Base           giteaPRBranch `json:"base"`
}

type giteaPRBranch struct {
	Ref  string           `json:"ref"`
	SHA  string           `json:"sha"`
	Repo *giteaRepository `json:"repo"`
}

type giteaRepository struct {
	Name    string     `json:"name"`
	HTMLURL string     `json:"html_url"`
	Owner   githubUser `json:"owner"`
}

type giteaLabel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
}

func giteaLabelNames(labels []giteaLabel) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names
}

type giteaReview struct {
	State string     `json:"state"`
	User  githubUser `json:"user"`
}

type giteaCombinedStatus struct {
	State    string        `json:"state"`
	Statuses []giteaStatus `json:"statuses"`
}

type giteaStatus struct {
	Context     string `json:"context"`
	Status      string `json:"status"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
}

type giteaBranch struct {
	Name   string `json:"name"`
	Commit struct {
		ID        string     `json:"id"`
		Timestamp *time.Time `json:"timestamp"`
	} `json:"commit"`
}
