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
)

// maxPerPage is the GitHub REST API's maximum page size; getAllPages requests
// it to minimize round trips when following pagination (#139).
const maxPerPage = 100

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
	if !e.hasRetryGuidance() {
		return message
	}
	var guidance []string
	if e.retryAfter != "" {
		guidance = append(guidance, "Retry-After="+strconv.Quote(e.retryAfter))
	}
	if e.rateLimitRemaining == "0" {
		guidance = append(guidance, "X-RateLimit-Remaining=\"0\"")
		if e.rateLimitReset != "" {
			guidance = append(guidance, "X-RateLimit-Reset="+strconv.Quote(e.rateLimitReset))
		}
	}
	return message + " (" + strings.Join(guidance, ", ") + ")"
}

func (e *providerResponseError) hasRetryGuidance() bool {
	return e.retryAfter != "" || e.rateLimitRemaining == "0"
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

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func httpClientOrDefault(client HTTPClient) HTTPClient {
	if client != nil {
		return client
	}
	return http.DefaultClient
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

func doJSON(client HTTPClient, req *http.Request, out interface{}) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	return readJSONResponse(resp, req.Method, req.URL.String(), out)
}

// readJSONResponse consumes and closes resp: it surfaces a non-2xx status as an
// error and otherwise decodes the body into out (when non-nil). It is shared by the
// single-shot doJSON path and the retry-aware provider request loops.
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

// withRunIDFooter appends a run-id breadcrumb footer to a PR body so the run
// journal (once #8 lands) can link the PR URL back to the run bidirectionally.
// A no-op when runID is empty.
// runFooter is the marker line withRunIDFooter embeds to tie a created provider
// entity back to its run — and the exact term CreateWorkItem searches for to
// make creation idempotent (#140).
func runFooter(runID string) string {
	return "goobers run-id: " + runID
}

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
