// Package providerfixture records, normalizes, replays, and compares provider
// contract fixtures.
package providerfixture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

// SchemaVersion is the current serialized provider fixture schema.
const SchemaVersion = "v1"

const (
	normalizedOwner     = "fixture-owner"
	normalizedRepo      = "fixture-repo"
	normalizedTimestamp = "2000-01-01T00:00:00Z"
	maxResponseBytes    = 16 << 20
)

// ErrContractAssertion identifies a fixture that violates provider behavior.
var ErrContractAssertion = errors.New("provider contract assertion failed")

// ErrFixtureDrift identifies a material normalized difference from the baseline.
var ErrFixtureDrift = errors.New("material normalized fixture drift")

// Repository identifies the normalized provider fixture scope.
type Repository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// Fixture contains normalized responses for a provider contract request set.
type Fixture struct {
	SchemaVersion string     `json:"schemaVersion"`
	Provider      string     `json:"provider"`
	Repository    Repository `json:"repository"`
	Issue         string     `json:"issue,omitempty"`
	PullRequest   string     `json:"pullRequest,omitempty"`
	Exchanges     []Exchange `json:"exchanges"`
}

// Exchange records one request and its normalized response.
type Exchange struct {
	Name     string          `json:"name"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Response FixtureResponse `json:"response"`
}

// FixtureResponse is the replayable portion of a provider API response.
type FixtureResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body"`
}

// HTTPClient is the transport seam used by Refresh.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// RefreshConfig selects the live fixture source and HTTP transport.
type RefreshConfig struct {
	Repository  Repository
	Issue       string
	PullRequest string
	Token       string
	BaseURL     string
	Client      HTTPClient
}

type requestSpec struct {
	name   string
	method string
	path   string
}

