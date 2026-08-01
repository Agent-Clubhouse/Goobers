package validate

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	yamlv3 "gopkg.in/yaml.v3"
)

// schemaFinding is one leaf schema-validation failure, resolved (best-effort)
// to the source position of the offending YAML node and, for an
// additionalProperties near-miss, a "did you mean" suggestion (#2025). Line
// is 0 when no source node could be resolved for the failure's JSON pointer
// (e.g. a required-but-entirely-absent property has no node to point at) —
// the caller falls back to file-level reporting in that case, exactly as
// before this feature existed.
type schemaFinding struct {
	message string
	line    int
	col     int
}

var additionalPropertyPattern = regexp.MustCompile(`'([^']*)'`)

// parseYAMLNode reparses content (a single YAML document's own source,
// already known to convert cleanly via sigs.k8s.io/yaml since that happened
// first) with gopkg.in/yaml.v3 to recover node positions, which the
// sigs.k8s.io/yaml round-trip through JSON does not preserve. Returns nil on
// a parse failure — schemaFindings degrades to file-level reporting, exactly
// as it did before positions existed.
func parseYAMLNode(content string) *yamlv3.Node {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(content), &doc); err != nil {
		return nil
	}
	if doc.Kind != yamlv3.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0]
}

// schemaFindings turns a jsonschema ValidationError tree into per-leaf
// findings, each resolved against schema (the compiled root schema, for
// did-you-mean candidates) and node (the document's parsed YAML root, for
// source position) when both are available. schema/node may be nil — every
// resolution step degrades to "unknown", matching flattenSchemaError's
// original file-only behavior.
func schemaFindings(err error, schema *jsonschema.Schema, node *yamlv3.Node) []schemaFinding {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []schemaFinding{{message: err.Error()}}
	}
	var findings []schemaFinding
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			findings = append(findings, resolveSchemaFinding(e, schema, node))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(findings) == 0 {
		findings = append(findings, schemaFinding{message: ve.Message})
	}
	return findings
}

// resolveSchemaFinding builds one leaf's finding: the friendly message (with
// a did-you-mean suffix for an additionalProperties near-miss) and its best
// available source position.
func resolveSchemaFinding(e *jsonschema.ValidationError, schema *jsonschema.Schema, node *yamlv3.Node) schemaFinding {
	loc := e.InstanceLocation
	if loc == "" {
		loc = "(root)"
	}
	message := friendlySchemaMessage(e.Message)
	segments := jsonPointerSegments(e.InstanceLocation)

	if names := additionalPropertyNames(e.Message); len(names) > 0 {
		parent := yamlNodeAt(node, segments)
		line, col := 0, 0
		if parent != nil {
			line, col = parent.Line, parent.Column
			if key := yamlMappingKey(parent, names[0]); key != nil {
				line, col = key.Line, key.Column
			}
		}
		if candidates := schemaPropertyNames(schemaObjectAt(schema, segments)); len(candidates) > 0 {
			if suggestion, ok := didYouMean(names[0], candidates); ok {
				message += fmt.Sprintf("; did you mean %q?", suggestion)
			}
		}
		return schemaFinding{message: fmt.Sprintf("%s: %s", loc, message), line: line, col: col}
	}

	line, col := 0, 0
	if target := yamlNodeAt(node, segments); target != nil {
		line, col = target.Line, target.Column
	}
	return schemaFinding{message: fmt.Sprintf("%s: %s", loc, message), line: line, col: col}
}

// additionalPropertyNames extracts the quoted, rejected property name(s) from
// an "additionalProperties 'a', 'b' not allowed" message. Returns nil for any
// other keyword's message.
func additionalPropertyNames(message string) []string {
	if !strings.HasPrefix(message, "additionalProperties ") {
		return nil
	}
	var names []string
	for _, m := range additionalPropertyPattern.FindAllStringSubmatch(message, -1) {
		names = append(names, m[1])
	}
	return names
}

// jsonPointerSegments splits an RFC 6901 JSON pointer into its unescaped
// segments ("" and "/" both yield no segments — the document root).
func jsonPointerSegments(pointer string) []string {
	if pointer == "" || pointer == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		parts[i] = p
	}
	return parts
}

// yamlNodeAt walks root (a document's parsed YAML root node) along segments,
// returning the node at that path or nil if any segment doesn't resolve —
// which is expected and not an error: a required-but-absent property, for
// instance, legitimately has no node.
func yamlNodeAt(root *yamlv3.Node, segments []string) *yamlv3.Node {
	node := root
	for _, seg := range segments {
		if node == nil {
			return nil
		}
		node = yamlChild(node, seg)
	}
	return node
}

func yamlChild(node *yamlv3.Node, key string) *yamlv3.Node {
	switch node.Kind {
	case yamlv3.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				return node.Content[i+1]
			}
		}
		return nil
	case yamlv3.SequenceNode:
		idx, err := strconv.Atoi(key)
		if err != nil || idx < 0 || idx >= len(node.Content) {
			return nil
		}
		return node.Content[idx]
	default:
		return nil
	}
}

// yamlMappingKey returns the KEY node (not the value) matching key within a
// mapping node, so an additionalProperties finding can point at the actual
// typo'd field name rather than the whole enclosing block.
func yamlMappingKey(node *yamlv3.Node, key string) *yamlv3.Node {
	if node == nil || node.Kind != yamlv3.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i]
		}
	}
	return nil
}

// schemaObjectAt resolves the compiled sub-schema governing the object at
// segments within root (the document's root schema), so its declared
// Properties can seed a did-you-mean candidate list. Returns nil when the
// path doesn't resolve to a schema with known properties (e.g. it allows
// arbitrary additional properties one level up already).
func schemaObjectAt(root *jsonschema.Schema, segments []string) *jsonschema.Schema {
	schema := resolveSchemaRef(root)
	for _, seg := range segments {
		if schema == nil {
			return nil
		}
		if idx, err := strconv.Atoi(seg); err == nil {
			schema = resolveSchemaRef(schemaItemAt(schema, idx))
			continue
		}
		if schema.Properties == nil {
			return nil
		}
		schema = resolveSchemaRef(schema.Properties[seg])
	}
	return schema
}

// resolveSchemaRef follows a bare $ref wrapper (a schema with no properties
// of its own) to the schema it points at, matching how this repo's schemas
// factor shared shapes like repoRef into $defs.
func resolveSchemaRef(s *jsonschema.Schema) *jsonschema.Schema {
	for s != nil && s.Ref != nil && s.Properties == nil {
		s = s.Ref
	}
	return s
}

func schemaItemAt(s *jsonschema.Schema, idx int) *jsonschema.Schema {
	if s == nil {
		return nil
	}
	if idx < len(s.PrefixItems) {
		return s.PrefixItems[idx]
	}
	return s.Items2020
}

func schemaPropertyNames(s *jsonschema.Schema) []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	return names
}

// didYouMean returns the candidate closest to name by edit distance, when
// that distance is small enough to be a plausible typo rather than an
// unrelated field (the same threshold internal/capability.Suggest uses).
func didYouMean(name string, candidates []string) (string, bool) {
	bestDistance := -1
	var best string
	for _, candidate := range candidates {
		distance := editDistance(name, candidate)
		if bestDistance == -1 || distance < bestDistance {
			bestDistance = distance
			best = candidate
		}
	}
	if bestDistance < 0 || bestDistance > 2 {
		return "", false
	}
	return best, true
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(
				current[j-1]+1,
				previous[j]+1,
				previous[j-1]+cost,
			)
		}
		previous = current
	}
	return previous[len(b)]
}
