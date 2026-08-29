package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxPerPage is the GitHub REST API's maximum page size; getAllPages requests
// it to minimize round trips when following pagination (#139).
const maxPerPage = 100

const defaultProviderHTTPTimeout = 60 * time.Second

// errStopPaging lets a getAllPages callback halt pagination early (e.g. once a
// bounded list has collected enough items) without surfacing an error.
var errStopPaging = errors.New("stop paging")

// withPerPage sets per_page=n on endpoint, unless the caller already pinned a
// per_page (a Limit-bounded list keeps its own page size).
func withPerPage(endpoint string, n int) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	q := u.Query()
	if q.Get("per_page") == "" {
		q.Set("per_page", strconv.Itoa(n))
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// readPage reads and closes a paginated GET response, returning the raw body
// and the rel="next" URL from the Link header ("" when there is no next page).
func readPage(resp *http.Response, method, endpoint string) ([]byte, string, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("%s %s: read body: %w", method, endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", newProviderResponseError(resp, method, endpoint, body)
	}
	return body, parseNextLink(resp.Header.Get("Link")), nil
}

// parseNextLink extracts the rel="next" URL from a GitHub Link header, e.g.
//
//	<https://api.github.com/...&page=2>; rel="next", <...&page=5>; rel="last"
//
// returning "" when there is no next page.
func parseNextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, attr := range segs[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				return urlPart[1 : len(urlPart)-1]
			}
		}
	}
	return ""
}

// HTTPClient sends HTTP requests for provider implementations.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type providerResponseError struct {
	method             string
	endpoint           string
	statusCode         int
	body               string
	retryAfter         string
	rateLimitRemaining string
	rateLimitReset     string
}

func (e *providerResponseError) Error() string {
	message := fmt.Sprintf("%s %s failed: status %d: %s", e.method, e.endpoint, e.statusCode, e.body)
	return message + retryGuidanceSuffix(e.retryAfter, e.rateLimitRemaining, e.rateLimitReset)
}

func (e *providerResponseError) hasRetryGuidance() bool {
	return e.retryAfter != "" || e.rateLimitRemaining == "0"
}

// IsNotFoundError reports whether err is a typed provider response with HTTP 404.
func IsNotFoundError(err error) bool {
	var responseErr *providerResponseError
	return errors.As(err, &responseErr) && responseErr.statusCode == http.StatusNotFound
}

// isIdempotentHTTPMethod reports whether method is safe to retry
// automatically after a transport failure or 5xx without risking a
// duplicate side effect (#2026), shared by every provider's low-level send
// loop. GET/HEAD/PUT/DELETE are idempotent by HTTP semantics; POST and
// PATCH are excluded by default — a provider's write surface (branch/
// commit/PR/work-item mutations, comments) has no transport-level dedup
// marker to make a blind retry of those safe, so a lost response is
// surfaced as an error rather than silently risking a duplicate commit,
// comment, or state transition.
func isIdempotentHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// ErrMergeConflict is the provider-neutral sentinel a MergePullRequest
// implementation wraps when the forge reports a confirmed merge conflict
// through a channel other than GitHub's 405-with-wording response — e.g.
// ADO's completion job reports mergeStatus="conflicts" in an ordinary 200
// response body, not an HTTP error (CONF-3 #2076). IsMergeConflictError
// recognizes either shape.
var ErrMergeConflict = errors.New("provider reported a merge conflict")

// IsForbiddenPATError reports whether err is a typed provider response GitHub
// returned specifically because a fine-grained personal access token lacks a
// permission that has no equivalent grant to request (issue #2685: a
// fine-grained PAT's grantable permission list has no "Checks" entry at all,
// so a private repo's commits/{ref}/check-runs read is permanently
// unreachable on that token type — this is not a transient or
// differently-scoped 403). Deliberately narrow, mirroring
// IsMergeConflictError's discipline: GitHub returns 403 for many unrelated
// reasons (rate limiting, org SSO enforcement, plain missing scope), so the
// status code alone is never sufficient — the response body must also name
// this exact condition.
func IsForbiddenPATError(err error) bool {
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	if responseErr.statusCode != http.StatusForbidden {
		return false
	}
	return strings.Contains(responseErr.body, "Resource not accessible by personal access token")
}

