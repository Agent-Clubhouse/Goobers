package main

import (
	"strings"

	"sigs.k8s.io/yaml"
)

// toJSON returns JSON bytes for a file that is either JSON or YAML. JSON is valid
// YAML, so YAMLToJSON handles both; we special-case the .json extension only to
// avoid surprises with anchors/aliases.
func toJSON(file string, raw []byte) ([]byte, error) {
	if strings.HasSuffix(strings.ToLower(file), ".json") {
		return raw, nil
	}
	return yaml.YAMLToJSON(raw)
}
