// Package providerwriter is the one seam through which the cloud control
// role's terminal hooks write to the repository provider (parking labels,
// failure comments, branch delete, abort label, claim-marker release).
//
// Every call is keyed by run ID. The implementation resolves credentials and
// validates the target repository server-side against the run's pinned
// configuration; a caller never supplies a credential or an unvalidated
// repository. Every write is idempotent on retry, so an ambiguous outcome is
// simply retried.
package providerwriter

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/providers"
)

// ErrPending is returned by the default Writer until the control role holds
// provider credentials.
var ErrPending = errors.New("provider writes are not available until #8093")

// ErrRepositoryNotPinned is a permanent refusal: the repository is not one the
// run's pinned configuration declares for its gaggle. Callers must not retry.
var ErrRepositoryNotPinned = errors.New("repository is not pinned for the run")

// ItemRef addresses one work item on behalf of a run.
type ItemRef struct {
	RunID  string
	Repo   providers.RepositoryRef
	ItemID string
}

// Writer is the provider access terminal hooks need. Transient provider or
// credential failures are returned wrapped and are retryable.
type Writer interface {
	WorkItem(ctx context.Context, ref ItemRef) (providers.WorkItem, error)
	Comments(ctx context.Context, ref ItemRef) ([]providers.Comment, error)
	// Update applies req to ref; req.Repository and req.ItemID are set from ref.
	// An Update carrying a Comment is not idempotent on its own: callers must
	// set req.IdempotencyKey on it, or use the Comments + UpdateComment upsert
	// pattern keyed on a body marker. Implementations refuse a Comment with no
	// IdempotencyKey as a permanent error rather than risk a duplicate post.
	Update(ctx context.Context, ref ItemRef, req providers.UpdateWorkItemRequest) error
	UpdateComment(ctx context.Context, ref ItemRef, commentID, body string) error
	// PullLabels and DeleteBranch target the run's WorkspaceRepository.
	PullLabels(ctx context.Context, runID, pullID string) ([]string, error)
	// DeleteBranch treats an already-absent branch as success.
	DeleteBranch(ctx context.Context, runID, branch string) error
	// ReleaseClaimMarker removes the provider-visible claim marker for
	// ref.ItemID held by ref.RunID; an already-absent marker is success. The
	// release is scoped to ref.RunID's claim: if a successor has since claimed
	// the item it fails (permanently) instead of closing the successor's claim.
	ReleaseClaimMarker(ctx context.Context, ref ItemRef, reason claimrelease.Reason) error
}

// Pending is the Writer used until #8093 composes a real one: every call
// reports ErrPending.
type Pending struct{}

var _ Writer = Pending{}

func pending(op string) error { return fmt.Errorf("%s: %w", op, ErrPending) }

// WorkItem reports ErrPending.
func (Pending) WorkItem(context.Context, ItemRef) (providers.WorkItem, error) {
	return providers.WorkItem{}, pending("get work item")
}

// Comments reports ErrPending.
func (Pending) Comments(context.Context, ItemRef) ([]providers.Comment, error) {
	return nil, pending("list comments")
}

// Update reports ErrPending.
func (Pending) Update(context.Context, ItemRef, providers.UpdateWorkItemRequest) error {
	return pending("update work item")
}

// UpdateComment reports ErrPending.
func (Pending) UpdateComment(context.Context, ItemRef, string, string) error {
	return pending("update comment")
}

// PullLabels reports ErrPending.
func (Pending) PullLabels(context.Context, string, string) ([]string, error) {
	return nil, pending("list pull labels")
}

// DeleteBranch reports ErrPending.
func (Pending) DeleteBranch(context.Context, string, string) error { return pending("delete branch") }

// ReleaseClaimMarker reports ErrPending.
func (Pending) ReleaseClaimMarker(context.Context, ItemRef, claimrelease.Reason) error {
	return pending("release claim marker")
}

// ErrPermanent marks a refusal that retrying cannot fix (an unkeyed comment
// update, a claim released by a successor, ...). Implementations wrap it.
var ErrPermanent = errors.New("permanent provider-writer refusal")

// Permanent reports whether err should not be retried.
func Permanent(err error) bool {
	return errors.Is(err, ErrRepositoryNotPinned) || errors.Is(err, ErrPermanent)
}