// IsMergeConflictError reports whether err is a typed provider response that
// the forge returned specifically because the pull request has merge
// conflicts (issue #1751). A conflicted PR is a normal business refusal —
// merge-pr records it as a non-merge so merge-review's fail branch can count
// and demote it — not the infrastructure failure a bare provider error
// implies.
//
// Deliberately narrow. GitHub answers the merge endpoint with 405 for several
// distinct policy refusals (draft PRs, branch-protection blocks, ruleset
// violations), and #1751 requires that only a confirmed conflict be
// reclassified: an unrecognized 405 must keep the generic provider-error
// behavior rather than be silently recorded as a conflict refusal. So the
// status code alone is never sufficient — the response body must also name
// the condition. ADO has no such HTTP-status-coded refusal (its completion
// job reports conflicts via mergeStatus in a 200 response, CONF-3 #2076),
// so its MergePullRequest wraps ErrMergeConflict directly instead of
// constructing a providerResponseError.
func IsMergeConflictError(err error) bool {
	if errors.Is(err, ErrMergeConflict) {
		return true
	}
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	if responseErr.statusCode != http.StatusMethodNotAllowed {
		return false
	}
	return mentionsMergeConflict(responseErr.body)
}

// IsRequiredStatusCheckPendingError reports whether GitHub refused a merge
// because a required status check has not reported yet. GitHub uses 405 for
// unrelated policy refusals too, so both the status and API message must match.
func IsRequiredStatusCheckPendingError(err error) bool {
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	if responseErr.statusCode != http.StatusMethodNotAllowed {
		return false
	}
	var response struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(responseErr.body), &response); err != nil {
		return false
	}
	message := strings.ToLower(strings.Join(strings.Fields(response.Message), " "))
	return strings.Contains(message, "required status check ") &&
		strings.Contains(message, " is expected")
}

// mentionsMergeConflict reports whether a provider response body names a
// merge-conflict refusal. GitHub's merge endpoint phrases this as
// "Pull Request is not mergeable"; the explicit "merge conflict" wording is
// accepted too so a forge that says it plainly is not missed.
func mentionsMergeConflict(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "not mergeable") || strings.Contains(lowered, "merge conflict")
}

// IsPullRequestAlreadyExistsError reports whether err is a typed provider
// response the forge returned because a pull request already exists for the
// requested head/base (issue #1767). OpenPullRequest's own contract promises
// idempotency by checking first and creating only when nothing is found, but
// a second caller for the same stable run branch can still win a genuine
// create race between that check and the POST — a normal, expected business
// outcome (the PR OpenPullRequest was about to open already exists), not an
// infrastructure failure. The caller re-resolves via FindPullRequestByBranch
// on a classified error rather than this function returning the PR itself,
// so classification and recovery stay separate the way IsMergeConflictError
// and IsNotFoundError do.
//
// Deliberately narrow, mirroring IsMergeConflictError's discipline: GitHub's
// pull-creation endpoint returns 422 for many unrelated validation failures
// (empty diff, invalid base, etc.), so the status code alone is never
// sufficient — the response body must also name the condition.
func IsPullRequestAlreadyExistsError(err error) bool {
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	if responseErr.statusCode != http.StatusUnprocessableEntity {
		return false
	}
	return strings.Contains(strings.ToLower(responseErr.body), "already exists")
}

// retryGuidanceSuffix formats GitHub's raw rate-limit headers as the
// `(Retry-After="1", X-RateLimit-Remaining="0", X-RateLimit-Reset="...")`
// suffix shared by every error that surfaces a rate-limited response's
// guidance verbatim — providerResponseError's generic non-2xx path and
// RateLimitError's typed give-up (providers/github_issues.go) both append
// this SAME format, so the string classification IsTransientError's
// subprocess-crossed fallback depends on (hasRateLimitRetryGuidance,
// providers/transient.go) recognizes either one identically. Returns "" when
// none of the three raw values carry guidance (retryAfter empty and
// remaining isn't exactly "0" — GitHub's own signal for "quota exhausted,"
// never inferred from any other value).
func retryGuidanceSuffix(retryAfter, remaining, reset string) string {
	var guidance []string
	if retryAfter != "" {
		guidance = append(guidance, "Retry-After="+strconv.Quote(retryAfter))
	}
	if remaining == "0" {
		guidance = append(guidance, `X-RateLimit-Remaining="0"`)
		if reset != "" {
			guidance = append(guidance, "X-RateLimit-Reset="+strconv.Quote(reset))
		}
	}
	if len(guidance) == 0 {
		return ""
	}
	return " (" + strings.Join(guidance, ", ") + ")"
}

