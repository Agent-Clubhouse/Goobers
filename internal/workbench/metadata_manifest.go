package workbench

import (
	"bytes"
	"slices"

	"gopkg.in/yaml.v3"
)

func editMetadataManifest(scope Scope, raw []byte, edit MetadataRelationshipEdit) ([]byte, error) {
	manifest, err := ParseManifest(raw, scope)
	if err != nil {
		return nil, err
	}
	node, err := parseSourceYAML(raw)
	if err != nil {
		return nil, err
	}
	changed, err := editMetadataEdges(scope, node, manifest.Edges, edit)
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

func editMetadataEdges(scope Scope, owner *yaml.Node, existing []Edge, edit MetadataRelationshipEdit) (bool, error) {
	index := slices.IndexFunc(existing, func(edge Edge) bool { return edge.EdgeID == edit.Edge.EdgeID })
	if index >= 0 && existing[index] != edit.Edge {
		return false, ErrMetadataEdge
	}
	if edit.Action == "remove" {
		if index < 0 {
			return false, ErrMetadataEdge
		}
		sequence := metadataMappingValue(owner, "edges")
		sequence.Content = slices.Delete(sequence.Content, index, index+1)
		return true, nil
	}
	if index >= 0 {
		return false, nil
	}
	proposed := append(slices.Clone(existing), edit.Edge)
	if err := validateEdges(scope, proposed); err != nil {
		return false, err
	}
	sequence := metadataMappingValue(owner, "edges")
	if sequence == nil {
		sequence = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		owner.Content = append(owner.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "edges"}, sequence)
	}
	var value yaml.Node
	if err := value.Encode(edit.Edge); err != nil {
		return false, err
	}
	sequence.Kind, sequence.Tag = yaml.SequenceNode, "!!seq"
	sequence.Content = append(sequence.Content, &value)
	return true, nil
}
