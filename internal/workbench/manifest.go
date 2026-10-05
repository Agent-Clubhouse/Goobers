package workbench

import (
	"errors"
	"path"
	"strings"
)

// Alias is an alternate source-owned name for an existing object. It is not a
// second objective and cannot change the target's qualified identity.
type Alias struct {
	Name   string  `json:"name" yaml:"name"`
	Target NodeRef `json:"target" yaml:"target"`
}

// Manifest holds explicit relationships in one configured repository file.
// Ownership is selected before checking write permission; a denied native write
// must never fall back to manufacturing another edge in this file.
type Manifest struct {
	SchemaVersion string  `json:"schemaVersion" yaml:"schemaVersion"`
	Aliases       []Alias `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	Edges         []Edge  `json:"edges" yaml:"edges"`
}

// ParseManifest rejects unknown fields, unbounded YAML, duplicates and scope
// escapes. A successful parse does not establish this file's configured ownership.
func ParseManifest(raw []byte, scope Scope) (Manifest, error) {
	if err := scope.Validate(); err != nil {
		return Manifest{}, err
	}
	node, err := parseSourceYAML(raw)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := decodeClosed(node, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.SchemaVersion != "relationships/v1" || manifest.Edges == nil || len(manifest.Aliases) > 128 {
		return Manifest{}, errors.New("workbench: invalid manifest version, edges or alias bound")
	}
	seen := make(map[string]bool, len(manifest.Aliases))
	for _, alias := range manifest.Aliases {
		if !bindingID.MatchString(alias.Name) || seen[alias.Name] {
			return Manifest{}, errors.New("workbench: invalid or duplicate alias")
		}
		if err := scope.ValidateRef(alias.Target); err != nil {
			return Manifest{}, err
		}
		seen[alias.Name] = true
	}
	return manifest, validateEdges(scope, manifest.Edges)
}

// Owner identifies the single authoritative location of an authored edge.
type Owner struct {
	Kind, SourceBindingID, Path, Field string
	Object                             NodeRef
}

// Ownership is supplied by trusted source adapters from representation and
// configured ownership. It contains no credentials or mutation availability.
type Ownership struct {
	NativeField   string
	DocumentPath  string
	ManifestOwner *Owner
}

// ResolveOwner applies native -> document -> configured manifest precedence.
// Callers must report a selected owner's denied/unsupported mutation directly.
// They cannot retry owner selection after a permission or capability failure.
func ResolveOwner(scope Scope, edge Edge, policy Ownership) (Owner, error) {
	if err := scope.ValidateEdge(edge); err != nil {
		return Owner{}, err
	}
	if policy.NativeField != "" {
		if !textValue(policy.NativeField, 256) {
			return Owner{}, errors.New("workbench: invalid native relationship field")
		}
		object := edge.From
		if edge.Kind == "parent-of" {
			// Hierarchy is displayed parent -> child, but the child's native
			// parent field owns the relationship. Membership remains on the item.
			object = edge.To
		}
		return Owner{Kind: "native", SourceBindingID: object.SourceBindingID, Field: policy.NativeField, Object: object}, nil
	}
	if policy.DocumentPath != "" {
		if edge.From.Kind != "objective-document" || !validSourcePath(policy.DocumentPath) {
			return Owner{}, errors.New("workbench: invalid frontmatter owner")
		}
		return Owner{Kind: "frontmatter", SourceBindingID: edge.From.SourceBindingID, Path: policy.DocumentPath}, nil
	}
	if policy.ManifestOwner != nil {
		owner := *policy.ManifestOwner
		if owner.Kind != "manifest" || !scope.Bindings[owner.SourceBindingID] || !validSourcePath(owner.Path) || owner.Field != "" || owner.Object != (NodeRef{}) {
			return Owner{}, errors.New("workbench: manifest owner is not a configured source location")
		}
		return owner, nil
	}
	return Owner{}, errors.New("workbench: relationship has no configured source owner")
}

// OwnedEdge combines a source-authored edge with its verified source location.
type OwnedEdge struct {
	Edge  Edge
	Owner Owner
}

// ValidateOwnership reports competing authorities and reused IDs in a bounded
// scan. It never silently deduplicates conflicting source truth or picks a winner.
func ValidateOwnership(scope Scope, values []OwnedEdge) error {
	if len(values) > 200000 {
		return errors.New("workbench: relationship projection exceeds edge bound")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	ids := make(map[string]OwnedEdge, len(values))
	tuples := make(map[string]OwnedEdge, len(values))
	for _, value := range values {
		if err := scope.ValidateEdge(value.Edge); err != nil {
			return err
		}
		if err := validateOwnerLocation(scope, value); err != nil {
			return err
		}
		edge := value.Edge
		tuple := edge.Kind + ":" + edge.From.Key() + ":" + edge.To.Key()
		if previous, ok := ids[edge.EdgeID]; ok && previous != value {
			return errors.New("workbench: edge ID has conflicting source authority or content")
		}
		if previous, ok := tuples[tuple]; ok && previous != value {
			return errors.New("workbench: relationship has conflicting source authorities")
		}
		ids[edge.EdgeID], tuples[tuple] = value, value
	}
	return nil
}

func validateOwnerLocation(scope Scope, value OwnedEdge) error {
	var selection Ownership
	switch value.Owner.Kind {
	case "native":
		selection.NativeField = value.Owner.Field
	case "frontmatter":
		selection.DocumentPath = value.Owner.Path
	case "manifest":
		selection.ManifestOwner = &value.Owner
	default:
		return errors.New("workbench: unknown relationship owner kind")
	}
	owner, err := ResolveOwner(scope, value.Edge, selection)
	if err != nil || owner != value.Owner {
		return errors.New("workbench: relationship owner does not match its source location")
	}
	return nil
}

func validSourcePath(value string) bool {
	return textValue(value, 1024) && value != "." && value != ".." && !path.IsAbs(value) && path.Clean(value) == value && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\:?#*")
}