func newProviderResponseError(resp *http.Response, method, endpoint string, body []byte) error {
	return &providerResponseError{
		method:             method,
		endpoint:           endpoint,
		statusCode:         resp.StatusCode,
		body:               strings.TrimSpace(string(body)),
		retryAfter:         strings.TrimSpace(resp.Header.Get("Retry-After")),
		rateLimitRemaining: strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")),
		rateLimitReset:     strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")),
	}
}

// CommandRunner executes external commands such as git clone.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type environmentCommandRunner interface {
	RunWithEnv(context.Context, []string, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func (execRunner) RunWithEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	return cmd.CombinedOutput()
}

func httpClientOrDefault(client HTTPClient) HTTPClient {
	if client != nil {
		return client
	}
	return newProviderHTTPClient(defaultProviderHTTPTimeout)
}

func newProviderHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func commandRunnerOrDefault(runner CommandRunner) CommandRunner {
	if runner != nil {
		return runner
	}
	return execRunner{}
}

func newJSONRequest(ctx context.Context, method, endpoint string, body interface{}) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// readJSONResponse consumes and closes resp: it surfaces a non-2xx status as an
// error and otherwise decodes the body into out (when non-nil).
func readJSONResponse(resp *http.Response, method, endpoint string, out interface{}) error {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return newProviderResponseError(resp, method, endpoint, body)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// contextSleep waits for d or until ctx is cancelled, whichever comes first.
func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func joinURL(base string, elems ...string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}
	path := strings.TrimRight(u.Path, "/")
	for _, elem := range elems {
		path += "/" + strings.Trim(elem, "/")
	}
	u.Path = path
	return u.String(), nil
}

