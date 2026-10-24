// Package providerposter adapts the cloud provider-writer seam to the provider
// interfaces the shared terminal-hook policies speak (gate.Commenter and
// parking.Poster), for one run.
package providerposter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// Poster issues every call on behalf of RunID, so the writer resolves the
// repository's credentials from that run's pinned configuration.
type Poster struct {
	Writer providerwriter.Writer
	RunID  string
}

func (p Poster) ref(repo providers.RepositoryRef, itemID string) providerwriter.ItemRef {
	return providerwriter.ItemRef{RunID: p.RunID, Repo: repo, ItemID: itemID}
}

// ListComments lists the comments on itemID.
func (p Poster) ListComments(ctx context.Context, repo providers.RepositoryRef, itemID string) ([]providers.Comment, error) {
	return p.Writer.Comments(ctx, p.ref(repo, itemID))
}

// GetWorkItem reads itemID.
func (p Poster) GetWorkItem(ctx context.Context, repo providers.RepositoryRef, itemID string) (providers.WorkItem, error) {
	return p.Writer.WorkItem(ctx, p.ref(repo, itemID))
}

// UpdateComment edits an existing comment.
func (p Poster) UpdateComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error {
	return p.Writer.UpdateComment(ctx, p.ref(repo, ""), commentID, body)
}

// UpdateWorkItem applies req. A request carrying a comment is made idempotent
// across retries and successor leaders: unless the caller keyed it, the key is
// derived from the run, the item and the comment text.
func (p Poster) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if req.Comment != "" && req.IdempotencyKey == "" {
		req.IdempotencyKey = IdempotencyKey(p.RunID, req.ID, req.Comment)
	}
	return providers.WorkItem{}, p.Writer.Update(ctx, p.ref(req.Repository, req.ID), req)
}

// IdempotencyKey derives the key for a comment a run posts on an item.
func IdempotencyKey(runID, itemID, comment string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + itemID + "\x00" + comment))
	return "terminal/" + runID + "/" + itemID + "/" + hex.EncodeToString(sum[:8])
}
