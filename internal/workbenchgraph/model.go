// Package workbenchgraph projects already-authorized source adapter snapshots.
// It performs no I/O, retains no planning truth, and grants no read/write authority.
// Callers must never populate Snapshot from an untrusted client-authored graph.
package workbenchgraph

import (
	"errors"

	"github.com/goobers/goobers/internal/workbench"
)

// Projection bounds limit transient input and output; no graph is retained.
const (
	MaxSnapshotBytes = 32 << 20
	MaxPages         = 512
	MaxNodes         = 10000
	MaxEdges         = 50000
)

var (
	// ErrInvalidSnapshot refuses mixed pins, invalid scope, or adapter metadata.
	ErrInvalidSnapshot = errors.New("workbenchgraph: invalid or incoherent authorized snapshot")
	// ErrProjectionBound refuses work above a named projection limit.
	ErrProjectionBound = errors.New("workbenchgraph: projection exceeds a named bound")
)

// Snapshot contains only trusted adapter output under one current SourceSet.
// Authorization remains the caller's responsibility. Multiple repository pages
// must describe the same commit and configured target; native pages are never
// promoted to an atomic provider snapshot, even when a page reports exhaustion.
type Snapshot struct {
	Sources   workbench.SourceSet
	Backlogs  []BacklogWindow
	Documents []workbench.DocumentPage
}

// BacklogWindow qualifies a native provider page with its authorized binding.
type BacklogWindow struct {
	SourceBindingID string
	Page            workbench.BacklogPage
}

// Graph contains observed source facts only. Partial, conflicts, and unresolved
// endpoints prohibit completeness assumptions. No output implies progress,
// deletion, movement, write authority, or permission to read referenced content.
type Graph struct {
	Generation string     `json:"generation,omitempty"`
	GaggleID   string     `json:"gaggleId"`
	Nodes      []Node     `json:"nodes"`
	Edges      []Edge     `json:"edges"`
	Documents  []Document `json:"documents"`
	Aliases    []Alias    `json:"aliases"`
	Sources    []Coverage `json:"sources"`
	Conflicts  []Conflict `json:"conflicts"`
	Partial    bool       `json:"partial"`
}

// Node retains all distinct observed representations. Conflict never selects a
// winning title/revision or silently combines two live objective locations.
type Node struct {
	Key          string        `json:"key"`
	Observations []Observation `json:"observations"`
	Conflict     bool          `json:"conflict"`
}

// Observation preserves a source representation without computing progress.
type Observation struct {
	ContentDigest string                      `json:"contentDigest"`
	Ref           workbench.NodeRef           `json:"ref"`
	Title         string                      `json:"title"`
	Type          string                      `json:"type,omitempty"`
	State         string                      `json:"state,omitempty"`
	Revision      string                      `json:"revision"`
	Locator       workbench.SourceLocator     `json:"locator"`
	Objective     bool                        `json:"objective"`
	Path          string                      `json:"path,omitempty"`
	Provenance    *workbench.SourceProvenance `json:"provenance,omitempty"`
}

// Endpoint resolves only to an unambiguous observed node. Native is an explicit
// source link whose configured project membership was not independently proven.
// A valid scoped Ref can still be unresolved because its content was not read.
type Endpoint struct {
	Ref      *workbench.NodeRef      `json:"ref,omitempty"`
	Native   *workbench.NativeTarget `json:"native,omitempty"`
	Resolved bool                    `json:"resolved"`
}

// Edge retains explicit native or authored relationship direction and ownership.
type Edge struct {
	Key       string   `json:"key"` // Projection identity only, never a new source-owned ID.
	EdgeID    string   `json:"edgeId,omitempty"`
	Kind      string   `json:"kind"`
	From      Endpoint `json:"from"`
	To        Endpoint `json:"to"`
	Rationale string   `json:"rationale,omitempty"`
	Origin    string   `json:"origin"` // native or authored
	Owner     Owner    `json:"owner"`
	Conflict  bool     `json:"conflict"`
}

// Document preserves ordinary reference material and omitted configured files
// without inventing persistent document identities or copying their body bytes.
type Document struct {
	SourceBindingID string                      `json:"sourceBindingId"`
	Path            string                      `json:"path"`
	Status          string                      `json:"status"`
	Provenance      *workbench.SourceProvenance `json:"provenance,omitempty"`
	Ref             *workbench.NodeRef          `json:"ref,omitempty"`
}

// Alias is an alternate source-owned name, never a second planning object.
type Alias struct {
	Name   string   `json:"name"`
	Target Endpoint `json:"target"`
	Owner  Owner    `json:"owner"`
}

// Coverage explains what a supplied source read can and cannot establish.
type Coverage struct {
	SourceBindingID    string   `json:"sourceBindingId"`
	Kind               string   `json:"kind"`
	Status             string   `json:"status"`      // complete, partial, or not-read
	Consistency        string   `json:"consistency"` // commit-pinned or native-non-snapshot
	SourceTargetDigest string   `json:"sourceTargetDigest,omitempty"`
	Commit             string   `json:"commit,omitempty"`
	Reasons            []string `json:"reasons,omitempty"`
}

// Conflict identifies ambiguous source observations without selecting a winner.
type Conflict struct {
	Kind string   `json:"kind"`
	Keys []string `json:"keys"`
}

// Owner is the public source location of a relationship. Object is present only
// for native field ownership, so an empty reference cannot imply another node.
type Owner struct {
	Kind            string             `json:"kind"`
	SourceBindingID string             `json:"sourceBindingId"`
	Path            string             `json:"path,omitempty"`
	Field           string             `json:"field,omitempty"`
	Object          *workbench.NodeRef `json:"object,omitempty"`
}

func graphOwner(source workbench.Owner) Owner {
	owner := Owner{Kind: source.Kind, SourceBindingID: source.SourceBindingID, Path: source.Path, Field: source.Field}
	if source.Object != (workbench.NodeRef{}) {
		object := source.Object
		owner.Object = &object
	}
	return owner
}
