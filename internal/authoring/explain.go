// Package authoring projects release-pinned facts from the JSON Schemas
// embedded in the running binary.
package authoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

// ErrUnknownSelector identifies a selector absent from the built-in schemas.
var ErrUnknownSelector = errors.New("unknown selector")

// Explanation combines field facts from an embedded schema with lifecycle
// metadata from the built-in schema and DSL feature registries.
type Explanation struct {
	Selector      string `json:"selector"`
	Documented    bool   `json:"documented"`
	Description   string `json:"description,omitempty"`
	Type          any    `json:"type,omitempty"`
	AllowedValues []any  `json:"allowedValues,omitempty"`
	Default       *any   `json:"default,omitempty"`
	Required      *bool  `json:"required,omitempty"`
	Stability     string `json:"stability"`
	SinceVersion  string `json:"sinceVersion"`
	Example       any    `json:"example"`
}

type selectorPart struct {
	name    string
	element bool
}

type schemaDocument struct {
	entry schemas.Entry
	root  map[string]any
}

type registry struct {
	documents map[string]*schemaDocument
}

// Explain resolves dotted or slash-separated selectors. Array elements use [].
func Explain(selector string) (Explanation, error) {
	parts, err := parseSelector(selector)
	if err != nil {
		return Explanation{}, err
	}

	r := registry{documents: make(map[string]*schemaDocument)}
	doc, err := r.load(parts[0].name)
	if err != nil {
		return Explanation{}, unknownSelector(selector)
	}
	contractEntry := doc.entry

	declared := doc.root
	resolved := doc.root
	currentDoc := doc
	var required *bool

	for _, part := range parts[1:] {
		currentDoc, resolved, err = r.resolve(currentDoc, resolved)
		if err != nil {
			return Explanation{}, unknownSelector(selector)
		}
		properties, ok := resolved["properties"].(map[string]any)
		if !ok {
			return Explanation{}, unknownSelector(selector)
		}
		child, ok := properties[part.name].(map[string]any)
		if !ok {
			return Explanation{}, unknownSelector(selector)
		}
		isRequired := containsString(resolved["required"], part.name)
		required = &isRequired
		declared = child
		currentDoc, resolved, err = r.resolve(currentDoc, child)
		if err != nil {
			return Explanation{}, unknownSelector(selector)
		}

		if !part.element {
			continue
		}
		items, ok := resolved["items"].(map[string]any)
		if !ok {
			return Explanation{}, unknownSelector(selector)
		}
		declared = items
		currentDoc, resolved, err = r.resolve(currentDoc, items)
		if err != nil {
			return Explanation{}, unknownSelector(selector)
		}
		required = nil
	}

	explanation := projectFacts(selector, declared, resolved, required)
	if explanation.AllowedValues == nil {
		if items, ok := resolved["items"].(map[string]any); ok {
			_, item, resolveErr := r.resolve(currentDoc, items)
			if resolveErr != nil {
				return Explanation{}, unknownSelector(selector)
			}
			explanation.AllowedValues = schemaAllowedValues(items, item)
		}
	}
	explanation.Stability, explanation.SinceVersion = selectorLifecycle(parts, contractEntry)
	explanation.Example, err = r.schemaExample(currentDoc, declared, resolved, 0)
	if err != nil {
		return Explanation{}, fmt.Errorf("explain %q: %w", selector, err)
	}
	return explanation, nil
}

func projectFacts(selector string, declared, resolved map[string]any, required *bool) Explanation {
	explanation := Explanation{Selector: selector, Required: required}
	if description, ok := schemaString(declared, resolved, "description"); ok {
		explanation.Description = description
		explanation.Documented = strings.TrimSpace(description) != ""
	}
	if schemaType, ok := schemaValue(declared, resolved, "type"); ok {
		explanation.Type = schemaType
	} else if value, ok := schemaValue(declared, resolved, "const"); ok {
		explanation.Type = jsonValueType(value)
	} else if values, ok := schemaSlice(declared, resolved, "enum"); ok && len(values) > 0 {
		explanation.Type = jsonValueType(values[0])
	} else if schemaType := primarySchemaType(nil, resolved); schemaType != "" {
		explanation.Type = schemaType
	}
	explanation.AllowedValues = schemaAllowedValues(declared, resolved)
	if value, ok := schemaValue(declared, resolved, "default"); ok {
		explanation.Default = &value
	}
	explanation.SinceVersion, _ = schemaString(declared, resolved, "sinceVersion")
	return explanation
}

