package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	adoPullRequestPageSize = 100

	// adoZeroObjectID is the all-zero SHA Azure DevOps' Git Refs API
	// recognizes as "no object": oldObjectId when creating a ref from
	// nothing (CreateBranch) and newObjectId to delete one (DeleteBranch).
	adoZeroObjectID = "0000000000000000000000000000000000000000"
)

// ADOProvider implements repo, backlog, and trigger operations for Azure DevOps.
type ADOProvider struct {
	Organization string
	Project      string
	BaseURL      string
	Token        string
	Username     string
	Client       HTTPClient
	Runner       CommandRunner

	credentialSource ADOCredentialSource
	secretRegistrar  SecretRegistrar
	rateObserver     RateLimitObserver
	quotaObserver    QuotaObserver
	maxRetries       int
	maxRateLimitWait time.Duration
	now              func() time.Time
	sleep            func(context.Context, time.Duration) error
	jitter           func(time.Duration) time.Duration

	stateMu         sync.RWMutex
	stateCategories map[string][]adoWorkItemState
}

// NewADOProvider constructs an Azure DevOps provider with optional overrides.
func NewADOProvider(organization, project, token string, opts ...func(*ADOProvider)) *ADOProvider {
	p := &ADOProvider{
		Organization:     organization,
		Project:          project,
		BaseURL:          "https://dev.azure.com",
		Token:            token,
		Username:         "goobers",
		maxRetries:       defaultRateLimitRetries,
		maxRateLimitWait: defaultRateLimitMaxWait,
		now:              time.Now,
		sleep:            contextSleep,
		jitter:           randomJitter,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.credentialSource == nil && p.Token != "" {
		p.credentialSource = NewADOPATCredentialSource(p.Username, p.Token)
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
	if p.secretRegistrar != nil && p.Token != "" {
		p.secretRegistrar.Register([]byte(p.Token))
		p.secretRegistrar.Register([]byte(strings.TrimPrefix(basicAuth(p.Username, p.Token), "Basic ")))
	}
	return p
}

// WithADOCredentialSource configures PAT or Entra authentication independently
// of the legacy fixed-token constructor argument. An explicit source wins.
func WithADOCredentialSource(source ADOCredentialSource) func(*ADOProvider) {
	return func(p *ADOProvider) { p.credentialSource = source }
}

// SecretRegistrar receives provider credential forms that must be scrubbed.
type SecretRegistrar interface {
	Register(secret []byte)
}

// WithADOSecretRegistrar registers the raw token and encoded Basic credential
// with the scrubber used by journals and telemetry.
func WithADOSecretRegistrar(registrar SecretRegistrar) func(*ADOProvider) {
	return func(p *ADOProvider) { p.secretRegistrar = registrar }
}

// WithADORateLimitObserver receives Azure DevOps rate-limit decisions.
func WithADORateLimitObserver(observer RateLimitObserver) func(*ADOProvider) {
	return func(p *ADOProvider) { p.rateObserver = observer }
}

// WithADOQuotaObserver receives quota-window observations from ADO responses.
func WithADOQuotaObserver(observer QuotaObserver) func(*ADOProvider) {
	return func(p *ADOProvider) { p.quotaObserver = observer }
}

// WithADOMaxRateLimitRetries overrides the retry count for rate-limited requests.
func WithADOMaxRateLimitRetries(n int) func(*ADOProvider) {
	return func(p *ADOProvider) { p.maxRetries = n }
}

// Kind returns the Azure DevOps provider kind.
func (p *ADOProvider) Kind() ProviderKind {
	return ProviderADO
}

// Capabilities declares ADO's current truth (design doc
// docs/design/provider-contract-conformance.md §3.2/§4, CONF-3 #2076): the
// landing set — pr.merge, pr.landing.detect-policy, pr.landing.enqueue,
// pr.landing.poll, pr.compare, branch.delete — is now conformant, mapped
// onto the mergepolicy seam (#758) per §4's table. pr.review.submit/
// threads and repo.policy.read remain excluded (no ADO implementation
// exists). pr.status.publish is real (PublishPullRequestStatus).
// backlog.blockers is excluded (CONF-5 #2078, closing #2059): ADO
// dependency-link modeling reaches parity in V1, so there is no real
// native-dependency read to declare — Dispatcher returns ErrUnsupported
// for this capability instead of the deleted fail-open stub.
func (p *ADOProvider) Capabilities() CapabilitySet {
	return mandatoryCapabilities().With(
		CapPRQueryAuthor, CapPRQueryRequestedReviewer,
		CapPRStatusPublish,
		CapPRMerge, CapPRLandingDetectPolicy, CapPRLandingEnqueue, CapPRLandingPoll,
		CapPRCompare, CapBranchDelete,
	)
}

// CloneRepository clones an Azure DevOps repository to a local destination.
func (p *ADOProvider) CloneRepository(ctx context.Context, req CloneRequest) (CloneResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return CloneResult{}, err
	}
	if req.Destination == "" {
		return CloneResult{}, fmt.Errorf("destination is required")
	}
	cloneURL := p.repositoryURL(req.Repository)
	args := []string{"clone"}
	if req.Branch != "" {
		args = append(args, "--branch", req.Branch)
	}
	args = append(args, cloneURL, req.Destination)
	var (
		out []byte
		err error
	)
	if p.credentialSource == nil {
		out, err = p.Runner.Run(ctx, "git", args...)
	} else {
		runner, ok := p.Runner.(environmentCommandRunner)
		if !ok {
			return CloneResult{}, fmt.Errorf("authenticated ADO clone requires an environment-capable command runner")
		}
		header, authErr := p.authorizationHeader(ctx)
		if authErr != nil {
			return CloneResult{}, fmt.Errorf("resolve ADO clone credential: %w", authErr)
		}
		out, err = runner.RunWithEnv(ctx, adoGitAuthEnv(header, cloneURL), "git", args...)
	}
	if err != nil {
		return CloneResult{}, fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return CloneResult{Path: req.Destination, URL: cloneURL}, nil
}

// RepositoryReachable verifies authenticated Git access without cloning.
func (p *ADOProvider) RepositoryReachable(ctx context.Context, repo RepositoryRef) error {
	if err := requireRepo(repo); err != nil {
		return err
	}
	args := []string{
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"ls-remote", "--heads", p.repositoryURL(repo),
	}
	if p.credentialSource == nil {
		if _, err := p.Runner.Run(ctx, "git", args...); err != nil {
			return fmt.Errorf("git ls-remote: %w", err)
		}
		return nil
	}
	runner, ok := p.Runner.(environmentCommandRunner)
	if !ok {
		return fmt.Errorf("authenticated ADO repository preflight requires an environment-capable command runner")
	}
	header, err := p.authorizationHeader(ctx)
	if err != nil {
		return fmt.Errorf("resolve ADO repository credential: %w", err)
	}
	if _, err := runner.RunWithEnv(ctx, adoGitAuthEnv(header, p.repositoryURL(repo)), "git", args...); err != nil {
		return fmt.Errorf("git ls-remote: %w", err)
	}
	return nil
}

func (p *ADOProvider) repositoryURL(repo RepositoryRef) string {
	if repo.URL != "" {
		return repo.URL
	}
	return fmt.Sprintf("%s/%s/%s/_git/%s", strings.TrimRight(p.BaseURL, "/"), url.PathEscape(p.Organization), url.PathEscape(p.project(repo)), url.PathEscape(repo.Name))
}

// CreateBranch creates an Azure DevOps branch ref.
func (p *ADOProvider) CreateBranch(ctx context.Context, req BranchRequest) (BranchResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return BranchResult{}, err
	}
	if req.Name == "" {
		return BranchResult{}, fmt.Errorf("branch name is required")
	}
	if req.BaseSHA == "" {
		return BranchResult{}, fmt.Errorf("base sha is required for ado branch creation")
	}
	endpoint, err := p.repoURL(req.Repository, "refs")
	if err != nil {
		return BranchResult{}, err
	}
	body := []map[string]string{{
		"name":        "refs/heads/" + req.Name,
		"oldObjectId": adoZeroObjectID,
		"newObjectId": req.BaseSHA,
	}}
	var out adoRefsResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return BranchResult{}, err
	}
	if len(out.Value) == 0 {
		return BranchResult{}, fmt.Errorf("ado branch creation returned no refs")
	}
	return BranchResult{Name: strings.TrimPrefix(out.Value[0].Name, "refs/heads/"), SHA: out.Value[0].ObjectID, URL: out.Value[0].URL}, nil
}

