package providers

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Attention inspection bounds are independent of generic unbounded inventory.
const (
	MaxAttentionComments      = 100
	MaxAttentionBlockers      = 64
	MaxAttentionResponseBytes = 1 << 20
	MaxAttentionDuration      = 5 * time.Second
)

// NeedsHumanInspector reads exactly one configured target. Incomplete coverage
// is retained explicitly and can never justify clearing a marker.
type NeedsHumanInspector interface {
	InspectNeedsHuman(context.Context, RepositoryRef, string) (NeedsHumanInspection, error)
}

// NeedsHumanClearer removes exactly the needs-human marker. It grants no
// authority itself; a host must first retain a current authorized assessment.
type NeedsHumanClearer interface {
	ClearNeedsHuman(context.Context, NeedsHumanClearRequest) (NativeWorkItemPatchResult, error)
}

// NeedsHumanInspection is source evidence, not an assessment of the answer.
// Comments never confer authority; a host verifies human session evidence.
type NeedsHumanInspection struct {
	Item             WorkItem
	Comments         []Comment
	Blockers         []AttentionBlocker
	CommentsComplete bool
	BlockersComplete bool
}

// AttentionBlocker contains only source-scoped native state. Unverified means
// unreadable, foreign or ambiguous; it must continue blocking resolution.
type AttentionBlocker struct {
	ID       string
	StableID string
	Revision string
	Open     bool
	Verified bool
}

// NeedsHumanClearRequest binds the immutable source identity and current native
// revision. It accepts neither an arbitrary label nor a replacement label set.
type NeedsHumanClearRequest struct {
	Repository                     RepositoryRef
	ID, StableID, ExpectedRevision string
}

// ErrAttentionChanged reports absent or changed source custody before mutation.
var ErrAttentionChanged = errors.New("provider: needs-human observation changed or is incomplete")

func attentionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithTimeout(ctx, MaxAttentionDuration)
	return WithResponseBodyLimit(bounded, MaxAttentionResponseBytes), cancel
}
func validateAttentionRequest(request NeedsHumanClearRequest) error {
	if !nativePositiveID(request.ID) || !nativePositiveID(request.StableID) || !nativeEditText(request.ExpectedRevision, 256, false) {
		return ErrAttentionChanged
	}
	return nil
}
func validateAttentionIdentity(item WorkItem, request NeedsHumanClearRequest) error {
	if err := validateAttentionRequest(request); err != nil {
		return err
	}
	if item.ID != request.ID || item.StableID != request.StableID || !item.HasLabel(LabelNeedsHuman) {
		return ErrAttentionChanged
	}
	return checkWorkItemRevision(item, request.ExpectedRevision)
}
func attentionCommentBounds(comments []Comment) bool {
	bytes := 0
	for _, comment := range comments {
		bytes += len(comment.Body) + len(comment.Author) + len(comment.URL)
		if len(comment.Body) > 64<<10 || bytes > 512<<10 || comment.ID == "" {
			return false
		}
	}
	return len(comments) <= MaxAttentionComments
}
func attentionOpenState(state string) (bool, bool) {
	switch strings.ToLower(state) {
	case "open":
		return true, true
	case "closed":
		return false, true
	default:
		return true, false
	}
}
