// Package authoring projects release-pinned facts from the JSON Schemas
// embedded in the running binary.
package authoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

// ErrUnknownSelector identifies a selector absent from the built-in schemas.
var ErrUnknownSelector = errors.New("unknown selector")

// ErrUnavailableSelector identifies a schema selector absent from the built-in
// DSL version.
var ErrUnavailableSelector = errors.New("unavailable selector")

// Explanation combines field facts from an embedded schema with lifecycle
// metadata from the built-in schema and DSL feature registries.
type Explanation struct {
	Selector      string `json:"selector"`
	Description   string `json:"description"`
	Type          any    `json:"type"`
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
	parts[0].name = contractEntry.Kind

	declared := doc.root
	resolved := doc.root
	currentDoc := doc
	var required *bool

	for _, part := range parts[1:] {
		childDoc, child, childResolved, isRequired, found, resolveErr :=
			r.resolveProperty(currentDoc, resolved, part.name, 0)
		if resolveErr != nil || !found {
			return Explanation{}, unknownSelector(selector)
		}
		required = &isRequired
		declared = child
		currentDoc, resolved = childDoc, childResolved

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
	explanation.Type, err = r.schemaType(currentDoc, declared, resolved, 0)
	if err != nil {
		return Explanation{}, fmt.Errorf("explain %q: %w", selector, err)
	}
	if explanation.AllowedValues == nil {
		if items, ok := resolved["items"].(map[string]any); ok {
			_, item, resolveErr := r.resolve(currentDoc, items)
			if resolveErr != nil {
				return Explanation{}, unknownSelector(selector)
			}
			explanation.AllowedValues = schemaAllowedValues(items, item)
		}
	}
	explanation.Stability, explanation.SinceVersion, err = selectorLifecycle(selector, parts, contractEntry)
	if err != nil {
		return Explanation{}, err
	}
	explanation.AllowedValues, err = selectorAllowedValues(parts, explanation.AllowedValues)
	if err != nil {
		return Explanation{}, fmt.Errorf("explain %q: %w", selector, err)
	}
	explanation.Example, err = r.schemaExample(currentDoc, declared, resolved, 0)
	if err != nil {
		return Explanation{}, fmt.Errorf("explain %q: %w", selector, err)
	}
	if err := validateSchemaExample(currentDoc, resolved, explanation.Example); err != nil {
		return Explanation{}, fmt.Errorf("explain %q generated an invalid example: %w", selector, err)
	}
	return explanation, nil
}

func projectFacts(selector string, declared, resolved map[string]any, required *bool) Explanation {
	explanation := Explanation{Selector: selector, Required: required}
	description, ok := schemaString(declared, resolved, "description")
	if !ok || strings.TrimSpace(description) == "" {
		normalized := strings.ReplaceAll(selector, "/", ".")
		parts := strings.Split(normalized, ".")
		description = fmt.Sprintf(
			"Defines the %s field in the built-in %s contract.",
			strings.TrimSuffix(parts[len(parts)-1], "[]"),
			parts[0],
		)
	}
	explanation.Description = description
	explanation.AllowedValues = schemaAllowedValues(declared, resolved)
	if value, ok := schemaValue(declared, resolved, "default"); ok {
		explanation.Default = &value
	}
	explanation.SinceVersion, _ = schemaString(declared, resolved, "sinceVersion")
	return explanation
}

func selectorLifecycle(requestedSelector string, parts []selectorPart, entry schemas.Entry) (string, string, error) {
	selector := normalizedSelector(parts)
	allFeatures := workflow.AllFeatures()
	versionFeatures, err := workflow.FeaturesAtDSLVersion(allFeatures, supportmatrix.CurrentDSLVersion)
	if err != nil {
		return "", "", fmt.Errorf("resolve built-in DSL features: %w", err)
	}

	for _, candidate := range selectorFeatureCandidates(selector) {
		if feature, ok := lookupFeature(versionFeatures, candidate); ok {
			return string(feature.Level), feature.SinceVersion, nil
		}
		if stability, sinceVersion, ok := featurePrefixLifecycle(candidate, versionFeatures); ok {
			return stability, sinceVersion, nil
		}
		if featureCandidateKnown(versionFeatures, candidate) {
			continue
		}
		if featureCandidateKnown(allFeatures, candidate) {
			return "", "", unavailableSelector(requestedSelector)
		}
	}
	for parent := selector; strings.Contains(parent, "."); {
		parent = parent[:strings.LastIndex(parent, ".")]
		if feature, ok := lookupFeature(versionFeatures, parent); ok {
			return string(feature.Level), feature.SinceVersion, nil
		}
		if _, ok := lookupFeature(allFeatures, parent); ok {
			return "", "", unavailableSelector(requestedSelector)
		}
	}
	return entry.Stability, entry.SinceVersion, nil
}

func selectorFeatureCandidates(selector string) []string {
	candidates := []string{selector}
	switch {
	case strings.HasPrefix(selector, "workflow.spec.tasks."):
		suffix := strings.TrimPrefix(selector, "workflow.spec.tasks.")
		switch {
		case suffix == "run" || strings.HasPrefix(suffix, "run."):
			candidates = append(candidates, "stage."+suffix)
		case suffix == "workspace":
			candidates = append(candidates, "stage.workspace")
		default:
			candidates = append(candidates, "task."+suffix)
		}
	case strings.HasPrefix(selector, "workflow.spec.gates."):
		suffix := strings.TrimPrefix(selector, "workflow.spec.gates.")
		switch {
		case suffix == "automated" || strings.HasPrefix(suffix, "automated."),
			suffix == "agentic" || strings.HasPrefix(suffix, "agentic."),
			suffix == "human" || strings.HasPrefix(suffix, "human."):
			candidates = append(candidates, "gate.evaluator."+suffix)
		default:
			candidates = append(candidates, "gate."+suffix)
		}
	case strings.HasPrefix(selector, "workflow.spec.triggers."):
		candidates = append(candidates, "trigger."+strings.TrimPrefix(selector, "workflow.spec.triggers."))
	}
	return candidates
}

func selectorAllowedValues(parts []selectorPart, values []any) ([]any, error) {
	if normalizedSelector(parts) != "workflow.spec.tasks.run.workspace" || values == nil {
		return values, nil
	}
	features, err := workflow.FeaturesAtDSLVersion(workflow.AllFeatures(), supportmatrix.CurrentDSLVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve built-in DSL features: %w", err)
	}
	filtered := make([]any, 0, len(values))
	for _, value := range values {
		workspace, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("run.workspace schema value %#v is not a string", value)
		}
		var featureID string
		switch workspace {
		case "repo", "scratch":
			featureID = "stage.run.workspace." + workspace
		case "repo-readonly":
			featureID = "stage.workspace.repo-readonly"
		default:
			return nil, fmt.Errorf("run.workspace schema value %q has no feature mapping", workspace)
		}
		if _, ok := lookupFeature(features, featureID); ok {
			filtered = append(filtered, value)
		}
	}
	return filtered, nil
}

