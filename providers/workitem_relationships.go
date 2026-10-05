package providers

import "context"

// MaxSelectedRelationshipTargets bounds explicit selected-item expansion.
const MaxSelectedRelationshipTargets = 65

// WorkItemRelationshipReader is optional and never used by inventory or mutation
// preflight. The item must already have been read through the supplied scope.
type WorkItemRelationshipReader interface {
	ReadWorkItemRelationships(context.Context, RepositoryRef, WorkItem) (WorkItemRelationships, error)
}

// WorkItemRelationTarget carries native evidence. Verified means the provider
// proved this target belongs to the exact supplied source, not merely its org.
type WorkItemRelationTarget struct {
	ID, StableID, URL string
	Verified          bool
}

// WorkItemRelationships preserves hierarchy and dependency semantics separately.
// Incomplete covers unreadable, foreign, invalid or truncated targets.
type WorkItemRelationships struct {
	Parents, Blockers                 []WorkItemRelationTarget
	ParentsComplete, BlockersComplete bool
}
