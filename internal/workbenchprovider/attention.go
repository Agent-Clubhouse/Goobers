package workbenchprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

// AttentionClient exposes a distinct control-marker operation. Generic field
// editing never gains control-label authority through this interface.
type AttentionClient interface {
	BacklogClient
	providers.NeedsHumanInspector
	providers.NeedsHumanClearer
}

// AttentionResolver projects exact-source bounded evidence and a single narrow
// marker attempt. The host owns current permission, claims lock and receipts.
type AttentionResolver struct {
	reader *BacklogReader
	client AttentionClient
}

// NewAttentionResolver requires the source's labels allowlist independently of
// the host's distinct backlog.resolve permission.
func NewAttentionResolver(scope workbench.Scope, source workbench.BoundSource, client AttentionClient) (*AttentionResolver, error) {
	if !NeedsHumanResolutionAllowed(source) {
		return nil, ErrUnsupportedEdit
	}
	reader, err := NewBacklogReader(scope, source, client)
	if err != nil {
		return nil, err
	}
	return &AttentionResolver{reader, client}, nil
}

// NeedsHumanResolutionAllowed is a configuration check, not authorization.
func NeedsHumanResolutionAllowed(source workbench.BoundSource) bool {
	return slices.Contains(BacklogCapabilities(source).Fields, "labels")
}

// NeedsHumanOperationDigest binds the physical target and exact assessment.
func NeedsHumanOperationDigest(scope workbench.Scope, source workbench.BoundSource, request workbench.NeedsHumanResolutionRequest) (string, error) {
	if !NeedsHumanResolutionAllowed(source) || !positiveID(request.ID) || !positiveID(request.SourceID) || request.ExpectedRevision == "" {
		return "", ErrUnsupportedEdit
	}
	target, err := workbench.BacklogMutationTargetDigest(scope, source)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Kind, Target string
		Request      workbench.NeedsHumanResolutionRequest
	}{"needs-human-clear-v1", target, request})
	if err != nil || len(raw) > 32<<10 {
		return "", ErrInvalidItem
	}
	return attentionDigest(raw), nil
}

// Inspect checks one native page and at most64 exact-target learned blockers.
// Every unreadable, foreign or unknown dependency remains explicitly unresolved.
func (r *AttentionResolver) Inspect(ctx context.Context, request workbench.BacklogItemRequest, learned []string) (workbench.NeedsHumanObservation, error) {
	var result workbench.NeedsHumanObservation
	if !positiveID(request.ID) || !positiveID(request.ExpectedSourceID) {
		return result, ErrInvalidItem
	}
	inspection, err := r.client.InspectNeedsHuman(ctx, r.reader.repository, request.ID)
	if err != nil {
		return result, err
	}
	result.Item, _, err = r.reader.project(inspection.Item)
	if err != nil {
		return result, err
	}
	if result.Item.Locator.ID != request.ID || result.Item.Ref.SourceID != request.ExpectedSourceID {
		return result, ErrIdentityChanged
	}
	result.MarkerPresent = inspection.Item.HasLabel(providers.LabelNeedsHuman)
	result.Comments = make([]workbench.NeedsHumanComment, 0, len(inspection.Comments))
	result.CommentsComplete = inspection.CommentsComplete
	seen := map[string]bool{}
	for _, comment := range inspection.Comments {
		projected := workbench.NeedsHumanComment{ID: comment.ID, Author: comment.Author, Text: comment.Body, CreatedAt: comment.CreatedAt}
		raw, _ := json.Marshal(projected)
		projected.Digest = attentionDigest(raw)
		result.Comments = append(result.Comments, projected)
		if seen[comment.ID] {
			result.CommentsComplete = false
		}
		seen[comment.ID] = true
	}
	result.Dependencies = make([]workbench.NeedsHumanDependency, 0, len(inspection.Blockers))
	for _, dependency := range inspection.Blockers {
		result.Dependencies = append(result.Dependencies, workbench.NeedsHumanDependency{ID: dependency.ID, SourceID: dependency.StableID, Revision: dependency.Revision, Open: dependency.Open, Verified: dependency.Verified})
	}
	result.DependenciesComplete = inspection.BlockersComplete
	result.LearnedDependencies, result.LearnedComplete = r.learned(ctx, learned)
	return result, nil
}
func (r *AttentionResolver) learned(ctx context.Context, ids []string) ([]workbench.NeedsHumanDependency, bool) {
	complete := len(ids) <= providers.MaxAttentionBlockers
	if !complete {
		ids = ids[:providers.MaxAttentionBlockers]
	}
	result := make([]workbench.NeedsHumanDependency, 0, len(ids))
	for _, id := range ids {
		dependency := workbench.NeedsHumanDependency{ID: id, Open: true}
		if positiveID(id) {
			item, err := r.client.GetWorkItem(ctx, r.reader.repository, id)
			if err == nil {
				projected, _, err := r.reader.project(item)
				if err == nil && projected.Locator.ID == id && (item.State == "open" || item.State == "closed") {
					dependency.SourceID = projected.Ref.SourceID
					dependency.Revision = projected.Revision
					dependency.Open = item.State != "closed"
					dependency.Verified = true
				}
			}
		}
		complete = complete && dependency.Verified
		result = append(result, dependency)
	}
	return result, complete
}

// Clear sends at most one provider mutation and one fresh read. A matching read
// without provider acknowledgement remains unknown and is never replayed.
func (r *AttentionResolver) Clear(ctx context.Context, request workbench.NeedsHumanResolutionRequest, operation string) (workbench.NeedsHumanResolutionReceipt, error) {
	receipt := workbench.NeedsHumanResolutionReceipt{OperationDigest: operation, Outcome: "not-applied", RevisionSemantics: backlogCapabilities(r.reader.repository.Provider, nil).RevisionSemantics}
	result, err := r.client.ClearNeedsHuman(ctx, providers.NeedsHumanClearRequest{Repository: r.reader.repository, ID: request.ID, StableID: request.SourceID, ExpectedRevision: request.ExpectedRevision})
	if !result.MutationAttempted && !result.Acknowledged {
		return receipt, err
	}
	receipt.Outcome = "unknown"
	receipt.ProviderAcknowledged = result.Acknowledged
	observed, readErr := r.reader.Get(ctx, workbench.BacklogItemRequest{ID: request.ID, ExpectedSourceID: request.SourceID})
	if readErr == nil {
		receipt.Observed = &observed
		receipt.ObservedClear = !attentionMarker(observed.Labels, r.reader.repository.Provider)
	}
	if err == nil && readErr == nil && result.Acknowledged && receipt.ObservedClear {
		receipt.Outcome = "confirmed"
		return receipt, nil
	}
	return receipt, errors.Join(ErrUnverifiedEdit, err, readErr)
}
func attentionMarker(labels []string, provider providers.ProviderKind) bool {
	for _, label := range labels {
		if label == providers.LabelNeedsHuman || (provider == providers.ProviderADO && strings.EqualFold(label, providers.LabelNeedsHuman)) {
			return true
		}
	}
	return false
}
func attentionDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