func addQuery(endpoint string, values url.Values) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	q := u.Query()
	for key, vals := range values {
		for _, val := range vals {
			q.Add(key, val)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func requireRepo(repo RepositoryRef) error {
	if repo.Name == "" && repo.ID == "" {
		return errors.New("repository name or id is required")
	}
	return nil
}

func requireOwnerRepo(repo RepositoryRef) error {
	if repo.Owner == "" {
		return errors.New("repository owner is required")
	}
	if repo.Name == "" {
		return errors.New("repository name is required")
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// statusLabelPrefix namespaces the labels that mirror a work item's Goobers
// processing status, kept distinct from workflow trust/ready labels so a status
// update never touches those.
const statusLabelPrefix = "goobers/status:"

func statusLabel(status WorkItemStatus) string {
	return statusLabelPrefix + string(status)
}

// StatusLabelFor returns the provider-visible status label that mirrors a work
// item's Goobers processing status (e.g. goobers/status:claimed). Exported so a
// caller translating the generic claim marker (LabelClaimed, "goobers:claimed",
// the GitHub plain-label convention) to Azure DevOps's status-label convention
// removes the right tag — an ADO claim is written as a status label, not a plain
// one, so removing LabelClaimed verbatim silently no-ops on an ADO board.
func StatusLabelFor(status WorkItemStatus) string {
	return statusLabel(status)
}

func replaceStatusLabel(labels []string, status WorkItemStatus) []string {
	next := make([]string, 0, len(labels)+1)
	for _, label := range labels {
		if strings.HasPrefix(label, "goobers/status:") {
			continue
		}
		next = append(next, label)
	}
	if status != "" {
		next = append(next, statusLabel(status))
	}
	return uniqueStrings(next)
}

func statusFromLabels(labels []string, fallbackState string) WorkItemStatus {
	for _, label := range labels {
		if strings.HasPrefix(label, statusLabelPrefix) {
			return WorkItemStatus(strings.TrimPrefix(label, statusLabelPrefix))
		}
	}
	switch strings.ToLower(fallbackState) {
	case "closed", "done", "resolved":
		return WorkItemStatusDone
	case "active", "in progress", "in-progress":
		return WorkItemStatusInProgress
	default:
		return WorkItemStatusOpen
	}
}

func basicAuth(username, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+token))
}

// RunIDFooterPrefix is the prefix of the run-id footer line withRunIDFooter
// embeds in every body it writes. It is control text: CreateWorkItem's
// idempotency lookup (#140) treats any body containing the footer for a run
// id as that run's own creation, so a writer that renders model-authored text
// into a body must refuse it (internal/nomination does).
const RunIDFooterPrefix = "goobers run-id: "

// runFooter is the marker line withRunIDFooter embeds to tie a created provider
// entity back to its run — and the exact term CreateWorkItem searches for to
// make creation idempotent (#140).
func runFooter(runID string) string {
	return RunIDFooterPrefix + runID
}

// withRunIDFooter appends a run-id breadcrumb footer to a PR body so the run
// journal (once #8 lands) can link the PR URL back to the run bidirectionally.
// A no-op when runID is empty.
func withRunIDFooter(body, runID string) string {
	if runID == "" {
		return body
	}
	footer := runFooter(runID)
	if body == "" {
		return "---\n" + footer
	}
	return body + "\n\n---\n" + footer
}

// capDescriptionWithFooter appends the run-ID footer and, when the result would
// exceed maxChars characters, trims the body so the whole description fits while
// keeping the footer intact. Some providers (notably Azure DevOps, which caps a
// PR description at 4000 characters and rejects anything longer with HTTP 400)
// enforce a hard limit that the structured PR body (which has no overall cap of
// its own) can exceed. The footer carries the run-id that open-pr relies on for
// idempotency and traceability, so it is always preserved; only the body is
// trimmed, preferring a line boundary so a markdown/HTML block is not sliced
// mid-line. maxChars <= 0 disables the cap. Length is counted in runes (Unicode
// code points), matching how ADO counts "characters".
func capDescriptionWithFooter(body, runID string, maxChars int) string {
	full := withRunIDFooter(body, runID)
	if maxChars <= 0 || utf8.RuneCountInString(full) <= maxChars {
		return full
	}
	const marker = "\n\n_… description truncated to fit the provider's length limit …_"
	footer := ""
	if runID != "" {
		if body == "" {
			footer = "---\n" + runFooter(runID)
		} else {
			footer = "\n\n---\n" + runFooter(runID)
		}
	}
	budget := maxChars - utf8.RuneCountInString(marker) - utf8.RuneCountInString(footer)
	if budget <= 0 {
		// Degenerate: the footer plus marker alone already exceed the limit.
		// Hard-truncate the fully rendered description on a rune boundary.
		return truncateRunes(full, maxChars)
	}
	trimmed := truncateRunes(body, budget)
	if idx := strings.LastIndexByte(trimmed, '\n'); idx > 0 {
		trimmed = trimmed[:idx]
	}
	return strings.TrimRight(trimmed, " \n") + marker + footer
}

// truncateRunes returns s limited to at most max runes, cutting on a rune
// boundary so multi-byte characters are never split.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func shouldEmitWorkItem(seen map[string]time.Time, item WorkItem) bool {
	updated := time.Time{}
	if item.UpdatedAt != nil {
		updated = *item.UpdatedAt
	}
	previous, ok := seen[item.ID]
	if ok && previous.Equal(updated) {
		return false
	}
	seen[item.ID] = updated
	return true
}

func normalizeCommitChange(changeType string, exists bool) (CommitChangeType, error) {
	switch CommitChangeType(changeType) {
	case "":
		if exists {
			return CommitChangeEdit, nil
		}
		return CommitChangeAdd, nil
	case CommitChangeAdd:
		if exists {
			return "", errors.New("cannot add an existing file")
		}
		return CommitChangeAdd, nil
	case CommitChangeEdit:
		if !exists {
			return "", errors.New("cannot edit a missing file")
		}
		return CommitChangeEdit, nil
	case CommitChangeDelete:
		if !exists {
			return "", errors.New("cannot delete a missing file")
		}
		return CommitChangeDelete, nil
	default:
		return "", fmt.Errorf("unsupported commit change type %q", changeType)
	}
}
