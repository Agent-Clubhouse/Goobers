package workbench

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

func parseSourceYAML(raw []byte) (*yaml.Node, error) {
	if len(raw) == 0 || len(raw) > MaxSourceBytes {
		return nil, errors.New("workbench: source must contain 1 byte through 1 MiB")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("workbench: exactly one YAML document is required")
	}
	budget := 20000
	if err := validateYAMLNode(&node, 0, &budget); err != nil {
		return nil, err
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("workbench: source must be a mapping")
	}
	root := node.Content[0]
	// yaml.v3 may attach leading CRLF comments to the document wrapper instead
	// of its first mapping key. Retain those comments when returning the mapping.
	root.HeadComment = strings.TrimSpace(node.HeadComment + "\n" + root.HeadComment)
	root.FootComment = strings.TrimSpace(root.FootComment + "\n" + node.FootComment)
	return root, nil
}

func validateYAMLNode(node *yaml.Node, depth int, remaining *int) error {
	*remaining--
	if depth > 24 || *remaining < 0 || node.Kind == yaml.AliasNode || node.Anchor != "" {
		return errors.New("workbench: source nesting, aliases or node budget exceeded")
	}
	if node.Kind == yaml.MappingNode {
		keys := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || keys[key.Value] {
				return errors.New("workbench: mapping keys must be unique strings")
			}
			keys[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := validateYAMLNode(child, depth+1, remaining); err != nil {
			return err
		}
	}
	return nil
}

func decodeClosed(node *yaml.Node, out any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	return decoder.Decode(out)
}
