// Package workbench describes source-owned planning objects and relationships.
// It is a projection contract, never an authorization or source-of-truth store.
package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxSourceBytes bounds each source document or relationship manifest.
	MaxSourceBytes = 1 << 20
	// MaxSourceEdges bounds explicit relationships in one source file.
	MaxSourceEdges = 2000
)

// NodeRef is qualified by gaggle and configured source binding. SourceID is the
// immutable provider identity, or persistent objective ID, never a title or URL.
type NodeRef struct {
	GaggleID        string `json:"gaggleId" yaml:"gaggleId"`
	SourceBindingID string `json:"sourceBindingId" yaml:"sourceBindingId"`
	Kind            string `json:"kind" yaml:"kind"`
	SourceID        string `json:"sourceId" yaml:"sourceId"`
}

// Edge preserves the authored direction; inverse navigation is computed.
type Edge struct {
	EdgeID    string  `json:"edgeId" yaml:"edgeId"`
	Kind      string  `json:"kind" yaml:"kind"`
	From      NodeRef `json:"from" yaml:"from"`
	To        NodeRef `json:"to" yaml:"to"`
	Rationale string  `json:"rationale,omitempty" yaml:"rationale,omitempty"`
}

// Scope lists already configured bindings. Validation is structural and cannot
// grant access to a binding; readers and writers still need current human policy.
type Scope struct {
	GaggleID string
	Bindings map[string]bool
}

var bindingID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$|^[a-z0-9]$`)
var persistentID = regexp.MustCompile(`^(obj|edge)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Validate checks the bounded source namespace independently of any edge count.
func (s Scope) Validate() error {
	if !textValue(s.GaggleID, 253) || len(s.Bindings) == 0 || len(s.Bindings) > 32 {
		return errors.New("workbench: invalid gaggle source scope")
	}
	for binding, enabled := range s.Bindings {
		if !enabled || !bindingID.MatchString(binding) {
			return errors.New("workbench: invalid configured source binding")
		}
	}
	return nil
}

// ValidateRef refuses scope escape and unqualified source identities.
func (s Scope) ValidateRef(ref NodeRef) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if ref.GaggleID != s.GaggleID || !s.Bindings[ref.SourceBindingID] {
		return errors.New("workbench: reference is outside the configured gaggle sources")
	}
	switch ref.Kind {
	case "work-item", "milestone", "pull-request", "document":
	case "objective-document":
		if !validPersistentID(ref.SourceID, "obj-") {
			return errors.New("workbench: objective reference requires a persistent objective ID")
		}
	default:
		return errors.New("workbench: unknown node kind")
	}
	if !textValue(ref.SourceID, 512) {
		return errors.New("workbench: invalid stable source identity")
	}
	return nil
}

// ValidateEdge checks authored semantics. observed-in-run is journal-derived and
// cannot be declared by a repository manifest or document author.
func (s Scope) ValidateEdge(edge Edge) error {
	if !validPersistentID(edge.EdgeID, "edge-") {
		return errors.New("workbench: invalid persistent edge ID")
	}
	switch edge.Kind {
	case "parent-of", "blocked-by", "contributes-to", "references", "milestone-member", "implemented-by":
	default:
		return errors.New("workbench: unknown or non-authored edge kind")
	}
	if err := s.ValidateRef(edge.From); err != nil {
		return err
	}
	if err := s.ValidateRef(edge.To); err != nil {
		return err
	}
	if edge.From.Key() == edge.To.Key() || (edge.Rationale != "" && !textValue(edge.Rationale, 4096)) {
		return errors.New("workbench: self edge or invalid rationale")
	}
	return validateEdgeTargets(edge)
}

func validateEdgeTargets(edge Edge) error {
	switch edge.Kind {
	case "parent-of", "blocked-by":
		if edge.From.Kind != "work-item" || edge.To.Kind != "work-item" {
			return errors.New("workbench: hierarchy and dependencies require work items")
		}
	case "contributes-to":
		if edge.To.Kind != "objective-document" && edge.To.Kind != "work-item" && edge.To.Kind != "milestone" {
			return errors.New("workbench: contribution target is not an objective source")
		}
	case "milestone-member":
		if edge.From.Kind != "work-item" || edge.To.Kind != "milestone" {
			return errors.New("workbench: milestone membership requires item and milestone")
		}
	case "implemented-by":
		if edge.From.Kind != "work-item" || edge.To.Kind != "pull-request" {
			return errors.New("workbench: implementation requires item and pull request")
		}
	}
	return nil
}

// Key is an opaque deterministic projection key, not a bearer credential.
// Objective identity is gaggle-wide and survives a verified repository move;
// its binding remains a required authorization/location field, not part of that
// identity. Native IDs remain qualified by their configured provider source.
func (ref NodeRef) Key() string {
	if ref.Kind == "objective-document" {
		ref.SourceBindingID = ""
	}
	raw, _ := json.Marshal(ref)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validPersistentID(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && persistentID.MatchString(value)
}

func textValue(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validateEdges(scope Scope, edges []Edge) error {
	if len(edges) > MaxSourceEdges {
		return errors.New("workbench: too many source edges")
	}
	ids := make(map[string]bool, len(edges))
	tuples := make(map[string]bool, len(edges))
	for _, edge := range edges {
		if err := scope.ValidateEdge(edge); err != nil {
			return err
		}
		tuple := edge.Kind + ":" + edge.From.Key() + ":" + edge.To.Key()
		if ids[edge.EdgeID] || tuples[tuple] {
			return fmt.Errorf("workbench: duplicate edge identity or relationship %s", edge.EdgeID)
		}
		ids[edge.EdgeID], tuples[tuple] = true, true
	}
	return nil
}