func selectorLifecycle(parts []selectorPart, entry schemas.Entry) (string, string) {
	selector := normalizedSelector(parts)
	candidates := []string{selector}
	switch {
	case strings.HasPrefix(selector, "workflow.spec.tasks."):
		candidates = append(candidates, "task."+strings.TrimPrefix(selector, "workflow.spec.tasks."))
	case strings.HasPrefix(selector, "workflow.spec.gates."):
		candidates = append(candidates, "gate."+strings.TrimPrefix(selector, "workflow.spec.gates."))
	case strings.HasPrefix(selector, "workflow.spec.triggers."):
		candidates = append(candidates, "trigger."+strings.TrimPrefix(selector, "workflow.spec.triggers."))
	}
	for _, candidate := range candidates {
		if feature, ok := workflow.LookupFeature(workflow.FeatureID(candidate)); ok {
			return string(feature.Level), feature.SinceVersion
		}
		if stability, sinceVersion, ok := featurePrefixLifecycle(candidate); ok {
			return stability, sinceVersion
		}
	}
	for parent := selector; strings.Contains(parent, "."); {
		parent = parent[:strings.LastIndex(parent, ".")]
		if feature, ok := workflow.LookupFeature(workflow.FeatureID(parent)); ok {
			return string(feature.Level), feature.SinceVersion
		}
	}
	return entry.Stability, entry.SinceVersion
}

func featurePrefixLifecycle(prefix string) (string, string, bool) {
	var stability, sinceVersion string
	found := false
	features, err := workflow.FeaturesAtDSLVersion(workflow.AllFeatures(), supportmatrix.CurrentDSLVersion)
	if err != nil {
		return "", "", false
	}
	for _, feature := range features {
		if !strings.HasPrefix(string(feature.ID), prefix+".") {
			continue
		}
		if !found {
			stability = string(feature.Level)
			sinceVersion = feature.SinceVersion
			found = true
			continue
		}
		if stability != string(feature.Level) || sinceVersion != feature.SinceVersion {
			return "", "", false
		}
	}
	return stability, sinceVersion, found
}

func normalizedSelector(parts []selectorPart) string {
	names := make([]string, len(parts))
	for i, part := range parts {
		names[i] = part.name
	}
	return strings.Join(names, ".")
}

func (r *registry) schemaExample(
	doc *schemaDocument,
	declared, resolved map[string]any,
	depth int,
) (any, error) {
	if depth > 32 {
		return nil, errors.New("schema nesting exceeds example depth")
	}
	if examples, ok := schemaSlice(declared, resolved, "examples"); ok && len(examples) > 0 {
		return examples[0], nil
	}
	if value, ok := schemaValue(declared, resolved, "default"); ok {
		return value, nil
	}
	if value, ok := schemaValue(declared, resolved, "const"); ok {
		return value, nil
	}
	if values, ok := schemaSlice(declared, resolved, "enum"); ok && len(values) > 0 {
		return values[0], nil
	}

	schemaType, _ := schemaValue(declared, resolved, "type")
	switch primarySchemaType(schemaType, resolved) {
	case "object":
		example := make(map[string]any)
		properties, _ := resolved["properties"].(map[string]any)
		required, _ := resolved["required"].([]any)
		for _, nameValue := range required {
			name, ok := nameValue.(string)
			if !ok {
				continue
			}
			child, ok := properties[name].(map[string]any)
			if !ok {
				continue
			}
			childDoc, childResolved, err := r.resolve(doc, child)
			if err != nil {
				return nil, err
			}
			example[name], err = r.schemaExample(childDoc, child, childResolved, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return example, nil
	case "array":
		if maximum, ok := schemaNumber(resolved, "maxItems"); ok && maximum == 0 {
			return []any{}, nil
		}
		items, ok := resolved["items"].(map[string]any)
		if !ok {
			return []any{}, nil
		}
		itemDoc, itemResolved, err := r.resolve(doc, items)
		if err != nil {
			return nil, err
		}
		item, err := r.schemaExample(itemDoc, items, itemResolved, depth+1)
		if err != nil {
			return nil, err
		}
		count := 1
		if minimum, ok := schemaNumber(resolved, "minItems"); ok && minimum > 1 {
			count = int(minimum)
		}
		example := make([]any, count)
		for i := range example {
			example[i] = item
		}
		return example, nil
	case "string":
		switch format, _ := resolved["format"].(string); format {
		case "uri":
			return "https://example.invalid", nil
		case "date-time":
			return "1970-01-01T00:00:00Z", nil
		}
		length := 1
		if minimum, ok := schemaNumber(resolved, "minLength"); ok && minimum > 1 {
			length = int(minimum)
		}
		return strings.Repeat("x", length), nil
	case "integer":
		if minimum, ok := schemaValue(declared, resolved, "minimum"); ok {
			return minimum, nil
		}
		return json.Number("0"), nil
	case "number":
		if minimum, ok := schemaValue(declared, resolved, "minimum"); ok {
			return minimum, nil
		}
		return json.Number("0"), nil
	case "boolean":
		return false, nil
	case "null":
		return nil, nil
	default:
		if alternatives, ok := resolved["oneOf"].([]any); ok {
			for _, alternative := range alternatives {
				node, ok := alternative.(map[string]any)
				if !ok {
					continue
				}
				alternativeDoc, alternativeResolved, err := r.resolve(doc, node)
				if err != nil {
					return nil, err
				}
				return r.schemaExample(alternativeDoc, node, alternativeResolved, depth+1)
			}
		}
		return map[string]any{}, nil
	}
}

func primarySchemaType(value any, resolved map[string]any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "null" {
				return text
			}
		}
	}
	if _, ok := resolved["properties"]; ok {
		return "object"
	}
	if _, ok := resolved["items"]; ok {
		return "array"
	}
	return ""
}

