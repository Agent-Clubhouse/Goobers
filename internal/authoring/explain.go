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
)

// ErrUnknownSelector identifies a selector absent from the built-in schemas.
var ErrUnknownSelector = errors.New("unknown selector")

// Explanation contains only facts explicitly encoded in a schema. Optional
// fields remain absent when the schema does not provide them.
type Explanation struct {
	Selector      string `json:"selector"`
	Documented    bool   `json:"documented"`
	Description   string `json:"description,omitempty"`
	Type          any    `json:"type,omitempty"`
	AllowedValues []any  `json:"allowedValues,omitempty"`
	Default       *any   `json:"default,omitempty"`
	Required      *bool  `json:"required,omitempty"`
	SinceVersion  string `json:"sinceVersion,omitempty"`
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
	}
	explanation.AllowedValues = schemaAllowedValues(declared, resolved)
	if value, ok := schemaValue(declared, resolved, "default"); ok {
		explanation.Default = &value
	}
	explanation.SinceVersion, _ = schemaString(declared, resolved, "sinceVersion")
	return explanation
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