// Refresh executes the provider contract request set and returns normalized responses.
func Refresh(ctx context.Context, cfg RefreshConfig) (Fixture, error) {
	if cfg.Repository.Owner == "" || cfg.Repository.Name == "" {
		return Fixture{}, fmt.Errorf("repository owner and name are required")
	}
	target, err := fixtureTarget(cfg.Issue, cfg.PullRequest)
	if err != nil {
		return Fixture{}, err
	}
	if cfg.Token == "" {
		return Fixture{}, fmt.Errorf("dedicated provider fixture token is required")
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return Fixture{}, fmt.Errorf("parse GitHub base URL: %w", err)
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	specs := target.requestSet(cfg.Repository)
	fixture := Fixture{
		SchemaVersion: SchemaVersion,
		Provider:      "github",
		Repository:    Repository{Owner: normalizedOwner, Name: normalizedRepo},
		Issue:         cfg.Issue,
		PullRequest:   cfg.PullRequest,
		Exchanges:     make([]Exchange, 0, len(specs)),
	}
	for _, spec := range specs {
		req, err := http.NewRequestWithContext(ctx, spec.method, baseURL+spec.path, nil)
		if err != nil {
			return Fixture{}, fmt.Errorf("create %s request: %w", spec.name, err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		req.Header.Set("User-Agent", "goobers-provider-fixture-refresh")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := client.Do(req)
		if err != nil {
			return Fixture{}, fmt.Errorf("%s request: %w", spec.name, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return Fixture{}, fmt.Errorf("read %s response: %w", spec.name, readErr)
		}
		if closeErr != nil {
			return Fixture{}, fmt.Errorf("close %s response: %w", spec.name, closeErr)
		}
		if len(body) > maxResponseBytes {
			return Fixture{}, fmt.Errorf("%s response exceeds %d bytes", spec.name, maxResponseBytes)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return Fixture{}, fmt.Errorf("%s request returned status %d: %s", spec.name, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		normalizedBody, err := normalizeJSON(body, cfg.Repository)
		if err != nil {
			return Fixture{}, fmt.Errorf("normalize %s response: %w", spec.name, err)
		}
		fixture.Exchanges = append(fixture.Exchanges, Exchange{
			Name:   spec.name,
			Method: spec.method,
			Path:   replaceRepository(spec.path, cfg.Repository),
			Response: FixtureResponse{
				Status:  resp.StatusCode,
				Headers: normalizeHeaders(resp.Header, cfg.Repository),
				Body:    normalizedBody,
			},
		})
	}
	return fixture, nil
}

// Read loads and validates a normalized fixture.
func Read(path string) (Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, err
	}
	var fixture Fixture
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture: %w", err)
	}
	if err := validate(fixture); err != nil {
		return Fixture{}, err
	}
	return fixture, nil
}

// Write validates and serializes a normalized fixture.
func Write(path string, fixture Fixture) error {
	raw, err := canonical(fixture)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create fixture directory: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write fixture: %w", err)
	}
	return nil
}

// CheckContract replays a fixture through its provider and checks its mappings.
func CheckContract(ctx context.Context, fixture Fixture) error {
	if err := validate(fixture); err != nil {
		return fmt.Errorf("%w: %w", ErrContractAssertion, err)
	}
	if fixture.Provider == "ado" {
		return checkADOContract(ctx, fixture)
	}
	if fixture.PullRequest != "" {
		return checkPullRequestContract(ctx, fixture)
	}
	return checkIssueContract(ctx, fixture)
}

func checkIssueContract(ctx context.Context, fixture Fixture) error {
	client := &replayClient{exchanges: fixture.Exchanges, used: make([]bool, len(fixture.Exchanges))}
	provider := providers.NewGitHubProvider(
		"fixture-token",
		func(p *providers.GitHubProvider) { p.BaseURL = "https://fixture.invalid" },
		providers.WithHTTPClient(client),
		providers.WithMaxTransientRetries(0),
	)
	repo := providers.RepositoryRef{Owner: fixture.Repository.Owner, Name: fixture.Repository.Name}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:  repo,
		State:       "open",
		OldestFirst: true,
		Limit:       100,
		Page:        1,
	})
	if err != nil {
		return fmt.Errorf("%w: ListWorkItems: %w", ErrContractAssertion, err)
	}
	item, err := provider.GetWorkItem(ctx, repo, fixture.Issue)
	if err != nil {
		return fmt.Errorf("%w: GetWorkItem: %w", ErrContractAssertion, err)
	}
	if item.Provider != providers.ProviderGitHub {
		return fmt.Errorf("%w: provider = %q, want %q", ErrContractAssertion, item.Provider, providers.ProviderGitHub)
	}
	if item.Type != "issue" || item.ID != fixture.Issue {
		return fmt.Errorf("%w: mapped item identity = %s/%s, want issue/%s", ErrContractAssertion, item.Type, item.ID, fixture.Issue)
	}
	if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.URL) == "" {
		return fmt.Errorf("%w: mapped issue must have a title and URL", ErrContractAssertion)
	}
	if item.CreatedAt == nil || item.UpdatedAt == nil {
		return fmt.Errorf("%w: mapped issue must preserve created_at and updated_at", ErrContractAssertion)
	}
	found := false
	for _, listed := range items {
		if listed.ID != fixture.Issue {
			continue
		}
		found = true
		if listed.Title != item.Title || listed.State != item.State || listed.URL != item.URL {
			return fmt.Errorf("%w: list/get mappings disagree for issue %s", ErrContractAssertion, fixture.Issue)
		}
	}
	if !found {
		return fmt.Errorf("%w: ListWorkItems did not return fixture issue %s", ErrContractAssertion, fixture.Issue)
	}
	if err := client.verifyConsumed(); err != nil {
		return fmt.Errorf("%w: %w", ErrContractAssertion, err)
	}
	return nil
}

func checkPullRequestContract(ctx context.Context, fixture Fixture) error {
	client := &replayClient{exchanges: fixture.Exchanges, used: make([]bool, len(fixture.Exchanges))}
	provider := providers.NewGitHubProvider(
		"fixture-token",
		func(p *providers.GitHubProvider) { p.BaseURL = "https://fixture.invalid" },
		providers.WithHTTPClient(client),
		providers.WithMaxTransientRetries(0),
	)
	repo := providers.RepositoryRef{Owner: fixture.Repository.Owner, Name: fixture.Repository.Name}
	items, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     repo,
		SkipCheckState: true,
	})
	if err != nil {
		return fmt.Errorf("%w: ListPullRequests: %w", ErrContractAssertion, err)
	}
	item, err := provider.GetPullRequest(ctx, repo, fixture.PullRequest)
	if err != nil {
		return fmt.Errorf("%w: GetPullRequest: %w", ErrContractAssertion, err)
	}
	if item.ID != fixture.PullRequest || item.Number <= 0 {
		return fmt.Errorf("%w: mapped pull request identity = %s/%d, want %s", ErrContractAssertion, item.ID, item.Number, fixture.PullRequest)
	}
	if strings.TrimSpace(item.URL) == "" || strings.TrimSpace(item.Head) == "" || strings.TrimSpace(item.Base) == "" {
		return fmt.Errorf("%w: mapped pull request must have a URL, head, and base", ErrContractAssertion)
	}
	if strings.TrimSpace(item.Author) == "" || len(item.Assignees) == 0 || len(item.RequestedReviewers) == 0 {
		return fmt.Errorf("%w: mapped pull request must preserve creator, assignees, and requested reviewers", ErrContractAssertion)
	}
	found := false
	for _, listed := range items {
		if listed.ID != fixture.PullRequest {
			continue
		}
		found = true
		if listed.URL != item.URL || listed.State != item.State || listed.Author != item.Author ||
			!slices.Equal(listed.Assignees, item.Assignees) ||
			!slices.Equal(listed.RequestedReviewers, item.RequestedReviewers) {
			return fmt.Errorf("%w: list/get mappings disagree for pull request %s", ErrContractAssertion, fixture.PullRequest)
		}
	}
	if !found {
		return fmt.Errorf("%w: ListPullRequests did not return fixture pull request %s", ErrContractAssertion, fixture.PullRequest)
	}
	if err := client.verifyConsumed(); err != nil {
		return fmt.Errorf("%w: %w", ErrContractAssertion, err)
	}
	return nil
}

