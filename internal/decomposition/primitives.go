package decomposition

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

const actionMarkerPrefix = "<!-- goobers-action:v1 "

// TargetLeaser serializes mutations for one provider/repository/item target.
type TargetLeaser interface {
	Acquire(context.Context, providers.RepositoryRef, string) (func() error, error)
}

// WorkItemMutationProvider is the provider-neutral surface used by Primitives.
type WorkItemMutationProvider interface {
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
	// FindWorkItemsByMarker must inspect the provider's authoritative item
	// listing, not an eventually-consistent full-text search index.
	FindWorkItemsByMarker(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error)
	CreateWorkItemComment(context.Context, providers.RepositoryRef, string, string) (providers.Comment, error)
}

// WorkItemHierarchyProvider is the optional native parent/child surface.
type WorkItemHierarchyProvider interface {
	ListWorkItemChildren(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error)
	AttachWorkItemChild(context.Context, providers.AttachWorkItemChildRequest) error
}

// Primitives performs retry-safe decomposition mutations under a shared target
// lease. Stable action keys are distinct per child or marker comment.
type Primitives struct {
	Provider WorkItemMutationProvider
	Leaser   TargetLeaser
}

// CreateChildRequest describes one idempotent child issue creation.
type CreateChildRequest struct {
	ParentID               string
	ExpectedParentRevision string
	IdempotencyKey         string
	Item                   providers.CreateWorkItemRequest
}

// MarkerCommentRequest describes one idempotent marker comment append.
type MarkerCommentRequest struct {
	ItemID           string
	ExpectedRevision string
	IdempotencyKey   string
	Body             string
}

// MarkerConflictError means a stable key already names different content or
// appears on more than one provider object.
type MarkerConflictError struct {
	Key    string
	Reason string
}

func (e *MarkerConflictError) Error() string {
	return fmt.Sprintf("idempotency marker %q conflicts: %s", e.Key, e.Reason)
}

// CreateChild creates or adopts exactly one child for a stable action key.
func (p Primitives) CreateChild(ctx context.Context, req CreateChildRequest) (providers.WorkItem, error) {
	if err := p.validate(); err != nil {
		return providers.WorkItem{}, err
	}
	if req.ParentID == "" {
		return providers.WorkItem{}, fmt.Errorf("parent id is required")
	}
	if req.Item.Repository.Name == "" {
		return providers.WorkItem{}, fmt.Errorf("repository is required")
	}
	payloadRequest := req.Item
	payloadRequest.RunID = ""
	payload, err := json.Marshal(payloadRequest)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("marshal child request: %w", err)
	}
	marker, body, err := markedBody(req.IdempotencyKey, req.Item.Body, payload)
	if err != nil {
		return providers.WorkItem{}, err
	}
	release, err := p.Leaser.Acquire(ctx, req.Item.Repository, req.ParentID)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("acquire target lease: %w", err)
	}
	defer func() { _ = release() }()

	if existing, found, err := p.findChild(ctx, req.Item.Repository, marker, req.IdempotencyKey); err != nil || found {
		return existing, err
	}
	if _, err := p.guardedItem(ctx, req.Item.Repository, req.ParentID, req.ExpectedParentRevision); err != nil {
		return providers.WorkItem{}, err
	}
	req.Item.Body = body
	req.Item.RunID = ""
	child, createErr := p.Provider.CreateWorkItem(ctx, req.Item)
	if createErr == nil {
		return child, nil
	}
	existing, found, lookupErr := p.findChild(ctx, req.Item.Repository, marker, req.IdempotencyKey)
	if lookupErr != nil {
		return providers.WorkItem{}, errors.Join(createErr, lookupErr)
	}
	if found {
		return existing, nil
	}
	return providers.WorkItem{}, createErr
}

// AppendMarkerComment appends or adopts exactly one comment for a stable key.
func (p Primitives) AppendMarkerComment(ctx context.Context, repo providers.RepositoryRef, req MarkerCommentRequest) (providers.Comment, error) {
	if err := p.validate(); err != nil {
		return providers.Comment{}, err
	}
	marker, body, err := markedBody(req.IdempotencyKey, req.Body, []byte(req.Body))
	if err != nil {
		return providers.Comment{}, err
	}
	release, err := p.Leaser.Acquire(ctx, repo, req.ItemID)
	if err != nil {
		return providers.Comment{}, fmt.Errorf("acquire target lease: %w", err)
	}
	defer func() { _ = release() }()

	if existing, found, err := p.findComment(ctx, repo, req.ItemID, marker, req.IdempotencyKey); err != nil || found {
		return existing, err
	}
	if _, err := p.guardedItem(ctx, repo, req.ItemID, req.ExpectedRevision); err != nil {
		return providers.Comment{}, err
	}
	comment, createErr := p.Provider.CreateWorkItemComment(ctx, repo, req.ItemID, body)
	if createErr == nil {
		return comment, nil
	}
	existing, found, lookupErr := p.findComment(ctx, repo, req.ItemID, marker, req.IdempotencyKey)
	if lookupErr != nil {
		return providers.Comment{}, errors.Join(createErr, lookupErr)
	}
	if found {
		return existing, nil
	}
	return providers.Comment{}, createErr
}