func jsonValueType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		number := string(value.(json.Number))
		if !strings.ContainsAny(number, ".eE") {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return ""
	}
}

func schemaNumber(node map[string]any, key string) (float64, bool) {
	value, ok := node[key]
	if !ok {
		return 0, false
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Float64()
	return parsed, err == nil
}

func schemaAllowedValues(declared, resolved map[string]any) []any {
	if values, ok := schemaSlice(declared, resolved, "enum"); ok {
		return values
	}
	if value, ok := schemaValue(declared, resolved, "const"); ok {
		return []any{value}
	}
	return nil
}

func parseSelector(selector string) ([]selectorPart, error) {
	if selector == "" || selector != strings.TrimSpace(selector) {
		return nil, unknownSelector(selector)
	}
	if strings.Contains(selector, ".") && strings.Contains(selector, "/") {
		return nil, unknownSelector(selector)
	}
	separator := "."
	if strings.Contains(selector, "/") {
		separator = "/"
		selector = strings.TrimPrefix(selector, "/")
	}
	rawParts := strings.Split(selector, separator)
	parts := make([]selectorPart, len(rawParts))
	for i, raw := range rawParts {
		if raw == "" {
			return nil, unknownSelector(selector)
		}
		element := strings.HasSuffix(raw, "[]")
		name := strings.TrimSuffix(raw, "[]")
		if name == "" || strings.ContainsAny(name, "[]") || (i == 0 && element) {
			return nil, unknownSelector(selector)
		}
		parts[i] = selectorPart{name: name, element: element}
	}
	return parts, nil
}

func unknownSelector(selector string) error {
	return fmt.Errorf("%w %q", ErrUnknownSelector, selector)
}

func (r *registry) load(kind string) (*schemaDocument, error) {
	entry, ok := schemas.Lookup(kind)
	if !ok {
		return nil, unknownSelector(kind)
	}
	if doc, ok := r.documents[entry.Kind]; ok {
		return doc, nil
	}
	raw, err := schemas.FS.ReadFile(entry.File)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode embedded schema %s: %w", entry.File, err)
	}
	doc := &schemaDocument{entry: entry, root: root}
	r.documents[entry.Kind] = doc
	return doc, nil
}

func (r *registry) resolve(
	doc *schemaDocument,
	node map[string]any,
) (*schemaDocument, map[string]any, error) {
	seen := make(map[string]bool)
	for {
		ref, ok := node["$ref"].(string)
		if !ok {
			return doc, node, nil
		}
		key := doc.entry.File + ":" + ref
		if seen[key] {
			return nil, nil, errors.New("schema reference cycle")
		}
		seen[key] = true

		file, fragment, _ := strings.Cut(ref, "#")
		if file != "" {
			file = strings.TrimPrefix(file, schemas.BaseURI)
			entry, ok := schemas.Lookup(strings.TrimSuffix(file, ".schema.json"))
			if !ok {
				return nil, nil, fmt.Errorf("unknown schema reference %q", ref)
			}
			var err error
			doc, err = r.load(entry.Kind)
			if err != nil {
				return nil, nil, err
			}
		}
		if fragment == "" {
			node = doc.root
			continue
		}
		var err error
		node, err = resolveJSONPointer(doc.root, fragment)
		if err != nil {
			return nil, nil, err
		}
	}
}

func resolveJSONPointer(root map[string]any, pointer string) (map[string]any, error) {
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("unsupported schema reference fragment %q", pointer)
	}
	var current any = root
	for _, encoded := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("schema reference fragment %q is not an object", pointer)
		}
		current, ok = object[segment]
		if !ok {
			return nil, fmt.Errorf("schema reference fragment %q does not exist", pointer)
		}
	}
	node, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema reference fragment %q is not an object", pointer)
	}
	return node, nil
}

func schemaValue(declared, resolved map[string]any, key string) (any, bool) {
	if value, ok := declared[key]; ok {
		return value, true
	}
	value, ok := resolved[key]
	return value, ok
}

func schemaString(declared, resolved map[string]any, key string) (string, bool) {
	value, ok := schemaValue(declared, resolved, key)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func schemaSlice(declared, resolved map[string]any, key string) ([]any, bool) {
	value, ok := schemaValue(declared, resolved, key)
	if !ok {
		return nil, false
	}
	values, ok := value.([]any)
	return values, ok
}

func containsString(value any, target string) bool {
	values, ok := value.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
