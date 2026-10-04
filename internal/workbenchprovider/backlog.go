// Package workbenchprovider projects bounded native source windows. It holds no
// credentials, cache or durable state. Its caller must authorize each operation
// and supply a provider whose HTTP client is scoped to that same authority.
package workbenchprovider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

// Projection read limits apply to every provider call made by this package.
const (
	MaxReadDuration  = 15 * time.Second
	MaxResponseBytes = 2 << 20
	DefaultPageItems = 50
)

// Projection errors never include native source bodies or credentials.
var (
	ErrInvalidSource   = errors.New("workbenchprovider: invalid bound backlog source")
	ErrInvalidCursor   = errors.New("workbenchprovider: invalid or foreign backlog cursor")
	ErrInvalidItem     = errors.New("workbenchprovider: invalid native item projection")
	ErrItemTooLarge    = errors.New("workbenchprovider: item exceeds projection bound")
	ErrIdentityChanged = errors.New("workbenchprovider: locator no longer identifies expected source object")
)

// BacklogClient is an already authorized provider. Constructors never resolve
// credentials or fall back to an automation identity.
type BacklogClient interface {
	Kind() providers.ProviderKind
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
}

// BacklogReader owns only a copied target/selector and an injected client. It is
// safe to discard after each authorized read; its cursor is not an access token.
type BacklogReader struct {
	client                        BacklogClient
	repository                    providers.RepositoryRef
	gaggle, binding, targetDigest string
	objectives                    apiv1.WorkbenchObjectiveSelector
}

// NewBacklogReader copies one configured source and an already authorized client.
func NewBacklogReader(scope workbench.Scope, source workbench.BoundSource, client BacklogClient) (*BacklogReader, error) {
	if scope.Validate() != nil || !scope.Bindings[source.Spec.Name] || source.Spec.Kind != "backlog" || client == nil || source.Backlog.BaseURL != "" {
		return nil, ErrInvalidSource
	}
	target := source.BacklogIdentity
	if !validBacklogTarget(source, target) || string(client.Kind()) != string(target.Provider) {
		return nil, ErrInvalidSource
	}
	r := &BacklogReader{client: client, gaggle: scope.GaggleID, binding: source.Spec.Name,
		repository: providers.RepositoryRef{Provider: client.Kind(), Owner: target.Owner, Project: target.Project, Name: target.Name}}
	if source.Spec.Objectives != nil {
		r.objectives.IDs = append([]string(nil), source.Spec.Objectives.IDs...)
		r.objectives.Types = append([]string(nil), source.Spec.Objectives.Types...)
		r.objectives.Labels = append([]string(nil), source.Spec.Objectives.Labels...)
	}
	r.targetDigest, _ = workbench.SourceTargetDigest(scope, source)
	return r, nil
}

func validBacklogTarget(source workbench.BoundSource, target apiv1.InteractiveRepositoryIdentity) bool {
	if source.Backlog.Provider != target.Provider || !component(target.Owner) {
		return false
	}
	switch target.Provider {
	case apiv1.ProviderGitHub:
		return target.Project == "" && component(target.Name) && source.Backlog.Project == target.Owner+"/"+target.Name
	case apiv1.ProviderADO:
		return target.Name == "" && component(target.Project) && source.Backlog.Project == target.Project
	default:
		return false
	}
}

// Page performs one bounded native provider window. It never follows a cursor,
// expands relationship targets, or applies scheduler filters to ordinary browse.
func (r *BacklogReader) Page(ctx context.Context, request workbench.BacklogPageRequest) (workbench.BacklogPage, error) {
	limit, cursor, err := r.parseCursor(request)
	if err != nil {
		return workbench.BacklogPage{}, err
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	info := &providers.ListWorkItemsPageInfo{CandidateCount: -1}
	items, err := r.client.ListWorkItems(ctx, providers.ListWorkItemsRequest{Repository: r.repository, State: "all", Limit: limit, Cursor: cursor, PageInfo: info, OldestFirst: true})
	if err != nil {
		return workbench.BacklogPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return workbench.BacklogPage{}, err
	}
	if len(items) > limit || info.CandidateCount < len(items) || info.CandidateCount > limit || (info.HasNext && !advancingCursor(cursor, info.NextCursor)) {
		return workbench.BacklogPage{}, fmt.Errorf("%w: provider violated bounded page contract", ErrInvalidItem)
	}
	page := workbench.BacklogPage{Items: make([]workbench.BacklogItem, 0, len(items)), SourceTargetDigest: r.targetDigest, Candidates: info.CandidateCount, Omitted: info.CandidateCount - len(items), Exhausted: !info.HasNext}
	if page.Omitted > 0 {
		page.Reasons = append(page.Reasons, "provider-candidates-not-returned")
	}
	r.projectPage(items, &page)
	if info.HasNext {
		page.NextCursor = r.encodeCursor(limit, info.NextCursor)
		page.Reasons = append(page.Reasons, "more-candidates")
	}
	page.Partial = len(page.Reasons) > 0
	return page, nil
}

func (r *BacklogReader) projectPage(items []providers.WorkItem, page *workbench.BacklogPage) {
	used := 0
	seen := map[string]bool{}
	for _, item := range items {
		projected, size, err := r.project(item)
		reason := ""
		switch {
		case errors.Is(err, ErrItemTooLarge):
			reason = "oversized-item"
		case err != nil:
			reason = "invalid-item"
		case seen[projected.Ref.Key()]:
			reason = "duplicate-item"
		case used+size > workbench.MaxBacklogPageBytes-4096:
			reason = "page-byte-limit"
		}
		if reason != "" {
			page.Omitted++
			appendReason(page, reason)
			continue
		}
		seen[projected.Ref.Key()] = true
		used += size
		page.Items = append(page.Items, projected)
	}
}

// Get reads one native locator, optionally checking the expected stable identity.
// ADO lookups additionally verify System.TeamProject because its REST lookup is
// organization-scoped even when the requested URI includes a project.
func (r *BacklogReader) Get(ctx context.Context, request workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
	if !positiveID(request.ID) || (request.ExpectedSourceID != "" && !positiveID(request.ExpectedSourceID)) {
		return workbench.BacklogItem{}, ErrInvalidItem
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	item, err := r.client.GetWorkItem(ctx, r.repository, request.ID)
	if err != nil {
		return workbench.BacklogItem{}, err
	}
	if err := ctx.Err(); err != nil {
		return workbench.BacklogItem{}, err
	}
	if item.ID != request.ID {
		return workbench.BacklogItem{}, ErrIdentityChanged
	}
	result, _, err := r.project(item)
	if err != nil {
		return workbench.BacklogItem{}, err
	}
	if request.ExpectedSourceID != "" && result.Ref.SourceID != request.ExpectedSourceID {
		return workbench.BacklogItem{}, ErrIdentityChanged
	}
	return result, nil
}

func boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(providers.WithResponseBodyLimit(ctx, MaxResponseBytes), MaxReadDuration)
}

func positiveID(value string) bool {
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == value
}

func appendReason(page *workbench.BacklogPage, reason string) {
	for _, existing := range page.Reasons {
		if existing == reason {
			return
		}
	}
	page.Reasons = append(page.Reasons, reason)
}