// AttachChild idempotently attaches a child through the provider's native
// hierarchy API, when that provider implements the decomposition surface.
func (p Primitives) AttachChild(ctx context.Context, repo providers.RepositoryRef, req providers.AttachWorkItemChildRequest) error {
	if err := p.validate(); err != nil {
		return err
	}
	hierarchy, ok := p.Provider.(WorkItemHierarchyProvider)
	if !ok {
		return fmt.Errorf("provider %q does not support native work item hierarchy", repo.Provider)
	}
	release, err := p.Leaser.Acquire(ctx, repo, req.ParentID)
	if err != nil {
		return fmt.Errorf("acquire target lease: %w", err)
	}
	defer func() { _ = release() }()

	children, err := hierarchy.ListWorkItemChildren(ctx, repo, req.ParentID)
	if err != nil {
		return err
	}
	for _, existing := range children {
		if existing.ID == req.ChildID {
			return nil
		}
	}
	parent, err := p.guardedItem(ctx, repo, req.ParentID, req.ExpectedParentRevision)
	if err != nil {
		return err
	}
	child, err := p.guardedItem(ctx, repo, req.ChildID, req.ExpectedChildRevision)
	if err != nil {
		return err
	}
	req.Repository = repo
	req.ExpectedParentRevision = parent.Revision
	req.ExpectedChildRevision = child.Revision
	return hierarchy.AttachWorkItemChild(ctx, req)
}

func (p Primitives) validate() error {
	if p.Provider == nil || p.Leaser == nil {
		return fmt.Errorf("provider and target leaser are required")
	}
	return nil
}

func (p Primitives) guardedItem(ctx context.Context, repo providers.RepositoryRef, id, expected string) (providers.WorkItem, error) {
	if expected == "" {
		return providers.WorkItem{}, fmt.Errorf("expected revision is required for work item %s", id)
	}
	item, err := p.Provider.GetWorkItem(ctx, repo, id)
	if err != nil {
		return providers.WorkItem{}, err
	}
	if item.Revision != expected {
		return providers.WorkItem{}, &providers.RevisionConflictError{ItemID: id, Expected: expected, Actual: item.Revision}
	}
	return item, nil
}

func (p Primitives) findChild(ctx context.Context, repo providers.RepositoryRef, marker, key string) (providers.WorkItem, bool, error) {
	items, err := p.Provider.FindWorkItemsByMarker(ctx, repo, markerKeyLine(key))
	if err != nil {
		return providers.WorkItem{}, false, err
	}
	var exact []providers.WorkItem
	for _, item := range items {
		if containsLine(item.Body, marker) {
			exact = append(exact, item)
		} else {
			return providers.WorkItem{}, false, &MarkerConflictError{Key: key, Reason: "existing child has different content digest"}
		}
	}
	if len(exact) > 1 {
		return providers.WorkItem{}, false, &MarkerConflictError{Key: key, Reason: "marker appears on multiple children"}
	}
	if len(exact) == 1 {
		return exact[0], true, nil
	}
	return providers.WorkItem{}, false, nil
}

func (p Primitives) findComment(ctx context.Context, repo providers.RepositoryRef, id, marker, key string) (providers.Comment, bool, error) {
	comments, err := p.Provider.ListComments(ctx, repo, id)
	if err != nil {
		return providers.Comment{}, false, err
	}
	var exact []providers.Comment
	for _, comment := range comments {
		if !containsLine(comment.Body, markerKeyLine(key)) {
			continue
		}
		if !containsLine(comment.Body, marker) {
			return providers.Comment{}, false, &MarkerConflictError{Key: key, Reason: "existing comment has different content digest"}
		}
		exact = append(exact, comment)
	}
	if len(exact) > 1 {
		return providers.Comment{}, false, &MarkerConflictError{Key: key, Reason: "marker appears on multiple comments"}
	}
	if len(exact) == 1 {
		return exact[0], true, nil
	}
	return providers.Comment{}, false, nil
}

func markedBody(key, body string, payload []byte) (string, string, error) {
	if strings.TrimSpace(key) == "" {
		return "", "", fmt.Errorf("idempotency key is required")
	}
	sum := sha256.Sum256(payload)
	marker := fmt.Sprintf("<!-- goobers-action-digest:v1 sha256:%s -->", hex.EncodeToString(sum[:]))
	markers := markerKeyLine(key) + "\n" + marker
	if body == "" {
		return marker, markers, nil
	}
	return marker, body + "\n\n" + markers, nil
}

func markerKeyLine(key string) string {
	return actionMarkerPrefix + "key=" + base64.RawURLEncoding.EncodeToString([]byte(key)) + " -->"
}

func containsLine(body, line string) bool {
	for _, candidate := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if candidate == line {
			return true
		}
	}
	return false
}
