package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/fieldpredicate"
)

// GitHubProvider implements repo, backlog, and trigger operations for GitHub.
type GitHubProvider struct {
	BaseURL     string
	Token       string
	Client      HTTPClient
	Runner      CommandRunner
	attribution Attribution

	// tokenSource, when set, resolves the token per request (issue #14 seam);
	// otherwise Token is used.
	tokenSource TokenSource
	// recorder receives "external ref touched" facts for the run journal.
	recorder MutationRecorder
	// rateObserver receives rate-limit backoff signals for telemetry.
	rateObserver RateLimitObserver
	// quotaObserver receives absolute remaining/reset response headers.
	quotaObserver QuotaObserver
	// quotaGate reserves provider budget before each attempted request.
	quotaGate         QuotaRequestGate
	quotaGateInClient bool
	// maxRetries bounds transport and server-error retries on a single request.
	maxRetries          int
	maxRateLimitRetries int
	// maxRateLimitWait bounds the total time one request spends sleeping on
	// rate-limit backoff before giving up with a typed RateLimitError (#614).
	maxRateLimitWait time.Duration
	// configuredLogin, when set, is the provider identity's GitHub login,
	// declared in config rather than fetched. GitHub App installation tokens
	// cannot call GET /user ("Resource not accessible by integration"), so a
	// provider authenticating as an App CANNOT self-report its login — which
	// broke every trusted-comment check (claim markers, verdicts, handoffs)
	// the first time claiming ran under App auth (#3343). Bot logins are the
	// App slug plus "[bot]".
	configuredLogin string
	// loginSelfReportRefusal, when set, is the reason AuthenticatedLogin must
	// refuse INSTEAD of falling back to GET /user — a fail-closed identity
	// posture for a caller that knows the login should have been declared and
	// was not resolved (Goobers#3914: a stage pod whose dispatcher stamped no
	// bot login at all).
	//
	// It exists because the GET /user fallback is not a neutral default under
	// App auth: an installation token cannot call that endpoint, so the
	// fallback is a request that CANNOT succeed, and it fails far away with
	// "Resource not accessible by integration" — an error naming the forge for
	// a fault that is entirely local. Refusing before the request states the
	// actual cause and, under a PAT, is never reached (a caller only sets this
	// when identity resolution itself was unavailable).
	loginSelfReportRefusal string
	// now and sleep are injectable for deterministic rate-limit tests.
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// WithConfiguredLogin declares the login AuthenticatedLogin returns instead
// of calling GET /user — required when the credential is a GitHub App
// installation token, which cannot self-report (#3343).
func WithConfiguredLogin(login string) func(*GitHubProvider) {
	return func(p *GitHubProvider) {
		p.configuredLogin = strings.TrimSpace(login)
	}
}

// WithLoginSelfReportRefused makes AuthenticatedLogin FAIL rather than fall
// back to GET /user, carrying reason as the diagnosis (Goobers#3914).
//
// For the caller that cannot resolve the provider identity and knows the
// fallback is not a safe default: a goobers-CLI stage in a pod, where the
// instance config is unreadable and the dispatcher stamped no login. Under
// GitHub App auth GET /user is a request that cannot succeed, and a stage that
// makes it anyway reports a forge permission error for a platform wiring
// fault. WithConfiguredLogin wins if both are set — a resolved identity is
// never overridden by a refusal.
func WithLoginSelfReportRefused(reason string) func(*GitHubProvider) {
	return func(p *GitHubProvider) {
		p.loginSelfReportRefusal = strings.TrimSpace(reason)
	}
}

// NewGitHubProvider constructs a GitHub provider with optional overrides.
func NewGitHubProvider(token string, opts ...func(*GitHubProvider)) *GitHubProvider {
	p := &GitHubProvider{
		BaseURL:             "https://api.github.com",
		Token:               token,
		maxRetries:          defaultRateLimitRetries,
		maxRateLimitRetries: defaultRateLimitRetries,
		maxRateLimitWait:    defaultRateLimitMaxWait,
		now:                 time.Now,
		sleep:               contextSleep,
		jitter:              randomJitter,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.Client = httpClientOrDefault(p.Client)
	p.Runner = commandRunnerOrDefault(p.Runner)
	if setter, ok := p.Client.(interface{ SetQuotaRequestGate(QuotaRequestGate) }); ok && p.quotaGate != nil {
		setter.SetQuotaRequestGate(p.quotaGate)
		p.quotaGateInClient = true
	}
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

// WithQuotaObserver receives quota-window observations from provider responses.
func WithQuotaObserver(observer QuotaObserver) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.quotaObserver = observer }
}

// WithQuotaRequestGate reserves quota before each attempted provider request.
func WithQuotaRequestGate(gate QuotaRequestGate) func(*GitHubProvider) {
	return func(p *GitHubProvider) { p.quotaGate = gate }
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
	return func(p *GitHubProvider) { p.maxRateLimitRetries = n }
}

// WithMaxTransientRetries overrides how many times a request with a transport
// failure or 5xx response is retried before the error is surfaced.
func WithMaxTransientRetries(n int) func(*GitHubProvider) {
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

// Capabilities declares GitHub's current truth (design doc
// docs/design/provider-contract-conformance.md §3.2, CONF-1 #2074): GitHub
// is the V0 workload and reaches every optional surface except
// pr.status.publish, which is an Azure DevOps / Gitea policy-evidence
// surface (#772) GitHub has no equivalent for.
func (p *GitHubProvider) Capabilities() CapabilitySet {
	return mandatoryCapabilities().With(
		CapPRCompare,
		CapPRQueryAuthor, CapPRQueryAssignee, CapPRQueryRequestedReviewer,
		CapPRReviewSubmit, CapPRReviewThreads, CapPRReviewResolve,
		CapPRMerge, CapPRLandingDetectPolicy, CapPRLandingEnqueue, CapPRLandingPoll,
		CapPRUpdateBranch, CapBranchDelete,
		CapRepoPolicyRead,
		CapBacklogBlockers,
	)
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
	return subscribeToWorkItems(ctx, sub, ProviderGitHub, "open", p.ListWorkItems), nil
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

// RepositorySizeKB returns the repository's on-disk size in KB, as reported
// by GitHub's repo-metadata endpoint (GET /repos/{owner}/{repo}, "size"
// field) — used by `goobers validate --check-repos` (#1547) to warn on an
// oversized target repository before it is checked out.
func (p *GitHubProvider) RepositorySizeKB(ctx context.Context, repo RepositoryRef) (int64, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name)
	if err != nil {
		return 0, err
	}
	var out struct {
		Size int64 `json:"size"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return 0, err
	}
	return out.Size, nil
}

type githubIssue struct {
	ID                       int64                           `json:"id"`
	Number                   int                             `json:"number"`
	Title                    string                          `json:"title"`
	Body                     string                          `json:"body"`
	State                    string                          `json:"state"`
	Locked                   bool                            `json:"locked"`
	Comments                 int                             `json:"comments"`
	HTMLURL                  string                          `json:"html_url"`
	User                     githubUser                      `json:"user"`
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
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
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
	ID                 int64         `json:"id"`
	Number             int           `json:"number"`
	Title              string        `json:"title"`
	State              string        `json:"state"`
	Merged             bool          `json:"merged"`
	MergedAt           *time.Time    `json:"merged_at"`
	MergeCommitSHA     string        `json:"merge_commit_sha"`
	ClosedAt           *time.Time    `json:"closed_at"`
	Mergeable          *bool         `json:"mergeable"`
	MergeableState     string        `json:"mergeable_state"`
	Draft              bool          `json:"draft"`
	Body               string        `json:"body"`
	HTMLURL            string        `json:"html_url"`
	User               githubUser    `json:"user"`
	Assignees          []githubUser  `json:"assignees"`
	RequestedReviewers []githubUser  `json:"requested_reviewers"`
	Labels             []githubLabel `json:"labels"`
	UpdatedAt          time.Time     `json:"updated_at"`
	Head               struct {
		Ref  string          `json:"ref"`
		SHA  string          `json:"sha"`
		Repo *restRepository `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

type githubMergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// githubBranchRule is one entry in GET .../rules/branches/{branch}'s
// response array — every ruleset rule that actually applies to the branch.
// DetectMergePolicy only reads Type ("merge_queue"); GetRepoPolicy also
// decodes Parameters for the "required_status_checks" rule type into
// githubRequiredStatusChecksParameters. Other rule types' Parameters shapes
// are not modeled here.
type githubBranchRule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// githubRequiredStatusChecksParameters is the Parameters shape of a
// "required_status_checks" branch rule.
type githubRequiredStatusChecksParameters struct {
	RequiredStatusChecks []struct {
		Context string `json:"context"`
	} `json:"required_status_checks"`
}

// githubRepoDetail is the subset of GET .../repos/{owner}/{repo} GetRepoPolicy
// reads: the repo-level flags controlling which merge methods a pull request
// may use. GitHub does not model "the required method" directly — a repo
// that allows exactly one of these is, in effect, requiring it (the
// squash-only scenario #877/#916 cite).
type githubRepoDetail struct {
	AllowMergeCommit bool `json:"allow_merge_commit"`
	AllowSquashMerge bool `json:"allow_squash_merge"`
	AllowRebaseMerge bool `json:"allow_rebase_merge"`
}

// allowedMergeMethods lists every merge method d's repo currently permits, in
// a stable merge/squash/rebase order.
func (d githubRepoDetail) allowedMergeMethods() []MergeMethod {
	var methods []MergeMethod
	if d.AllowMergeCommit {
		methods = append(methods, MergeMethodMerge)
	}
	if d.AllowSquashMerge {
		methods = append(methods, MergeMethodSquash)
	}
	if d.AllowRebaseMerge {
		methods = append(methods, MergeMethodRebase)
	}
	return methods
}

type githubPullRequestFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
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
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Output     struct {
		Summary string `json:"summary"`
	} `json:"output"`
}

type githubActionsRunsResponse struct {
	WorkflowRuns []githubActionsRun `json:"workflow_runs"`
}

type githubActionsRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

type githubCheckAnnotation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Level     string `json:"annotation_level"`
	Title     string `json:"title"`
	Message   string `json:"message"`
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
		Revision:       timeRevision(issue.UpdatedAt),
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
		Fields:         githubIssueFields(issue),
		BlockedByCount: blockedByCount,
		Raw:            issue,
		Integrity:      apiintegrity.Unapproved,
	}
}

func containsExactLine(body, marker string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if line == marker {
			return true
		}
	}
	return false
}

