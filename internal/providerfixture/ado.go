package providerfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

const (
	normalizedADOOrganization = "fixture-org"
	normalizedADOProject      = "fixture-project"
)

// ADORefreshConfig selects the live Azure DevOps fixture source.
type ADORefreshConfig struct {
	OrganizationURL string
	Project         string
	WorkItem        string
	Token           string
	Client          HTTPClient
}

// RefreshADO executes the ADO work-item contract request set and normalizes it.
func RefreshADO(ctx context.Context, cfg ADORefreshConfig) (Fixture, error) {
	baseURL, organization, err := parseADOOrganizationURL(cfg.OrganizationURL)
	if err != nil {
		return Fixture{}, err
	}
	if strings.TrimSpace(cfg.Project) == "" {
		return Fixture{}, fmt.Errorf("ADO project is required")
	}
	workItemID, err := strconv.Atoi(cfg.WorkItem)
	if err != nil || workItemID <= 0 {
		return Fixture{}, fmt.Errorf("ADO work item must be a positive number")
	}
	if cfg.Token == "" {
		return Fixture{}, fmt.Errorf("ADO PAT is required")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	fixture := Fixture{
		SchemaVersion: SchemaVersion,
		Provider:      string(providers.ProviderADO),
		Repository: Repository{
			Owner: normalizedADOOrganization,
			Name:  normalizedADOProject,
		},
		Issue: cfg.WorkItem,
	}
	recorder := &adoRecordingClient{
		client:       client,
		fixture:      &fixture,
		organization: organization,
		project:      cfg.Project,
	}
	provider := providers.NewADOProvider(organization, cfg.Project, cfg.Token, func(p *providers.ADOProvider) {
		p.BaseURL = baseURL
		p.Client = recorder
	})
	repository := providers.RepositoryRef{Project: cfg.Project}

	recorder.begin("list-open-work-items")
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:  repository,
		State:       "open",
		OldestFirst: true,
		Limit:       100,
	})
	if err != nil {
		return Fixture{}, fmt.Errorf("list open ADO work items: %w", err)
	}
	found := false
	for _, item := range items {
		if item.ID == cfg.WorkItem {
			found = true
			break
		}
	}
	if !found {
		return Fixture{}, fmt.Errorf("list open ADO work items did not return seeded work item %s", cfg.WorkItem)
	}

	recorder.begin("get-work-item")
	if _, err := provider.GetWorkItem(ctx, repository, cfg.WorkItem); err != nil {
		return Fixture{}, fmt.Errorf("get seeded ADO work item: %w", err)
	}
	return fixture, nil
}

func checkADOContract(ctx context.Context, fixture Fixture) error {
	client := &replayClient{exchanges: fixture.Exchanges, used: make([]bool, len(fixture.Exchanges))}
	provider := providers.NewADOProvider(
		fixture.Repository.Owner,
		fixture.Repository.Name,
		"fixture-token",
		func(p *providers.ADOProvider) {
			p.BaseURL = "https://fixture.invalid"
			p.Client = client
		},
	)
	repository := providers.RepositoryRef{Project: fixture.Repository.Name}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:  repository,
		State:       "open",
		OldestFirst: true,
		Limit:       100,
	})
	if err != nil {
		return fmt.Errorf("%w: ListWorkItems: %w", ErrContractAssertion, err)
	}
	item, err := provider.GetWorkItem(ctx, repository, fixture.Issue)
	if err != nil {
		return fmt.Errorf("%w: GetWorkItem: %w", ErrContractAssertion, err)
	}
	if item.Provider != providers.ProviderADO {
		return fmt.Errorf("%w: provider = %q, want %q", ErrContractAssertion, item.Provider, providers.ProviderADO)
	}
	if item.ID != fixture.Issue || strings.TrimSpace(item.Type) == "" {
		return fmt.Errorf("%w: mapped work-item identity = %s/%s, want non-empty type/%s", ErrContractAssertion, item.Type, item.ID, fixture.Issue)
	}
	if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.URL) == "" {
		return fmt.Errorf("%w: mapped work item must have a title and URL", ErrContractAssertion)
	}
	if item.CreatedAt == nil || item.UpdatedAt == nil {
		return fmt.Errorf("%w: mapped work item must preserve created and changed dates", ErrContractAssertion)
	}
	found := false
	for _, listed := range items {
		if listed.ID != fixture.Issue {
			continue
		}
		found = true
		if listed.Title != item.Title || listed.State != item.State || listed.URL != item.URL {
			return fmt.Errorf("%w: list/get mappings disagree for work item %s", ErrContractAssertion, fixture.Issue)
		}
	}
	if !found {
		return fmt.Errorf("%w: ListWorkItems did not return fixture work item %s", ErrContractAssertion, fixture.Issue)
	}
	if err := client.verifyConsumed(); err != nil {
		return fmt.Errorf("%w: %w", ErrContractAssertion, err)
	}
	return nil
}