// CheckDrift reports whether two normalized fixtures differ materially.
func CheckDrift(baseline, candidate Fixture) error {
	baselineRaw, err := canonical(baseline)
	if err != nil {
		return fmt.Errorf("canonicalize baseline: %w", err)
	}
	candidateRaw, err := canonical(candidate)
	if err != nil {
		return fmt.Errorf("canonicalize candidate: %w", err)
	}
	if bytes.Equal(baselineRaw, candidateRaw) {
		return nil
	}
	baselineDigest := sha256.Sum256(baselineRaw)
	candidateDigest := sha256.Sum256(candidateRaw)
	return fmt.Errorf("%w: baseline sha256:%x, candidate sha256:%x", ErrFixtureDrift, baselineDigest[:8], candidateDigest[:8])
}

func contractRequestSet(repo Repository, issue string) []requestSpec {
	owner := url.PathEscape(repo.Owner)
	name := url.PathEscape(repo.Name)
	query := url.Values{
		"direction": {"asc"},
		"page":      {"1"},
		"per_page":  {"100"},
		"sort":      {"created"},
		"state":     {"open"},
	}.Encode()
	return []requestSpec{
		{name: "list-open-issues", method: http.MethodGet, path: fmt.Sprintf("/repos/%s/%s/issues?%s", owner, name, query)},
		{name: "get-issue", method: http.MethodGet, path: fmt.Sprintf("/repos/%s/%s/issues/%s", owner, name, url.PathEscape(issue))},
	}
}

func pullRequestContractRequestSet(repo Repository, pullRequest string) []requestSpec {
	owner := url.PathEscape(repo.Owner)
	name := url.PathEscape(repo.Name)
	query := url.Values{
		"per_page": {"100"},
		"state":    {"open"},
	}.Encode()
	return []requestSpec{
		{name: "list-open-prs", method: http.MethodGet, path: fmt.Sprintf("/repos/%s/%s/pulls?%s", owner, name, query)},
		{name: "get-pr", method: http.MethodGet, path: fmt.Sprintf("/repos/%s/%s/pulls/%s", owner, name, url.PathEscape(pullRequest))},
	}
}

type targetKind int

const (
	issueTarget targetKind = iota
	pullRequestTarget
)

type target struct {
	kind   targetKind
	number string
}

func fixtureTarget(issue, pullRequest string) (target, error) {
	if (issue == "") == (pullRequest == "") {
		return target{}, fmt.Errorf("exactly one of issue or pull request is required")
	}
	kind := issueTarget
	number := issue
	name := "issue"
	if pullRequest != "" {
		kind = pullRequestTarget
		number = pullRequest
		name = "pull request"
	}
	parsed, err := strconv.Atoi(number)
	if err != nil || parsed <= 0 {
		return target{}, fmt.Errorf("%s must be a positive number", name)
	}
	return target{kind: kind, number: number}, nil
}

func (t target) requestSet(repo Repository) []requestSpec {
	if t.kind == pullRequestTarget {
		return pullRequestContractRequestSet(repo, t.number)
	}
	return contractRequestSet(repo, t.number)
}

func normalizeJSON(raw []byte, repo Repository) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	value = normalizeValue("", value, repo)
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func normalizeValue(key string, value any, repo Repository) any {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, childValue := range typed {
			typed[childKey] = normalizeValue(childKey, childValue, repo)
		}
		return typed
	case []any:
		for i, childValue := range typed {
			typed[i] = normalizeValue(key, childValue, repo)
		}
		return typed
	case string:
		typed = replaceRepository(typed, repo)
		lowerKey := strings.ToLower(key)
		if isIDField(lowerKey) {
			return "NORMALIZED"
		}
		if isTimestampField(lowerKey) {
			return normalizedTimestamp
		}
		return typed
	case json.Number:
		if isIDField(strings.ToLower(key)) {
			return json.Number("0")
		}
		return typed
	default:
		return value
	}
}

