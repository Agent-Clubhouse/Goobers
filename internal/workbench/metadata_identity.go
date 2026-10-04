package workbench

import (
	"bytes"
	"slices"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// MetadataObjectiveAssignment adds identity to an existing declared document.
// The ID is source-owned and immutable once assigned; this does not create a file.
type MetadataObjectiveAssignment struct {
	ObjectiveID string `json:"objectiveId"`
	Title       string `json:"title"`
}

// MetadataAliasEdit adds/removes one exact name and qualified target pair.
// Changing a target requires removing the observed alias before a later addition.
type MetadataAliasEdit struct {
	Action string `json:"action"`
	Alias  Alias  `json:"alias"`
}

// ValidateMetadataChangeRequest validates bounded shape and same-gaggle identities.
// It does not check source ownership, declared writes, provider authority or bytes.
// Custody and HTTP callers supply trusted route scope and validate authorization separately.
func ValidateMetadataChangeRequest(scope Scope, binding string, request MetadataChangeRequest) error {
	if scope.Validate() != nil || !scope.Bindings[binding] || !validSourcePath(request.Path) || !validMetadataRevision(request.Expected) || metadataOperationCount(request) != 1 {
		return ErrMetadataEdit
	}
	switch {
	case request.Objective != nil:
		objective := request.Objective
		return validateObjective(scope, binding, ObjectiveMetadata{SchemaVersion: "objectives/v1", ObjectiveID: objective.ObjectiveID, Title: objective.Title})
	case request.Alias != nil:
		alias := request.Alias
		if (alias.Action != "add" && alias.Action != "remove") || !bindingID.MatchString(alias.Alias.Name) {
			return ErrMetadataEdit
		}
		return scope.ValidateRef(alias.Alias.Target)
	case request.Relationship != nil:
		edit := request.Relationship
		if (edit.Action != "add" && edit.Action != "remove") || (edit.Edge.Kind != "references" && edit.Edge.Kind != "contributes-to") {
			return ErrMetadataEdit
		}
		return scope.ValidateEdge(edit.Edge)
	default:
		return validateMetadataFieldValue(request)
	}
}

func metadataOperationCount(r MetadataChangeRequest) int {
	count := 0
	if r.Field != "" || r.Value != nil {
		count++
	}
	if r.Relationship != nil {
		count++
	}
	if r.Objective != nil {
		count++
	}
	if r.Alias != nil {
		count++
	}
	return count
}

func validateMetadataFieldValue(request MetadataChangeRequest) error {
	if request.Value == nil {
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

func editMetadataManifestRequest(scope Scope, raw []byte, request MetadataChangeRequest) ([]byte, error) {
	if request.Alias == nil {
		return editMetadataManifest(scope, raw, *request.Relationship)
	}
	manifest, err := ParseManifest(raw, scope)
	if err != nil {
		return nil, err
	}
	node, err := parseSourceYAML(raw)
	if err != nil {
		return nil, err
	}
	changed, err := editMetadataAliases(node, manifest.Aliases, *request.Alias)
	if err != nil {
		return nil, err
	}
	if !changed {
		return append([]byte(nil), raw...), nil
	}
	newline := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		newline = "\r\n"
	}
	after, err := marshalMetadataYAML(node, newline)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		after = append([]byte{0xef, 0xbb, 0xbf}, after...)
	}
	if _, err := ParseManifest(after, scope); err != nil {
		return nil, err
	}
	return after, nil
}

func editMetadataAliases(owner *yaml.Node, existing []Alias, edit MetadataAliasEdit) (bool, error) {
	index := slices.IndexFunc(existing, func(alias Alias) bool { return alias.Name == edit.Alias.Name })
	if index >= 0 && existing[index] != edit.Alias {
		return false, ErrMetadataEdge
	}
	if edit.Action == "remove" {
		if index < 0 {
			return false, ErrMetadataEdge
		}
		sequence := metadataMappingValue(owner, "aliases")
		sequence.Content = slices.Delete(sequence.Content, index, index+1)
		return true, nil
	}
	if index >= 0 {
		return false, nil
	}
	if len(existing) >= 128 {
		return false, ErrMetadataEdit
	}
	sequence := metadataMappingValue(owner, "aliases")
	if sequence == nil {
		sequence = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		owner.Content = append(owner.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "aliases"}, sequence)
	}
	var value yaml.Node
	if err := value.Encode(edit.Alias); err != nil {
		return false, err
	}
	sequence.Kind, sequence.Tag = yaml.SequenceNode, "!!seq"
	sequence.Content = append(sequence.Content, &value)
	return true, nil
}
