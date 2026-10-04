package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// MetadataRevision pins source bytes, independently of transport cache metadata.
// The trusted repository reader must also prove the path is a regular file.
type MetadataRevision struct {
	Commit        string `json:"commit"`
	BlobID        string `json:"blobId"`
	ContentDigest string `json:"contentDigest"`
}

// MetadataFile is supplied by the authorized, revision-pinned repository reader,
// never deserialized from a client request. Previewing grants no write authority.
type MetadataFile struct {
	Path       string
	Provenance SourceProvenance
	Content    []byte
}

// MetadataChangeRequest names one existing declared file and exactly one edit.
// It cannot create files, move identities, choose a repository/branch, or replace
// arbitrary frontmatter. Description edits replace only the Markdown body.
type MetadataChangeRequest struct {
	Path         string                    `json:"path"`
	Expected     MetadataRevision          `json:"expected"`
	Field        apiv1.WorkbenchField      `json:"field,omitempty"`
	Value        *string                   `json:"value,omitempty"`
	Relationship *MetadataRelationshipEdit `json:"relationship,omitempty"`
}

// MetadataRelationshipEdit adds or removes one exact, persistently identified
// edge. Remove requires the full observed edge, including its rationale.
type MetadataRelationshipEdit struct {
	Action string `json:"action"`
	Edge   Edge   `json:"edge"`
}

// MetadataPreview contains bounded candidate source for a human-reviewed PR.
// Before and After each fit MaxSourceBytes. Formatting may normalize only when
// YAML changes; body-only edits preserve existing frontmatter bytes.
type MetadataPreview struct {
	Path                  string           `json:"path"`
	Expected              MetadataRevision `json:"expected"`
	TargetDigest          string           `json:"targetDigest"`
	OperationDigest       string           `json:"operationDigest"`
	ProposedContentDigest string           `json:"proposedContentDigest"`
	Changed               bool             `json:"changed"`
	Before                string           `json:"before"`
	After                 string           `json:"after"`
}

var (
	// ErrMetadataRevision refuses a stale or unverifiable immutable source pin.
	ErrMetadataRevision = errors.New("workbench: metadata source revision changed")
	// ErrMetadataEdit refuses operations outside the supported source allowlist.
	ErrMetadataEdit = errors.New("workbench: metadata edit is unsupported or not allowlisted")
	// ErrMetadataEdge refuses changing edge identity/content or source ownership.
	ErrMetadataEdge = errors.New("workbench: metadata edge conflicts with its source owner or observed content")
)

// PreviewMetadataChange performs no I/O. The source set and file must come from
// current server-authorized adapters. A later publisher must repeat policy and
// revision checks; neither this preview nor its digests confer authority.
func PreviewMetadataChange(set SourceSet, binding string, current MetadataFile, request MetadataChangeRequest) (MetadataPreview, error) {
	source, err := metadataSource(set, binding, request.Path)
	if err != nil {
		return MetadataPreview{}, err
	}
	if err := validateMetadataRevision(current, request); err != nil {
		return MetadataPreview{}, err
	}
	if err := validateMetadataChange(set, source, request); err != nil {
		return MetadataPreview{}, err
	}
	var after []byte
	if source.Spec.Kind == "documents" {
		after, err = editMetadataDocument(set.Scope, source, current.Content, request)
	} else {
		after, err = editMetadataManifest(set.Scope, current.Content, *request.Relationship)
	}
	if err != nil {
		return MetadataPreview{}, err
	}
	if len(after) > MaxSourceBytes {
		return MetadataPreview{}, errors.New("workbench: proposed source exceeds byte bound")
	}
	target, operation := metadataOperationDigests(set.Scope, source, request)
	return MetadataPreview{Path: request.Path, Expected: request.Expected, TargetDigest: target,
		OperationDigest: operation, ProposedContentDigest: metadataContentDigest(after),
		Changed: string(current.Content) != string(after), Before: string(current.Content), After: string(after)}, nil
}

// MetadataOperationDigest validates the current declaration and request shape
// without fetching source bytes or credentials. It supports exact receipt replay;
// only PreviewMetadataChange verifies current source content and edit ownership.
func MetadataOperationDigest(set SourceSet, binding string, request MetadataChangeRequest) (target, operation string, err error) {
	source, err := metadataSource(set, binding, request.Path)
	if err != nil {
		return "", "", err
	}
	if !validMetadataRevision(request.Expected) {
		return "", "", ErrMetadataRevision
	}
	if err := validateMetadataChange(set, source, request); err != nil {
		return "", "", err
	}
	target, operation = metadataOperationDigests(set.Scope, source, request)
	return target, operation, nil
}

