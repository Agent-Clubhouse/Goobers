package providerfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

const (
	normalizedADOOrganization = "fixture-org"
	normalizedADOProject      = "fixture-project"

	// ADOFixtureTag is the tag the seeded fixture work item carries
	// (test/adolive provision creates it). The open-work-items listing is
	// filtered to it: ListWorkItems returns the oldest open items first, up
	// to a limit, so in a live project with more than that many older open
	// items an unfiltered listing never reaches the seeded item, and the
	// recorded listing would churn with every unrelated item anyway.
	ADOFixtureTag = "goobers-fixture"
	// adoFixtureListLimit bounds the filtered listing.
	adoFixtureListLimit = 100
	// normalizedADOWorkItem replaces the live fixture's work-item number in
	// every recording, so a re-provisioned fixture (a new number) does not
	// read as drift against the checked-in baseline.
	normalizedADOWorkItem = "7"
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
	return refreshWithBackend(ctx, &adoRefreshBackend{cfg: cfg})
}

type adoRefreshBackend struct {
	cfg          ADORefreshConfig
	baseURL      string
	organization string
	client       HTTPClient
	identity     adoIdentity
}

func (b *adoRefreshBackend) validate() error {
	cfg := b.cfg
	baseURL, organization, err := parseADOOrganizationURL(cfg.OrganizationURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Project) == "" {
		return fmt.Errorf("ADO project is required")
	}
	workItemID, err := strconv.Atoi(cfg.WorkItem)
	if err != nil || workItemID <= 0 {
		return fmt.Errorf("ADO work item must be a positive number")
	}
	if cfg.Token == "" {
		return fmt.Errorf("ADO PAT is required")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	b.baseURL = baseURL
	b.organization = organization
	b.client = client
	b.identity = newADOIdentity(organization, cfg.Project, cfg.WorkItem)
	return nil
}

func (b *adoRefreshBackend) providerName() string {
	return string(providers.ProviderADO)
}

func (b *adoRefreshBackend) repositoryIdentity() Repository {
	return Repository{Owner: normalizedADOOrganization, Name: normalizedADOProject}
}

func (b *adoRefreshBackend) targetIdentity() (string, string) {
	return normalizedADOWorkItem, ""
}

func (b *adoRefreshBackend) requestSet(client HTTPClient) []refreshRequest {
	provider := providers.NewADOProvider(b.organization, b.cfg.Project, b.cfg.Token, func(p *providers.ADOProvider) {
		p.BaseURL = b.baseURL
		p.Client = client
	})
	repository := providers.RepositoryRef{Project: b.cfg.Project}
	return []refreshRequest{
		{
			name: "list-open-work-items",
			execute: func(ctx context.Context, _ HTTPClient) error {
				items, err := provider.ListWorkItems(ctx, adoFixtureListRequest(repository))
				if err != nil {
					return fmt.Errorf("list open ADO work items: %w", err)
				}
				for _, item := range items {
					if item.ID == b.cfg.WorkItem {
						return nil
					}
				}
				return fmt.Errorf(
					"list open ADO work items tagged %s did not return seeded work item %s (%d listed): the item must be open and tagged %s; omit -work-item and pass -provision-fixture to resolve or recreate the fixture",
					ADOFixtureTag, b.cfg.WorkItem, len(items), ADOFixtureTag)
			},
		},
		{
			name: "get-work-item",
			execute: func(ctx context.Context, _ HTTPClient) error {
				if _, err := provider.GetWorkItem(ctx, repository, b.cfg.WorkItem); err != nil {
					return fmt.Errorf("get seeded ADO work item: %w", err)
				}
				return nil
			},
		},
	}
}

func (b *adoRefreshBackend) httpClient() HTTPClient {
	return b.client
}

func (b *adoRefreshBackend) decorateRequest(*http.Request) {}

func (b *adoRefreshBackend) normalizePath(path string) string {
	return b.identity.replace(path)
}

func (b *adoRefreshBackend) normalizeBody(body []byte) (json.RawMessage, error) {
	return b.identity.normalizeJSON(body)
}

func (b *adoRefreshBackend) normalizeResponseHeaders(headers http.Header) map[string]string {
	return normalizeADOHeaders(headers)
}

func checkADOContract(ctx context.Context, fixture Fixture) error {
	return checkMappedContract(ctx, fixture, adoContractBackend{fixture: fixture})
}

type adoContractBackend struct {
	fixture Fixture
}

var _ contractBackend[providers.WorkItem] = adoContractBackend{}

func (b adoContractBackend) provider(client HTTPClient) mappedContractProvider[providers.WorkItem] {
	provider := providers.NewADOProvider(
		b.fixture.Repository.Owner,
		b.fixture.Repository.Name,
		"fixture-token",
		func(p *providers.ADOProvider) {
			p.BaseURL = "https://fixture.invalid"
			p.Client = client
		},
	)
	repository := providers.RepositoryRef{Project: b.fixture.Repository.Name}
	return mappedContractProvider[providers.WorkItem]{
		list: func(ctx context.Context) ([]providers.WorkItem, error) {
			return provider.ListWorkItems(ctx, adoFixtureListRequest(repository))
		},
		get: func(ctx context.Context) (providers.WorkItem, error) {
			return provider.GetWorkItem(ctx, repository, b.fixture.Issue)
		},
	}
}

func (b adoContractBackend) targetID() string                    { return b.fixture.Issue }
func (adoContractBackend) listOperation() string                 { return "ListWorkItems" }
func (adoContractBackend) getOperation() string                  { return "GetWorkItem" }
func (adoContractBackend) itemID(item providers.WorkItem) string { return item.ID }

func (b adoContractBackend) assertIdentity(item providers.WorkItem) error {
	if item.Provider != providers.ProviderADO {
		return fmt.Errorf("provider = %q, want %q", item.Provider, providers.ProviderADO)
	}
	if item.ID != b.fixture.Issue || strings.TrimSpace(item.Type) == "" {
		return fmt.Errorf("mapped work-item identity = %s/%s, want non-empty type/%s", item.Type, item.ID, b.fixture.Issue)
	}
	return nil
}

func (adoContractBackend) assertRequiredFields(item providers.WorkItem) error {
	if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.URL) == "" {
		return fmt.Errorf("mapped work item must have a title and URL")
	}
	if item.CreatedAt == nil || item.UpdatedAt == nil {
		return fmt.Errorf("mapped work item must preserve created and changed dates")
	}
	return nil
}

