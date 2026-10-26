package dslmigrate

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const previewAnnotationV21 = "goobers.dev/allow-preview-features"
const previewEntryV21 = previewAnnotationV21 + `: "true"`

// Explicitly selecting and writing the 2.1 target accepts the preview contract
// shown in the diff. An authored non-enabling value is never overridden.
func previewOptInV21SourceEdit(source []byte, root *yaml.Node) (*sourceEdit, error) {
	metadata, _ := mapValue(root, "metadata")
	if metadata == nil {
		metadata = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendPreviewAnnotationsV21(metadata)
		edit, err := insertMappingScalarEdit(source, root, "metadata: {annotations: {"+previewEntryV21+"}}")
		appendMappingNodeV21(root, "metadata", metadata)
		return &edit, err
	}
	if metadata.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("metadata must be a mapping before DSL 2.1 migration")
	}
	annotations, index := mapValue(metadata, "annotations")
	if annotations == nil {
		edit, err := insertMappingScalarEdit(source, metadata, "annotations: {"+previewEntryV21+"}")
		appendPreviewAnnotationsV21(metadata)
		return &edit, err
	}
	if annotations.Kind == yaml.ScalarNode && annotations.Tag == "!!null" {
		edit, err := nullScalarReplacementEdit(source, metadata.Content[index-1], annotations, []byte("{"+previewEntryV21+"}"), "metadata.annotations")
		replacement := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setScalar(replacement, previewAnnotationV21, "true", "!!str")
		metadata.Content[index] = replacement
		return &edit, err
	}
	if annotations.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("metadata.annotations must be a mapping before DSL 2.1 migration")
	}
	if existing, _ := mapValue(annotations, previewAnnotationV21); existing != nil {
		if existing.Kind != yaml.ScalarNode || existing.Value != "true" {
			return nil, fmt.Errorf("workflow.metadata.annotations %s explicitly does not enable preview features; review that authored value before migrating to DSL 2.1", previewAnnotationV21)
		}
		return nil, nil
	}
	edit, err := insertMappingScalarEdit(source, annotations, previewEntryV21)
	setScalar(annotations, previewAnnotationV21, "true", "!!str")
	return &edit, err
}

func appendPreviewAnnotationsV21(metadata *yaml.Node) {
	annotations := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setScalar(annotations, previewAnnotationV21, "true", "!!str")
	appendMappingNodeV21(metadata, "annotations", annotations)
}

func appendMappingNodeV21(mapping *yaml.Node, key string, value *yaml.Node) {
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