func isIDField(key string) bool {
	return key == "id" || strings.HasSuffix(key, "_id")
}

func isTimestampField(key string) bool {
	return key == "timestamp" || strings.HasSuffix(key, "_at")
}

func replaceRepository(value string, repo Repository) string {
	actual := repo.Owner + "/" + repo.Name
	return strings.ReplaceAll(value, actual, normalizedOwner+"/"+normalizedRepo)
}

func normalizeHeaders(headers http.Header, repo Repository) map[string]string {
	names := []string{
		"Content-Type",
		"Link",
		"X-GitHub-Api-Version-Selected",
		"X-GitHub-Media-Type",
		"X-RateLimit-Limit",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
		"X-RateLimit-Resource",
		"X-RateLimit-Used",
	}
	normalized := make(map[string]string)
	for _, name := range names {
		value := strings.TrimSpace(headers.Get(name))
		if value == "" {
			continue
		}
		switch name {
		case "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-Used":
			value = "0"
		default:
			value = replaceRepository(value, repo)
		}
		normalized[name] = value
	}
	return normalized
}

func canonical(fixture Fixture) ([]byte, error) {
	if err := validate(fixture); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode fixture: %w", err)
	}
	return raw, nil
}

func validate(fixture Fixture) error {
	if fixture.SchemaVersion != SchemaVersion {
		return fmt.Errorf("fixture schemaVersion = %q, want %q", fixture.SchemaVersion, SchemaVersion)
	}
	if fixture.Provider != "github" && fixture.Provider != "ado" {
		return fmt.Errorf("fixture provider = %q, want github or ado", fixture.Provider)
	}
	if fixture.Repository.Owner == "" || fixture.Repository.Name == "" {
		return fmt.Errorf("fixture repository owner and name are required")
	}
	target, err := fixtureTarget(fixture.Issue, fixture.PullRequest)
	if err != nil {
		return fmt.Errorf("fixture %w", err)
	}
	if len(fixture.Exchanges) == 0 {
		return fmt.Errorf("fixture has no exchanges")
	}
	names := make(map[string]struct{}, len(fixture.Exchanges))
	for _, exchange := range fixture.Exchanges {
		if exchange.Name == "" || exchange.Method == "" || !strings.HasPrefix(exchange.Path, "/") {
			return fmt.Errorf("fixture exchange name, method, and absolute path are required")
		}
		if _, exists := names[exchange.Name]; exists {
			return fmt.Errorf("fixture exchange name %q is duplicated", exchange.Name)
		}
		names[exchange.Name] = struct{}{}
		if exchange.Response.Status < 100 || exchange.Response.Status > 599 {
			return fmt.Errorf("fixture exchange %q has invalid status %d", exchange.Name, exchange.Response.Status)
		}
		if !json.Valid(exchange.Response.Body) {
			return fmt.Errorf("fixture exchange %q has invalid JSON body", exchange.Name)
		}
	}
	requiredExchanges := []string{"list-open-issues", "get-issue"}
	switch {
	case fixture.Provider == "ado":
		requiredExchanges = []string{"list-open-work-items", "get-work-item"}
	case target.kind == pullRequestTarget:
		requiredExchanges = []string{"list-open-prs", "get-pr"}
	}
	for _, required := range requiredExchanges {
		if _, ok := names[required]; !ok {
			return fmt.Errorf("fixture is missing %q exchange", required)
		}
	}
	return nil
}

type replayClient struct {
	exchanges []Exchange
	used      []bool
}

func (c *replayClient) Do(req *http.Request) (*http.Response, error) {
	path := req.URL.RequestURI()
	for i, exchange := range c.exchanges {
		if c.used[i] || exchange.Method != req.Method || exchange.Path != path {
			continue
		}
		c.used[i] = true
		headers := make(http.Header, len(exchange.Response.Headers))
		for name, value := range exchange.Response.Headers {
			headers.Set(name, value)
		}
		return &http.Response{
			StatusCode: exchange.Response.Status,
			Header:     headers,
			Body:       io.NopCloser(bytes.NewReader(exchange.Response.Body)),
			Request:    req,
		}, nil
	}
	return nil, fmt.Errorf("unexpected provider request %s %s", req.Method, path)
}

func (c *replayClient) verifyConsumed() error {
	for i, used := range c.used {
		if !used {
			exchange := c.exchanges[i]
			return fmt.Errorf("fixture exchange %q was not consumed", exchange.Name)
		}
	}
	return nil
}
