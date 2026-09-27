package providers

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupportedCreateGraphFields refuses a work-item creation that declares
// graph edges the create operation does not publish (#5245).
//
// CreateWorkItemRequest carries Parent and Links, and NO provider
// implementation has ever acted on either one — not ADO, not GitHub, not
// Gitea. A caller that set them got a created item with none of its declared
// edges and no indication anything had been dropped, which is worse than a
// refusal: the item exists, so a retry produces a duplicate rather than the
// missing edges.
//
// This is deliberately a refusal rather than an implementation. Publishing a
// work-item GRAPH needs independent idempotency keys for nodes and edges,
// batch preflight, persisted intent and reconciliation of uncertain effects —
// the governed publisher #5245 scopes for 0.5. Its own release guidance is
// that 0.4.1 may "clarify/refuse unsupported behavior", which is what this
// does; #2179's requirement that divergence be explicit rather than a silent
// asymmetry is the same rule applied to a gap that turned out to be universal.
var ErrUnsupportedCreateGraphFields = errors.New("work-item creation does not publish graph edges")

// checkCreateWorkItemGraphFields refuses before any mutation.
//
// Ordered before the POST on purpose: #5245 requires that forbidden targets
// and fields fail BEFORE mutation, because the failure mode being fixed is
// precisely an item that got created while its declared edges did not.
//
// The message names the operations that do work, so the refusal is actionable:
// AttachWorkItemChild and AttachWorkItemBlocker publish parent/child and
// blocked-by edges as separate, revision-guarded mutations after creation.
func checkCreateWorkItemGraphFields(req CreateWorkItemRequest) error {
	var declared []string
	if req.Parent != nil && strings.TrimSpace(req.Parent.ID) != "" {
		declared = append(declared, "parent")
	}
	if len(req.Links) > 0 {
		declared = append(declared, "links")
	}
	if len(declared) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%w: %s declared on this create and would be silently dropped. "+
			"Create the item, then publish each edge with AttachWorkItemChild (parent/child) or "+
			"AttachWorkItemBlocker (blocked-by), which guard on the revisions they observed. "+
			"Governed single-call graph publication is tracked by #5245",
		ErrUnsupportedCreateGraphFields, strings.Join(declared, " and "))
}