func (b adoContractBackend) assertConsistency(listed, item providers.WorkItem) error {
	if listed.Title != item.Title || listed.State != item.State || listed.URL != item.URL {
		return fmt.Errorf("list/get mappings disagree for work item %s", b.fixture.Issue)
	}
	return nil
}

func (b adoContractBackend) missingItemError() error {
	return fmt.Errorf("ListWorkItems did not return fixture work item %s", b.fixture.Issue)
}

// adoFixtureListRequest is the open-work-items listing both the refresh and
// the contract replay issue: open items tagged ADOFixtureTag, oldest first.
func adoFixtureListRequest(repository providers.RepositoryRef) providers.ListWorkItemsRequest {
	return providers.ListWorkItemsRequest{
		Repository:  repository,
		State:       "open",
		Labels:      []string{ADOFixtureTag},
		OldestFirst: true,
		Limit:       adoFixtureListLimit,
	}
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

// adoIdentity replaces the live organization, project and fixture work-item
// number with their normalized placeholders.
type adoIdentity struct {
	organization string
	project      string
	workItem     string
	// workItemRef matches the fixture's number in work-item paths and URLs
	// (.../workItems/N, .../_workitems/edit/N) but not as a prefix of a longer
	// number.
	workItemRef *regexp.Regexp
}

func newADOIdentity(organization, project, workItem string) adoIdentity {
	return adoIdentity{
		organization: organization,
		project:      project,
		workItem:     workItem,
		workItemRef:  regexp.MustCompile(`(?i)(/workitems/|/_workitems/edit/)` + regexp.QuoteMeta(workItem) + `\b`),
	}
}

func (a adoIdentity) normalizeJSON(raw []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	value = a.normalizeValue("", value)
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func (a adoIdentity) normalizeValue(key string, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, childValue := range typed {
			typed[childKey] = a.normalizeValue(childKey, childValue)
		}
		return typed
	case []any:
		for i, childValue := range typed {
			typed[i] = a.normalizeValue(key, childValue)
		}
		return typed
	case string:
		typed = a.replace(typed)
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
		lowerKey := strings.ToLower(key)
		if lowerKey == "rev" || strings.HasSuffix(lowerKey, ".rev") {
			return json.Number("0")
		}
		if (lowerKey == "id" || strings.HasSuffix(lowerKey, ".id")) && typed.String() == a.workItem {
			return json.Number(normalizedADOWorkItem)
		}
		return typed
	default:
		return value
	}
}

func isADOTimestampField(key string) bool {
	return key == "timestamp" || key == "asof" || strings.HasSuffix(key, "date") || strings.HasSuffix(key, "_at")
}

func (a adoIdentity) replace(value string) string {
	organization, project := a.organization, a.project
	replacements := [][2]string{
		{"/" + url.PathEscape(organization) + "/" + url.PathEscape(project), "/" + normalizedADOOrganization + "/" + normalizedADOProject},
		{"/" + organization + "/" + project, "/" + normalizedADOOrganization + "/" + normalizedADOProject},
		{"/" + url.PathEscape(organization) + "/", "/" + normalizedADOOrganization + "/"},
		{"/" + organization + "/", "/" + normalizedADOOrganization + "/"},
	}
	for _, replacement := range replacements {
		value = strings.ReplaceAll(value, replacement[0], replacement[1])
	}
	if a.workItemRef != nil {
		value = a.workItemRef.ReplaceAllString(value, "${1}"+normalizedADOWorkItem)
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