func timeRevision(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func checkWorkItemRevision(item WorkItem, expected string) error {
	if expected == "" {
		return fmt.Errorf("expected revision is required for work item %s", item.ID)
	}
	if item.Revision != expected {
		return &RevisionConflictError{ItemID: item.ID, Expected: expected, Actual: item.Revision}
	}
	return nil
}

func githubIssueFields(issue githubIssue) fieldpredicate.Fields {
	fields := fieldpredicate.Fields{
		"id":       issue.ID,
		"number":   int64(issue.Number),
		"state":    issue.State,
		"locked":   issue.Locked,
		"comments": int64(issue.Comments),
	}
	if issue.User.Login != "" {
		fields["user.login"] = issue.User.Login
	}
	if issue.CreatedAt != nil {
		fields["created_at"] = issue.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if issue.UpdatedAt != nil {
		fields["updated_at"] = issue.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if len(issue.Assignees) > 0 {
		fields["assignee.login"] = issue.Assignees[0].Login
	}
	if issue.Milestone != nil {
		fields["milestone.number"] = int64(issue.Milestone.Number)
		fields["milestone.title"] = issue.Milestone.Title
	}
	if issue.IssueDependenciesSummary != nil {
		fields["issue_dependencies_summary.total_blocked_by"] = int64(issue.IssueDependenciesSummary.TotalBlockedBy)
	}
	return fields
}
