package workbench

import "time"

const (
	// MaxBacklogPageItems bounds one caller-driven provider window.
	MaxBacklogPageItems = 100
	// MaxBacklogCursorBytes bounds opaque continuation input.
	MaxBacklogCursorBytes = 2048
	// MaxBacklogItemBytes and MaxBacklogPageBytes bound projected source data.
	MaxBacklogItemBytes = 256 << 10
	// MaxBacklogPageBytes includes the projection envelope and cursor.
	MaxBacklogPageBytes = 1 << 20
)

// BacklogPageRequest requests one provider window, never an automatic scan.
// A cursor is tied to the configured source target and the original page size.
type BacklogPageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// BacklogItemRequest uses the current native locator (GH issue number, ADO ID).
// ExpectedSourceID optionally detects a moved/replaced locator. Reads never
// treat a human number as the immutable identity of a GitHub issue.
type BacklogItemRequest struct {
	ID               string `json:"id"`
	ExpectedSourceID string `json:"expectedSourceId,omitempty"`
}

// SourceLocator is navigation metadata, not a persistent node identity.
type SourceLocator struct {
	ID  string `json:"id"`
	URL string `json:"url,omitempty"`
}

// BacklogItem is a bounded native source projection. Description excludes a
// separate ADO acceptance-criteria field. Revision remains provider-native;
// GitHub timestamps are preflight checks, not server-side compare-and-swap.
type BacklogItem struct {
	Ref                  NodeRef              `json:"ref"`
	Locator              SourceLocator        `json:"locator"`
	Revision             string               `json:"revision,omitempty"`
	RevisionSemantics    string               `json:"revisionSemantics"`
	Type                 string               `json:"type"`
	Title                string               `json:"title"`
	Description          string               `json:"description,omitempty"`
	AcceptanceCriteria   string               `json:"acceptanceCriteria,omitempty"`
	State                string               `json:"state"`
	Labels               []string             `json:"labels,omitempty"`
	Assignees            []string             `json:"assignees,omitempty"`
	Objective            bool                 `json:"objective"`
	UpdatedAt            *time.Time           `json:"updatedAt,omitempty"`
	Relationships        []NativeRelationship `json:"relationships,omitempty"`
	RelationshipCoverage RelationshipCoverage `json:"relationshipCoverage"`
}

// NativeRelationship preserves native direction. Incoming parent-of means the
// target is this item's parent. An unresolved target carries only source data;
// it grants no visibility and must not become a scoped graph edge until its
// membership in the configured source has been verified.
type NativeRelationship struct {
	Kind     string       `json:"kind"`
	Incoming bool         `json:"incoming,omitempty"`
	Target   NativeTarget `json:"target"`
}

// NativeTarget distinguishes a verified scoped node from an unresolved native link.
type NativeTarget struct {
	Ref      *NodeRef      `json:"ref,omitempty"`
	Kind     string        `json:"kind"`
	StableID string        `json:"stableId,omitempty"`
	Locator  SourceLocator `json:"locator"`
}

// RelationshipCoverage uses complete, partial, not-loaded or unsupported.
// Complete refers to relation enumeration, never linked-content authorization.
type RelationshipCoverage struct {
	Parents    string `json:"parents"`
	Blockers   string `json:"blockers"`
	Milestones string `json:"milestones"`
}

// BacklogPage reports one non-snapshot provider window. Exhausted means this
// window found no further candidates; it cannot prove a consistent full scan
// while the provider changes. Omitted includes mixed PR candidates (GH), missing
// hydration (ADO), invalid identities and oversized source data. Partial is true
// whenever more candidates or omissions remain, with machine-readable reasons.
type BacklogPage struct {
	Items              []BacklogItem `json:"items"`
	NextCursor         string        `json:"nextCursor,omitempty"`
	Exhausted          bool          `json:"exhausted"`
	Partial            bool          `json:"partial"`
	Reasons            []string      `json:"reasons,omitempty"`
	Candidates         int           `json:"candidates"`
	Omitted            int           `json:"omitted"`
	SourceTargetDigest string        `json:"sourceTargetDigest"`
}