// DeleteBranch removes an Azure DevOps branch ref (CONF-3 #2076, design doc
// §4: branch.delete ≙ ref update to zeroed object id) via the same refs
// batch-update endpoint CreateBranch uses, reversed: oldObjectId pins the
// SHA being removed — an optimistic-concurrency guard exactly like GitHub's
// ExpectedSHA lease — and newObjectId is adoZeroObjectID, which ADO's Git
// Refs API treats as "delete this ref". When req.ExpectedSHA is empty, the
// current tip is resolved first: ADO's ref-update rejects an update whose
// oldObjectId doesn't match the ref's live value, so even an unconditional
// delete needs a real one.
func (p *ADOProvider) DeleteBranch(ctx context.Context, req DeleteBranchRequest) (DeleteBranchResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return DeleteBranchResult{}, err
	}
	if req.Name == "" {
		return DeleteBranchResult{}, fmt.Errorf("branch name is required")
	}
	expected := req.ExpectedSHA
	if expected == "" {
		sha, found, err := p.lookupBranchSHA(ctx, req.Repository, req.Name)
		if err != nil {
			return DeleteBranchResult{}, err
		}
		if !found {
			return DeleteBranchResult{Deleted: false}, nil
		}
		expected = sha
	}
	endpoint, err := p.repoURL(req.Repository, "refs")
	if err != nil {
		return DeleteBranchResult{}, err
	}
	body := []map[string]string{{
		"name":        "refs/heads/" + req.Name,
		"oldObjectId": expected,
		"newObjectId": adoZeroObjectID,
	}}
	var out adoRefsResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return DeleteBranchResult{}, err
	}
	if len(out.Value) == 0 {
		// The batch update reported no ref touched: with an explicit
		// ExpectedSHA the caller cared about the lease, so treat an
		// unconfirmed delete as a lost lease (BranchTipChangedError) rather
		// than silently reporting success; without one, absence is
		// ambiguous-but-benign (already gone), matching GitHub's 404-is-
		// not-an-error DeleteBranch semantics.
		if req.ExpectedSHA != "" {
			return DeleteBranchResult{}, &BranchTipChangedError{Name: req.Name, ExpectedSHA: req.ExpectedSHA}
		}
		return DeleteBranchResult{Deleted: false}, nil
	}
	return DeleteBranchResult{Deleted: true}, nil
}