func lookupFeature(features []workflow.Feature, id string) (workflow.Feature, bool) {
	for _, feature := range features {
		if feature.ID == workflow.FeatureID(id) {
			return feature, true
		}
	}
	return workflow.Feature{}, false
}

func featureCandidateKnown(features []workflow.Feature, candidate string) bool {
	if _, ok := lookupFeature(features, candidate); ok {
		return true
	}
	for _, feature := range features {
		if strings.HasPrefix(string(feature.ID), candidate+".") {
			return true
		}
	}
	return false
}

func featurePrefixLifecycle(prefix string, features []workflow.Feature) (string, string, bool) {
	var stability, sinceVersion string
	found := false
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
		if err := r.applyObjectConstraints(doc, resolved, resolved, example, depth); err != nil {
			return nil, err
		}
		minimum, _ := schemaNumber(resolved, "minProperties")
		if len(example) < int(minimum) {
			properties, _ := resolved["properties"].(map[string]any)
			names := make([]string, 0, len(properties))
			for name := range properties {
				if _, exists := example[name]; !exists {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			for _, name := range names {
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
				if len(example) >= int(minimum) {
					break
				}
			}
		}
		if len(example) < int(minimum) {
			additional, ok := resolved["additionalProperties"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("cannot satisfy minProperties %d", int(minimum))
			}
			additionalDoc, additionalResolved, err := r.resolve(doc, additional)
			if err != nil {
				return nil, err
			}
			for len(example) < int(minimum) {
				name := fmt.Sprintf("key%d", len(example)+1)
				if _, exists := example[name]; exists {
					continue
				}
				example[name], err = r.schemaExample(
					additionalDoc,
					additional,
					additionalResolved,
					depth+1,
				)
				if err != nil {
					return nil, err
				}
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
		if pattern, ok := schemaString(declared, resolved, "pattern"); ok {
			example, err := minimalPatternExample(pattern)
			if err != nil {
				return nil, err
			}
			return example, nil
		}
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

func (r *registry) applyObjectConstraints(
	doc *schemaDocument,
	base, constraints map[string]any,
	example map[string]any,
	depth int,
) error {
	properties, _ := base["properties"].(map[string]any)
	required, _ := constraints["required"].([]any)
	for _, nameValue := range required {
		name, ok := nameValue.(string)
		if !ok {
			continue
		}
		child, ok := properties[name].(map[string]any)
		if !ok {
			return fmt.Errorf("required property %q has no schema", name)
		}
		if constrainedProperties, ok := constraints["properties"].(map[string]any); ok {
			if constrained, ok := constrainedProperties[name].(map[string]any); ok {
				child = constrained
			}
		}
		childDoc, childResolved, err := r.resolve(doc, child)
		if err != nil {
			return err
		}
		example[name], err = r.schemaExample(childDoc, child, childResolved, depth+1)
		if err != nil {
			return err
		}
	}

	if dependent, ok := constraints["dependentRequired"].(map[string]any); ok {
		for name, dependenciesValue := range dependent {
			if _, present := example[name]; !present {
				continue
			}
			dependencies, ok := dependenciesValue.([]any)
			if !ok {
				continue
			}
			if err := r.applyObjectConstraints(
				doc,
				base,
				map[string]any{"required": dependencies},
				example,
				depth+1,
			); err != nil {
				return err
			}
		}
	}

	for _, keyword := range []string{"oneOf", "anyOf"} {
		alternatives, ok := constraints[keyword].([]any)
		if !ok || len(alternatives) == 0 {
			continue
		}
		alternative, ok := alternatives[0].(map[string]any)
		if !ok {
			continue
		}
		if err := r.applyObjectConstraints(doc, base, alternative, example, depth+1); err != nil {
			return err
		}
		break
	}

	allOf, _ := constraints["allOf"].([]any)
	for _, conjunctValue := range allOf {
		conjunct, ok := conjunctValue.(map[string]any)
		if !ok {
			continue
		}
		selected := conjunct
		if condition, ok := conjunct["if"].(map[string]any); ok {
			branch := "else"
			if objectMatches(condition, example) {
				branch = "then"
			}
			selected, _ = conjunct[branch].(map[string]any)
			if selected == nil {
				continue
			}
		}
		if err := r.applyObjectConstraints(doc, base, selected, example, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func objectMatches(schema map[string]any, value map[string]any) bool {
	if required, ok := schema["required"].([]any); ok {
		for _, name := range required {
			text, ok := name.(string)
			if !ok {
				continue
			}
			if _, present := value[text]; !present {
				return false
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, constraintValue := range properties {
		actual, present := value[name]
		if !present {
			continue
		}
		constraint, ok := constraintValue.(map[string]any)
		if !ok {
			continue
		}
		if expected, ok := constraint["const"]; ok && !reflectJSONEqual(actual, expected) {
			return false
		}
		if values, ok := constraint["enum"].([]any); ok {
			matched := false
			for _, expected := range values {
				if reflectJSONEqual(actual, expected) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

func reflectJSONEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func validateSchemaExample(doc *schemaDocument, selected map[string]any, example any) error {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	for _, file := range schemas.Files() {
		raw, err := schemas.FS.ReadFile(file)
		if err != nil {
			return err
		}
		if err := compiler.AddResource(schemas.BaseURI+file, bytes.NewReader(raw)); err != nil {
			return err
		}
	}
	standalone := maps.Clone(selected)
	for _, key := range []string{"$id", "$schema", "$defs"} {
		delete(standalone, key)
	}
	standalone["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	if definitions, ok := doc.root["$defs"]; ok {
		standalone["$defs"] = definitions
	}
	raw, err := json.Marshal(standalone)
	if err != nil {
		return err
	}
	const resource = schemas.BaseURI + "__explain-example.schema.json"
	if err := compiler.AddResource(resource, bytes.NewReader(raw)); err != nil {
		return err
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return err
	}
	return compiled.Validate(example)
}

func (r *registry) schemaType(
	doc *schemaDocument,
	declared, resolved map[string]any,
	depth int,
) (any, error) {
	if depth > 32 {
		return nil, errors.New("schema nesting exceeds type depth")
	}
	if value, ok := schemaValue(declared, resolved, "type"); ok {
		return value, nil
	}
	if value, ok := schemaValue(declared, resolved, "const"); ok {
		return jsonValueType(value), nil
	}
	var types []string
	if values, ok := schemaSlice(declared, resolved, "enum"); ok && len(values) > 0 {
		for _, value := range values {
			types = appendUniqueString(types, jsonValueType(value))
		}
	} else if _, ok := resolved["properties"]; ok {
		types = []string{"object"}
	} else if _, ok := resolved["items"]; ok {
		types = []string{"array"}
	} else {
		for _, keyword := range []string{"oneOf", "anyOf"} {
			alternatives, ok := schemaSlice(declared, resolved, keyword)
			if !ok {
				continue
			}
			for _, alternative := range alternatives {
				node, ok := alternative.(map[string]any)
				if !ok {
					continue
				}
				alternativeDoc, alternativeResolved, err := r.resolve(doc, node)
				if err != nil {
					return nil, err
				}
				value, err := r.schemaType(alternativeDoc, node, alternativeResolved, depth+1)
				if err != nil {
					return nil, err
				}
				types = appendTypeValue(types, value)
			}
			break
		}
	}
	if types == nil {
		return []any{"null", "boolean", "object", "array", "number", "string", "integer"}, nil
	}
	return projectedTypes(types)
}

func appendTypeValue(types []string, value any) []string {
	switch typed := value.(type) {
	case string:
		return appendUniqueString(types, typed)
	case []any:
		for _, value := range typed {
			if text, ok := value.(string); ok {
				types = appendUniqueString(types, text)
			}
		}
	}
	return types
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	if value != "" {
		values = append(values, value)
	}
	return values
}

func projectedTypes(types []string) (any, error) {
	switch len(types) {
	case 0:
		return nil, errors.New("schema composition does not imply a JSON type")
	case 1:
		return types[0], nil
	default:
		values := make([]any, len(types))
		for i, schemaType := range types {
			values[i] = schemaType
		}
		return values, nil
	}
}

func minimalPatternExample(pattern string) (string, error) {
	expression, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", fmt.Errorf("parse pattern %q: %w", pattern, err)
	}
	example, err := minimalRegexpMatch(expression.Simplify())
	if err != nil {
		return "", fmt.Errorf("generate example for pattern %q: %w", pattern, err)
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("compile pattern %q: %w", pattern, err)
	}
	if !compiled.MatchString(example) {
		return "", fmt.Errorf("generated example %q does not match pattern %q", example, pattern)
	}
	return example, nil
}

func minimalRegexpMatch(expression *syntax.Regexp) (string, error) {
	switch expression.Op {
	case syntax.OpNoMatch:
		return "", errors.New("pattern cannot match any string")
	case syntax.OpEmptyMatch,
		syntax.OpBeginLine,
		syntax.OpEndLine,
		syntax.OpBeginText,
		syntax.OpEndText,
		syntax.OpWordBoundary,
		syntax.OpNoWordBoundary:
		return "", nil
	case syntax.OpLiteral:
		return string(expression.Rune), nil
	case syntax.OpCharClass:
		character, ok := printableClassRune(expression.Rune)
		if !ok {
			return "", errors.New("character class has no supported rune")
		}
		return string(character), nil
	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return "a", nil
	case syntax.OpCapture, syntax.OpPlus:
		return minimalRegexpMatch(expression.Sub[0])
	case syntax.OpConcat, syntax.OpAlternate:
		values := make([]string, len(expression.Sub))
		for i, subexpression := range expression.Sub {
			value, err := minimalRegexpMatch(subexpression)
			if err != nil {
				return "", err
			}
			values[i] = value
		}
		if expression.Op == syntax.OpConcat {
			return strings.Join(values, ""), nil
		}
		sort.Slice(values, func(i, j int) bool {
			return len(values[i]) < len(values[j]) ||
				(len(values[i]) == len(values[j]) && values[i] < values[j])
		})
		return values[0], nil
	case syntax.OpQuest, syntax.OpStar:
		return "", nil
	case syntax.OpRepeat:
		value, err := minimalRegexpMatch(expression.Sub[0])
		if err != nil {
			return "", err
		}
		return strings.Repeat(value, expression.Min), nil
	default:
		return "", fmt.Errorf("unsupported regular expression operation %s", expression.Op)
	}
}

func printableClassRune(ranges []rune) (rune, bool) {
	for _, candidate := range "a0x1A._/-@=+" {
		for i := 0; i+1 < len(ranges); i += 2 {
			if candidate >= ranges[i] && candidate <= ranges[i+1] {
				return candidate, true
			}
		}
	}
	for i := 0; i+1 < len(ranges); i += 2 {
		lower := max(ranges[i], ' ')
		upper := min(ranges[i+1], '~')
		if lower <= upper {
			return lower, true
		}
	}
	return 0, false
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
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		number := string(typed)
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

func unavailableSelector(selector string) error {
	return fmt.Errorf(
		"%w %q in built-in DSL version %s",
		ErrUnavailableSelector,
		selector,
		supportmatrix.CurrentDSLVersion,
	)
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

func (r *registry) resolveProperty(
	doc *schemaDocument,
	node map[string]any,
	name string,
	depth int,
) (*schemaDocument, map[string]any, map[string]any, bool, bool, error) {
	if depth > 32 {
		return nil, nil, nil, false, false, errors.New("schema nesting exceeds selector depth")
	}
	doc, node, err := r.resolve(doc, node)
	if err != nil {
		return nil, nil, nil, false, false, err
	}
	if properties, ok := node["properties"].(map[string]any); ok {
		if child, ok := properties[name].(map[string]any); ok {
			childDoc, resolved, err := r.resolve(doc, child)
			return childDoc, child, resolved, containsString(node["required"], name), true, err
		}
	}
	for _, keyword := range []string{"oneOf", "anyOf"} {
		alternatives, _ := node[keyword].([]any)
		for _, value := range alternatives {
			alternative, ok := value.(map[string]any)
			if !ok {
				continue
			}
			childDoc, child, resolved, required, found, err :=
				r.resolveProperty(doc, alternative, name, depth+1)
			if err != nil || found {
				return childDoc, child, resolved, required, found, err
			}
		}
	}
	return nil, nil, nil, false, false, nil
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