func parseADOOrganizationURL(raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("parse ADO organization URL %q", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		return "", "", fmt.Errorf("ADO organization URL must have https://host/organization form")
	}
	organization, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("parse ADO organization path: %w", err)
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), organization, nil
}

type adoRecordingClient struct {
	client       HTTPClient
	fixture      *Fixture
	organization string
	project      string
	operation    string
	request      int
}

func (c *adoRecordingClient) begin(operation string) {
	c.operation = operation
	c.request = 0
}

func (c *adoRecordingClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	closeErr := resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%s response exceeds %d bytes", c.operation, maxResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp, nil
	}
	normalizedBody, err := normalizeADOJSON(body, c.organization, c.project)
	if err != nil {
		return nil, fmt.Errorf("normalize %s response: %w", c.operation, err)
	}
	c.request++
	name := c.operation
	if c.request > 1 {
		name += "-" + strconv.Itoa(c.request)
	}
	c.fixture.Exchanges = append(c.fixture.Exchanges, Exchange{
		Name:   name,
		Method: req.Method,
		Path:   replaceADOIdentity(req.URL.RequestURI(), c.organization, c.project),
		Response: FixtureResponse{
			Status:  resp.StatusCode,
			Headers: normalizeADOHeaders(resp.Header),
			Body:    normalizedBody,
		},
	})
	return resp, nil
}

func normalizeADOJSON(raw []byte, organization, project string) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	value = normalizeADOValue("", value, organization, project)
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func normalizeADOValue(key string, value any, organization, project string) any {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, childValue := range typed {
			typed[childKey] = normalizeADOValue(childKey, childValue, organization, project)
		}
		return typed
	case []any:
		for i, childValue := range typed {
			typed[i] = normalizeADOValue(key, childValue, organization, project)
		}
		return typed
	case string:
		typed = replaceADOIdentity(typed, organization, project)
		lowerKey := strings.ToLower(key)
		if lowerKey == "id" || strings.HasSuffix(lowerKey, ".id") ||
			lowerKey == "descriptor" || strings.HasSuffix(lowerKey, "descriptor") {
			return "NORMALIZED"
		}
		if lowerKey == "imageurl" || strings.Contains(typed, "/_apis/GraphProfile/MemberAvatars/") {
			return "NORMALIZED"
		}
		if isADOTimestampField(lowerKey) {
			return normalizedTimestamp
		}
		return typed
	case json.Number:
		if strings.EqualFold(key, "rev") || strings.HasSuffix(strings.ToLower(key), ".rev") {
			return json.Number("0")
		}
		return typed
	default:
		return value
	}
}

func isADOTimestampField(key string) bool {
	return key == "timestamp" || key == "asof" || strings.HasSuffix(key, "date") || strings.HasSuffix(key, "_at")
}

func replaceADOIdentity(value, organization, project string) string {
	replacements := [][2]string{
		{"/" + url.PathEscape(organization) + "/" + url.PathEscape(project), "/" + normalizedADOOrganization + "/" + normalizedADOProject},
		{"/" + organization + "/" + project, "/" + normalizedADOOrganization + "/" + normalizedADOProject},
		{"/" + url.PathEscape(organization) + "/", "/" + normalizedADOOrganization + "/"},
		{"/" + organization + "/", "/" + normalizedADOOrganization + "/"},
	}
	for _, replacement := range replacements {
		value = strings.ReplaceAll(value, replacement[0], replacement[1])
	}
	if value == organization {
		return normalizedADOOrganization
	}
	if value == project {
		return normalizedADOProject
	}
	return value
}

func normalizeADOHeaders(headers http.Header) map[string]string {
	names := []string{"Content-Type", "X-RateLimit-Limit", "X-RateLimit-Remaining"}
	normalized := make(map[string]string)
	for _, name := range names {
		value := strings.TrimSpace(headers.Get(name))
		if value == "" {
			continue
		}
		if name != "Content-Type" {
			value = "0"
		}
		normalized[name] = value
	}
	return normalized
}
