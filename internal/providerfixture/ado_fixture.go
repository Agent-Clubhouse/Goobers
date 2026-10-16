package providerfixture

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

const (
	// ADOFixtureTitle is the title of the seeded fixture work item. Together
	// with ADOFixtureTag it identifies the item the drift workflow reads, so
	// a closed or deleted fixture is replaced rather than pinned by number.
	ADOFixtureTitle = "goobers provider fixture (do not close)"
	// ADOFixtureBody is the seeded fixture work item's description.
	ADOFixtureBody = "Stable seeded work item read by the ADO provider fixture-drift workflow (#4602). Do not edit or close it."
	// ADOFixtureDefaultType is the work item type a provisioned fixture uses
	// unless the caller picks another; the checked-in baseline records it.
	ADOFixtureDefaultType = "Issue"
)

// ErrADOFixtureMissing reports that no open fixture work item exists and the
// caller did not allow provisioning one.
var ErrADOFixtureMissing = errors.New("no open ADO provider fixture work item")

// ADOFixtureConfig selects the project whose fixture work item is resolved.
type ADOFixtureConfig struct {
	OrganizationURL string
	Project         string
	Token           string
	// WorkItemType is the type of a provisioned fixture; empty means
	// ADOFixtureDefaultType.
	WorkItemType string
	// Provision creates the fixture when no open one exists.
	Provision bool
	Client    HTTPClient
}

// ADOFixtureResolution identifies the fixture work item the refresh reads.
type ADOFixtureResolution struct {
	WorkItem string
	Created  bool
}

// EnsureADOFixture returns the oldest open work item titled ADOFixtureTitle
// and tagged ADOFixtureTag. When none is open (the fixture was closed,
// removed or never seeded) it creates one if cfg.Provision is set, and
// otherwise fails with ErrADOFixtureMissing. Closed or removed fixtures are
// never reopened or edited.
func EnsureADOFixture(ctx context.Context, cfg ADOFixtureConfig) (ADOFixtureResolution, error) {
	provider, repository, err := newADOFixtureProvider(cfg)
	if err != nil {
		return ADOFixtureResolution{}, err
	}
	id, found, err := findOpenADOFixture(ctx, provider, repository)
	if err != nil {
		return ADOFixtureResolution{}, err
	}
	if found {
		return ADOFixtureResolution{WorkItem: id}, nil
	}
	if !cfg.Provision {
		return ADOFixtureResolution{}, fmt.Errorf(
			"%w titled %q and tagged %s: re-run with -provision-fixture to create one",
			ErrADOFixtureMissing, ADOFixtureTitle, ADOFixtureTag)
	}
	itemType := strings.TrimSpace(cfg.WorkItemType)
	if itemType == "" {
		itemType = ADOFixtureDefaultType
	}
	created, err := provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
		Repository: repository,
		Type:       itemType,
		Title:      ADOFixtureTitle,
		Body:       ADOFixtureBody,
		Labels:     []string{ADOFixtureTag},
	})
	if err != nil {
		return ADOFixtureResolution{}, fmt.Errorf("create ADO fixture work item: %w", err)
	}
	return ADOFixtureResolution{WorkItem: created.ID, Created: true}, nil
}

// FindOpenADOFixture returns the work item EnsureADOFixture reads — the
// oldest open work item titled ADOFixtureTitle and tagged ADOFixtureTag —
// and whether one exists. It never creates one. "Open" is decided the way the
// ADO provider decides it for every listing: by each item's state category
// (Completed and Removed are closed), so a process that renames its closed
// states agrees with the drift workflow. cfg.Provision and cfg.WorkItemType
// are ignored.
func FindOpenADOFixture(ctx context.Context, cfg ADOFixtureConfig) (string, bool, error) {
	provider, repository, err := newADOFixtureProvider(cfg)
	if err != nil {
		return "", false, err
	}
	return findOpenADOFixture(ctx, provider, repository)
}

func newADOFixtureProvider(cfg ADOFixtureConfig) (*providers.ADOProvider, providers.RepositoryRef, error) {
	baseURL, organization, err := parseADOOrganizationURL(cfg.OrganizationURL)
	if err != nil {
		return nil, providers.RepositoryRef{}, err
	}
	if strings.TrimSpace(cfg.Project) == "" {
		return nil, providers.RepositoryRef{}, fmt.Errorf("ADO project is required")
	}
	if cfg.Token == "" {
		return nil, providers.RepositoryRef{}, fmt.Errorf("ADO PAT is required")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	provider := providers.NewADOProvider(organization, cfg.Project, cfg.Token, func(p *providers.ADOProvider) {
		p.BaseURL = baseURL
		p.Client = client
	})
	return provider, providers.RepositoryRef{Project: cfg.Project}, nil
}

func findOpenADOFixture(ctx context.Context, provider *providers.ADOProvider, repository providers.RepositoryRef) (string, bool, error) {
	items, err := provider.ListWorkItems(ctx, adoFixtureListRequest(repository))
	if err != nil {
		return "", false, fmt.Errorf("list open ADO fixture work items: %w", err)
	}
	for _, item := range items {
		if item.Title == ADOFixtureTitle {
			return item.ID, true, nil
		}
	}
	return "", false, nil
}