// Commit writes file changes to an Azure DevOps branch.
func (p *ADOProvider) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return CommitResult{}, err
	}
	if err := validateCommitRequest(req); err != nil {
		return CommitResult{}, err
	}
	changes := make([]adoChange, 0, len(req.Files))
	for _, file := range req.Files {
		if err := validateCommitFile(file); err != nil {
			return CommitResult{}, err
		}
		exists, err := p.pathExists(ctx, req.Repository, req.Branch, file.Path)
		if err != nil {
			return CommitResult{}, err
		}
		changeType, err := normalizeCommitChange(file.ChangeType, exists)
		if err != nil {
			return CommitResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		change := adoChange{
			ChangeType: string(changeType),
			Item:       map[string]string{"path": "/" + strings.TrimPrefix(file.Path, "/")},
		}
		if changeType != CommitChangeDelete {
			change.NewContent = &adoNewContent{Content: file.Content, ContentType: "rawtext"}
		}
		changes = append(changes, change)
	}
	endpoint, err := p.repoURL(req.Repository, "pushes")
	if err != nil {
		return CommitResult{}, err
	}
	oldObjectID := req.BaseSHA
	if oldObjectID == "" {
		var err error
		oldObjectID, err = p.branchSHA(ctx, req.Repository, req.Branch)
		if err != nil {
			return CommitResult{}, err
		}
	}
	body := adoPushRequest{
		RefUpdates: []adoRefUpdate{{Name: "refs/heads/" + req.Branch, OldObjectID: oldObjectID}},
		Commits:    []adoCommit{{Comment: req.Message, Changes: changes}},
	}
	var out adoPushResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return CommitResult{}, err
	}
	commitID := ""
	if len(out.Commits) > 0 {
		commitID = out.Commits[0].CommitID
	}
	return CommitResult{SHA: commitID, URL: out.URL}, nil
}

func (p *ADOProvider) pathExists(ctx context.Context, repo RepositoryRef, branch, path string) (bool, error) {
	endpoint, err := p.repoURL(repo, "items")
	if err != nil {
		return false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"path":                          []string{"/" + strings.TrimPrefix(path, "/")},
		"versionDescriptor.versionType": []string{"branch"},
		"versionDescriptor.version":     []string{branch},
		"includeContentMetadata":        []string{"false"},
	})
	if err != nil {
		return false, err
	}
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("GET %s failed: status %d", endpoint, resp.StatusCode)
	}
	return true, nil
}

func (p *ADOProvider) branchSHA(ctx context.Context, repo RepositoryRef, branch string) (string, error) {
	sha, found, err := p.lookupBranchSHA(ctx, repo, branch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("ado branch %q not found", branch)
	}
	return sha, nil
}