func metadataSource(set SourceSet, binding, path string) (BoundSource, error) {
	if set.Scope.Validate() != nil || !set.Scope.Bindings[binding] || !validSourcePath(path) {
		return BoundSource{}, ErrMetadataEdit
	}
	for _, source := range set.Sources {
		if source.Spec.Name != binding {
			continue
		}
		if (source.Spec.Kind != "documents" && source.Spec.Kind != "relationships") || !slices.Contains(source.Spec.Paths, path) {
			return BoundSource{}, ErrMetadataEdit
		}
		if !validRepositoryIdentity(source.Spec.Repository) || source.Repository.BaseURL != "" || source.Repository.Branch == "" {
			return BoundSource{}, ErrMetadataEdit
		}
		target := apiv1.InteractiveRepositoryIdentity{Provider: source.Repository.Provider, Owner: source.Repository.Owner, Project: source.Repository.Project, Name: source.Repository.Name}
		if target != *source.Spec.Repository {
			return BoundSource{}, ErrMetadataEdit
		}
		return source, nil
	}
	return BoundSource{}, ErrMetadataEdit
}

func validateMetadataRevision(current MetadataFile, request MetadataChangeRequest) error {
	if len(current.Content) > MaxSourceBytes || !utf8.Valid(current.Content) {
		return errors.New("workbench: metadata source is not bounded UTF-8")
	}
	pins := MetadataRevision{Commit: current.Provenance.Commit, BlobID: current.Provenance.BlobID, ContentDigest: current.Provenance.ContentDigest}
	if current.Path != request.Path || pins != request.Expected || !validMetadataRevision(pins) || pins.ContentDigest != metadataContentDigest(current.Content) {
		return ErrMetadataRevision
	}
	return nil
}

func validateMetadataChange(set SourceSet, source BoundSource, request MetadataChangeRequest) error {
	if request.Relationship != nil {
		if request.Field != "" || request.Value != nil {
			return ErrMetadataEdit
		}
		return validateMetadataRelationship(set, source, request.Path, *request.Relationship)
	}
	if request.Value == nil || source.Spec.Kind != "documents" || !source.AllowsField(request.Field) {
		return ErrMetadataEdit
	}
	switch request.Field {
	case "title":
		if !textValue(*request.Value, 512) {
			return ErrMetadataEdit
		}
	case "description":
		if len(*request.Value) > MaxSourceBytes || !utf8.ValidString(*request.Value) {
			return ErrMetadataEdit
		}
	default:
		return ErrMetadataEdit
	}
	return nil
}

func validateMetadataRelationship(set SourceSet, source BoundSource, path string, edit MetadataRelationshipEdit) error {
	if edit.Action != "add" && edit.Action != "remove" {
		return ErrMetadataEdit
	}
	// Native hierarchy, blocker, membership and PR associations never fall back
	// to the manifest merely because their provider mutation is unavailable.
	if (edit.Edge.Kind != "references" && edit.Edge.Kind != "contributes-to") || !source.AllowsRelationship(apiv1.WorkbenchRelationship(edit.Edge.Kind)) {
		return ErrMetadataEdit
	}
	if err := set.Scope.ValidateEdge(edit.Edge); err != nil {
		return err
	}
	if source.Spec.Kind == "documents" {
		if edit.Edge.From.Kind != "objective-document" || edit.Edge.From.SourceBindingID != source.Spec.Name {
			return ErrMetadataEdge
		}
		return nil
	}
	owner := Owner{Kind: "manifest", SourceBindingID: source.Spec.Name, Path: path}
	if set.ManifestOwner == nil || *set.ManifestOwner != owner || edit.Edge.From.Kind != "work-item" {
		return ErrMetadataEdge
	}
	for _, bound := range set.Sources {
		if bound.Spec.Name == edit.Edge.From.SourceBindingID && bound.Spec.Kind == "backlog" {
			return nil
		}
	}
	return ErrMetadataEdge
}

func validMetadataRevision(value MetadataRevision) bool {
	return metadataHex(value.Commit, 40) && metadataHex(value.BlobID, 40) && metadataHex(value.ContentDigest, 64)
}

func metadataOperationDigests(scope Scope, source BoundSource, request MetadataChangeRequest) (target, operation string) {
	target = metadataTargetDigest(scope, source, request.Path)
	operation = metadataDigest(struct {
		Version, Target string
		Request         MetadataChangeRequest
	}{"metadata-change/v1", target, request})
	return target, operation
}

func metadataTargetDigest(scope Scope, source BoundSource, path string) string {
	return metadataDigest(struct {
		Gaggle, Binding, Kind, Branch, Path string
		Repository                          apiv1.InteractiveRepositoryIdentity
	}{scope.GaggleID, source.Spec.Name, source.Spec.Kind, source.Repository.Branch, path, *source.Spec.Repository})
}

func metadataDigest(value any) string {
	raw, _ := json.Marshal(value)
	return metadataContentDigest(raw)
}

func metadataContentDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func metadataHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
