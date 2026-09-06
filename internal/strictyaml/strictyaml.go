// Package strictyaml wraps sigs.k8s.io/yaml's YAML-to-JSON conversion with a
// duplicate-mapping-key check (#3643). sigs.k8s.io/yaml (and the yaml.v2
// decoder it round-trips through) silently keeps the LAST occurrence of a
// repeated key instead of erroring, so a later duplicate can override an
// earlier, safety-relevant field while schema and unknown-field validation
// both see only the surviving value and report success. Every canonical
// YAML-decoding boundary should go through YAMLToJSON or Unmarshal here
// instead of calling sigs.k8s.io/yaml directly.
package strictyaml

import (
	"fmt"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// YAMLToJSON converts raw to JSON like sigs.k8s.io/yaml.YAMLToJSON, but
// rejects a document containing a duplicate mapping key at any depth instead
// of silently keeping the last one.
func YAMLToJSON(raw []byte) ([]byte, error) {
	if err := checkDuplicateKeys(raw); err != nil {
		return nil, err
	}
	return yaml.YAMLToJSON(raw)
}

// Unmarshal decodes raw into out like sigs.k8s.io/yaml.Unmarshal, but rejects
// a document containing a duplicate mapping key at any depth instead of
// silently keeping the last one.
func Unmarshal(raw []byte, out interface{}) error {
	if err := checkDuplicateKeys(raw); err != nil {
		return err
	}
	return yaml.Unmarshal(raw, out)
}

// checkDuplicateKeys walks raw's own YAML parse tree (gopkg.in/yaml.v3, which
// — unlike the yaml.v2-based sigs.k8s.io/yaml conversion this package wraps —
// preserves every key node it encountered, duplicates included) looking for a
// repeated key within the same mapping. A syntax error here is left for the
// caller's own parse to surface in its usual form; duplicate-key checking
// only applies to YAML that parses cleanly.
func checkDuplicateKeys(raw []byte) error {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	if doc.Kind != yamlv3.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	return walkDuplicateKeys(doc.Content[0])
}

func walkDuplicateKeys(node *yamlv3.Node) error {
	switch node.Kind {
	case yamlv3.MappingNode:
		firstLine := make(map[string]int, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if line, ok := firstLine[key.Value]; ok {
				return fmt.Errorf("duplicate key %q at line %d (first set at line %d)", key.Value, key.Line, line)
			}
			firstLine[key.Value] = key.Line
			if err := walkDuplicateKeys(node.Content[i+1]); err != nil {
				return err
			}
		}
	case yamlv3.SequenceNode, yamlv3.DocumentNode:
		for _, child := range node.Content {
			if err := walkDuplicateKeys(child); err != nil {
				return err
			}
		}
	}
	return nil
}