// lookupBranchSHA is branchSHA's found-or-not-found counterpart (CONF-3
// #2076): DeleteBranch needs to tell "ref absent" apart from "lookup
// failed" without branchSHA's caller-facing error-on-absent behavior.
func (p *ADOProvider) lookupBranchSHA(ctx context.Context, repo RepositoryRef, branch string) (string, bool, error) {
	endpoint, err := p.repoURL(repo, "refs")
	if err != nil {
		return "", false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"filter": []string{"heads/" + strings.TrimPrefix(branch, "refs/heads/")}})
	if err != nil {
		return "", false, err
	}
	var out adoRefsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return "", false, err
	}
	if len(out.Value) == 0 || out.Value[0].ObjectID == "" {
		return "", false, nil
	}
	return out.Value[0].ObjectID, true, nil
}

func (p *ADOProvider) repoURL(repo RepositoryRef, elems ...string) (string, error) {
	return p.repoURLVersion(repo, "7.1", elems...)
}

// repoURLVersion builds a git-repository-scoped ADO endpoint with an explicit
// api-version, mirroring workURL/workURLVersion. The PR-labels endpoints
// (AddPullRequestLabels/RemovePullRequestLabel) are published only under the
// "7.1-preview.1" version and reject a plain "7.1", so they cannot use the
// 7.1-pinned repoURL.
func (p *ADOProvider) repoURLVersion(repo RepositoryRef, version string, elems ...string) (string, error) {
	repoID := repo.ID
	if repoID == "" {
		repoID = repo.Name
	}
	parts := []string{p.Organization, p.project(repo), "_apis", "git", "repositories", repoID}
	parts = append(parts, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	return addQuery(endpoint, url.Values{"api-version": []string{version}})
}

func (p *ADOProvider) workURL(project string, elems ...string) (string, error) {
	return p.workURLVersion(project, "7.1", elems...)
}

func (p *ADOProvider) workURLVersion(project, version string, elems ...string) (string, error) {
	parts := []string{p.Organization, project, "_apis", "wit"}
	parts = append(parts, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	return addQuery(endpoint, url.Values{"api-version": []string{version}})
}

func (p *ADOProvider) project(repo RepositoryRef) string {
	if repo.Project != "" {
		return repo.Project
	}
	return p.Project
}

func (p *ADOProvider) do(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	resp, err := p.send(ctx, method, endpoint, body, "")
	if err != nil {
		return err
	}
	return readJSONResponse(resp, method, endpoint, out)
}

func (p *ADOProvider) doPatch(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	resp, err := p.send(ctx, method, endpoint, body, "application/json-patch+json")
	if err != nil {
		return err
	}
	return readJSONResponse(resp, method, endpoint, out)
}

func (p *ADOProvider) send(ctx context.Context, method, endpoint string, body interface{}, contentType string) (*http.Response, error) {
	maxWait := p.maxRateLimitWait
	if maxWait <= 0 {
		maxWait = defaultRateLimitMaxWait
	}
	var waited time.Duration
	rateAttempt := 0
	transientAttempt := 0
	authRetried := false
	for {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		header, err := p.authorizationHeader(ctx)
		if err != nil {
			return nil, err
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := httpClientOrDefault(p.Client).Do(req)
		if err != nil {
			// A transport failure (connection reset, DNS blip, timeout) is only
			// safe to retry automatically for an idempotent method (#2026): a
			// POST/PATCH may have already committed server-side before its
			// response was lost, and ADO has no transport-level dedup marker
			// (unlike GitHub issue creation's footer check, #140) to make a
			// blind retry safe for those.
			if isIdempotentHTTPMethod(method) && transientAttempt < p.maxRetries {
				if serr := p.sleep(ctx, backoffDuration(transientAttempt)); serr != nil {
					return nil, serr
				}
				transientAttempt++
				continue
			}
			return nil, fmt.Errorf("send request: %w", err)
		}
		p.observeQuota(ctx, resp)
		if resp.StatusCode == http.StatusUnauthorized && !authRetried && p.invalidateCredential() {
			_ = resp.Body.Close()
			authRetried = true
			continue
		}
		if resp.StatusCode >= 500 && isIdempotentHTTPMethod(method) && transientAttempt < p.maxRetries {
			_ = resp.Body.Close()
			if err := p.sleep(ctx, backoffDuration(transientAttempt)); err != nil {
				return nil, err
			}
			transientAttempt++
			continue
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		wait, ev := p.rateLimitPlan(resp, endpoint, rateAttempt)
		if rateAttempt >= p.maxRetries || wait > maxWait-waited {
			ev.Outcome = RateLimitOutcomeExhausted
			p.observeRateLimit(ctx, ev)
			return resp, nil
		}
		_ = resp.Body.Close()
		if err := p.sleep(ctx, wait); err != nil {
			ev.Outcome = RateLimitOutcomeCanceled
			p.observeRateLimit(ctx, ev)
			return nil, err
		}
		ev.Outcome = RateLimitOutcomeRetry
		p.observeRateLimit(ctx, ev)
		waited += wait
		rateAttempt++
	}
}

func (p *ADOProvider) authorizationHeader(ctx context.Context) (string, error) {
	if p.credentialSource == nil {
		return "", nil
	}
	credential, err := p.credentialSource.Credential(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve ADO credential: %w", err)
	}
	header, err := credential.authorizationHeader()
	if err != nil {
		return "", err
	}
	if p.secretRegistrar != nil {
		p.secretRegistrar.Register([]byte(credential.Secret))
		p.secretRegistrar.Register([]byte(strings.TrimSpace(strings.TrimPrefix(header, "Basic "))))
	}
	return header, nil
}

func (p *ADOProvider) invalidateCredential() bool {
	source, ok := p.credentialSource.(refreshableADOCredentialSource)
	if !ok {
		return false
	}
	source.Invalidate()
	return true
}

func (p *ADOProvider) rateLimitPlan(resp *http.Response, endpoint string, attempt int) (time.Duration, RateLimitEvent) {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	serverDelay, directed := retryAfterDelay(raw, p.now())
	wait := serverDelay
	if !directed || serverDelay <= 0 {
		wait = fallbackBackoff(attempt, p.jitter)
	} else {
		wait += rateLimitResetSlack
	}
	return wait, RateLimitEvent{
		Provider:      ProviderADO,
		Scope:         rateLimitScope(endpoint),
		Delay:         wait,
		Endpoint:      endpoint,
		Status:        resp.StatusCode,
		RetryAfter:    serverDelay,
		RetryAfterRaw: raw,
		Attempt:       attempt,
	}
}

func (p *ADOProvider) observeRateLimit(ctx context.Context, ev RateLimitEvent) {
	if p.rateObserver != nil {
		p.rateObserver.ObserveRateLimit(ctx, ev)
	}
}

func (p *ADOProvider) observeQuota(ctx context.Context, resp *http.Response) {
	if p.quotaObserver == nil {
		return
	}
	observation := QuotaObservation{Provider: ProviderADO}
	remaining, remainingErr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")))
	resetSeconds, resetErr := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")), 10, 64)
	if remainingErr == nil && resetErr == nil && remaining >= 0 && resetSeconds > 0 {
		observation.Remaining = remaining
		// X-RateLimit-Reset is Unix epoch time (Microsoft's rate-limits docs:
		// "Time when, if all resource consumption stops immediately, tracked
		// usage returns to 0 TSTUs. Expressed in Unix epoch time."), not a
		// duration in seconds from now — unlike Retry-After.
		observation.Reset = time.Unix(resetSeconds, 0).UTC()
		observation.Known = true
	} else if resp.StatusCode == http.StatusTooManyRequests {
		now := p.now()
		if delay, directed := retryAfterDelay(strings.TrimSpace(resp.Header.Get("Retry-After")), now); directed && delay > 0 {
			observation.Reset = now.Add(delay)
			observation.Known = true
		}
	}
	p.quotaObserver.ObserveQuota(ctx, observation)
}

type adoPatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

type adoRefsResponse struct {
	Value []struct {
		Name     string `json:"name"`
		ObjectID string `json:"objectId"`
		URL      string `json:"url"`
	} `json:"value"`
}

type adoRefUpdate struct {
	Name        string `json:"name"`
	OldObjectID string `json:"oldObjectId"`
}

type adoPushRequest struct {
	RefUpdates []adoRefUpdate `json:"refUpdates"`
	Commits    []adoCommit    `json:"commits"`
}

type adoCommit struct {
	Comment string      `json:"comment"`
	Changes []adoChange `json:"changes"`
}

type adoChange struct {
	ChangeType string            `json:"changeType"`
	Item       map[string]string `json:"item"`
	NewContent *adoNewContent    `json:"newContent,omitempty"`
}

type adoNewContent struct {
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
}

type adoPushResponse struct {
	URL     string `json:"url"`
	Commits []struct {
		CommitID string `json:"commitId"`
	} `json:"commits"`
}

type adoIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
}
