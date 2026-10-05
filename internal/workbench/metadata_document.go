package workbench

import (
	"bytes"
	"errors"
	"reflect"

	"gopkg.in/yaml.v3"
)

func editMetadataDocument(scope Scope, source BoundSource, raw []byte, request MetadataChangeRequest) ([]byte, error) {
	doc, err := ParseDocument(raw, scope, source.Spec.Name)
	if err != nil {
		return nil, err
	}
	if request.Objective != nil {
		if doc.Objective != nil {
			return nil, ErrMetadataEdit
		}
		return ProposeObjectiveMetadata(raw, scope, source.Spec.Name, ObjectiveMetadata{SchemaVersion: "objectives/v1", ObjectiveID: request.Objective.ObjectiveID, Title: request.Objective.Title})
	}
	if request.Field == "description" {
		return editMetadataBody(scope, source.Spec.Name, raw, doc, *request.Value)
	}
	// Ordinary field and edge edits require an already assigned objective.
	if doc.Objective == nil {
		return nil, ErrMetadataEdit
	}
	node := metadataMappingValue(doc.header, "goobers")
	if request.Relationship == nil {
		if doc.Objective.Title == *request.Value {
			return append([]byte(nil), raw...), nil
		}
		title := metadataMappingValue(node, "title")
		title.Value, title.Tag = *request.Value, "!!str"
	} else {
		from := NodeRef{GaggleID: scope.GaggleID, SourceBindingID: source.Spec.Name, Kind: "objective-document", SourceID: doc.Objective.ObjectiveID}
		if request.Relationship.Edge.From != from {
			return nil, ErrMetadataEdge
		}
		changed, err := editMetadataEdges(scope, node, doc.Objective.Edges, *request.Relationship)
		if err != nil {
			return nil, err
		}
		if !changed {
			return append([]byte(nil), raw...), nil
		}
	}
	header, err := marshalMetadataYAML(doc.header, doc.newline)
	if err != nil {
		return nil, err
	}
	after := append([]byte{}, doc.bom...)
	after = append(after, []byte("---"+doc.newline)...)
	after = append(after, header...)
	after = append(after, []byte("---"+doc.newline)...)
	after = append(after, doc.Body...)
	if _, err := ParseDocument(after, scope, source.Spec.Name); err != nil {
		return nil, err
	}
	return after, nil
}

func editMetadataBody(scope Scope, binding string, raw []byte, doc Document, body string) ([]byte, error) {
	if string(doc.Body) == body {
		return append([]byte(nil), raw...), nil
	}
	prefix := raw[:len(raw)-len(doc.Body)]
	after := append([]byte(nil), prefix...)
	// A closing delimiter at EOF needs a newline before its first body bytes.
	if len(prefix) > len(doc.bom) && prefix[len(prefix)-1] != '\n' && body != "" {
		after = append(after, []byte(doc.newline)...)
	}
	after = append(after, []byte(body)...)
	parsed, err := ParseDocument(after, scope, binding)
	if err != nil {
		return nil, err
	}
	// Without existing frontmatter, leading delimiters/BOM in replacement body
	// must not create a metadata namespace through the description permission.
	if string(parsed.Body) != body || !reflect.DeepEqual(parsed.Objective, doc.Objective) {
		return nil, errors.New("workbench: description edit would change metadata structure")
	}
	return after, nil
}

func metadataMappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func marshalMetadataYAML(node *yaml.Node, newline string) ([]byte, error) {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return nil, err
	}
	if newline == "\r\n" {
		raw = bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
	}
	return raw, nil
}
