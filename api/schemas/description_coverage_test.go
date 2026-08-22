package schemas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type descriptionCoverage struct {
	total         int
	undocumented  []string
	undocPathHash string
}

type legacyUndocumented struct {
	count    int
	pathHash string
}

var authorFacingSchemaRoots = map[string]bool{
	"gaggle.schema.json":     true,
	"goober.schema.json":     true,
	"instance.schema.json":   true,
	"invocation.schema.json": true,
	"manifest.schema.json":   true,
	"workflow.schema.json":   true,
}

// Fingerprinting the existing non-author gaps lets this test reject every new
// undocumented path without expanding this issue into unrelated contract prose.
var legacyUndocumentedSchemas = map[string]legacyUndocumented{
	"agent-toolkit-manifest.schema.json": {30, "5e796d83e2044f720341082f13f69e2f241b23cc87d9ddc7365926948f4bc70f"},
	"candidate-findings-v1.schema.json":  {9, "600935ca4f0646452c0d0970b304422d92b404ad6acb2304a81eaf98d89e1c39"},
	"diagnostics.schema.json":            {15, "a3f0cbff08fe67441b672c651d5a5d46f62ddeff43288288021a7ee9d6445362"},
	"features.schema.json":               {9, "5a63e854710cf8efb3add20719ff1900bd99cf314d05e6abc17fa6fb0552a391"},
	"journal-event.schema.json":          {11, "9db2f2e2a6a2686ddd5a06a4e3e3f32b538a4af4afac21b57d988eefeae10511"},
	"journal-run.schema.json":            {0, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	"remediation-brief-v1.schema.json":   {53, "68b9a4dd55e1be7fca8b1093698a07791e47914e1dd5c3f02c81f15f07101529"},
	"remediation-brief-v2.schema.json":   {68, "489a2a169f844fc77bc359b75627839a7b43520cabe2c068c0d9e9cdbf52996c"},
	"result.schema.json":                 {8, "1a5331ef336ceae49591b15e92f0bbd7a7a2c7aec005de93077d9d54d3d68f40"},
}

func TestDescriptionCoverage(t *testing.T) {
	files, err := fs.Glob(FS, "*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded schemas found")
	}

	decoded := make(map[string]map[string]any, len(files))
	for _, name := range files {
		data, err := FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		decoded[name] = schema
	}

	authorFacingSchemas := expandAuthorFacingSchemas(t, decoded)
	var report strings.Builder
	fmt.Fprintln(&report, "JSON Schema description coverage:")
	for _, name := range files {
		coverage := measureDescriptionCoverage(decoded[name])
		documented := coverage.total - len(coverage.undocumented)
		fmt.Fprintf(&report, "  %-42s %3d/%3d documented (%5.1f%%)\n",
			name, documented, coverage.total, float64(documented)*100/float64(coverage.total))

		if authorFacingSchemas[name] {
			if len(coverage.undocumented) != 0 {
				t.Errorf("%s must document every author-facing field; missing descriptions:\n%s",
					name, strings.Join(coverage.undocumented, "\n"))
			}
			continue
		}

		legacy, ok := legacyUndocumentedSchemas[name]
		if !ok {
			if len(coverage.undocumented) != 0 {
				t.Errorf("%s is a new schema with undocumented fields:\n%s",
					name, strings.Join(coverage.undocumented, "\n"))
			}
			continue
		}
		if len(coverage.undocumented) != legacy.count || coverage.undocPathHash != legacy.pathHash {
			t.Errorf("%s undocumented-field baseline changed; describe new fields and update the baseline only when legacy fields are documented\nmissing descriptions:\n%s",
				name, strings.Join(coverage.undocumented, "\n"))
		}
	}
	fmt.Print(report.String())
}

func TestDescriptionCoverageIncludesConditionalOnlyFields(t *testing.T) {
	for _, keyword := range []string{"if", "then", "else", "not"} {
		t.Run(keyword, func(t *testing.T) {
			schema := map[string]any{
				"allOf": []any{
					map[string]any{
						keyword: map[string]any{
							"properties": map[string]any{
								"conditionalField": map[string]any{"type": "string"},
							},
						},
					},
				},
			}

			coverage := measureDescriptionCoverage(schema)
			if coverage.total != 1 {
				t.Fatalf("total = %d, want 1", coverage.total)
			}
			want := []string{"/allOf/0/" + keyword + "/properties/conditionalField"}
			if !reflect.DeepEqual(coverage.undocumented, want) {
				t.Fatalf("undocumented = %v, want %v", coverage.undocumented, want)
			}
		})
	}
}

func TestDescriptionCoverageDeduplicatesReferencedConditionalConstraints(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"triggers": map[string]any{
				"description": "Workflow triggers.",
				"items":       map[string]any{"$ref": "#/$defs/trigger"},
				"allOf": []any{
					map[string]any{
						"if": map[string]any{
							"contains": map[string]any{
								"properties": map[string]any{
									"type": map[string]any{"const": "manual"},
								},
							},
						},
					},
				},
			},
		},
		"$defs": map[string]any{
			"trigger": map[string]any{
				"properties": map[string]any{
					"type": map[string]any{"description": "Trigger type."},
				},
			},
		},
	}

	coverage := measureDescriptionCoverage(schema)
	if coverage.total != 2 {
		t.Fatalf("total = %d, want 2", coverage.total)
	}
	if len(coverage.undocumented) != 0 {
		t.Fatalf("undocumented = %v, want none", coverage.undocumented)
	}
}

func expandAuthorFacingSchemas(t *testing.T, decoded map[string]map[string]any) map[string]bool {
	t.Helper()

	authorFacing := make(map[string]bool, len(authorFacingSchemaRoots))
	queue := sortedKeys(authorFacingSchemaRoots)
	for _, name := range queue {
		authorFacing[name] = true
	}

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		schema, ok := decoded[name]
		if !ok {
			t.Fatalf("author-facing schema %s is not embedded", name)
		}

		refs := make(map[string]bool)
		collectExternalSchemaRefs(schema, refs)
		for _, ref := range sortedKeys(refs) {
			if _, ok := decoded[ref]; !ok {
				t.Fatalf("author-facing schema %s references unembedded schema %s", name, ref)
			}
			if !authorFacing[ref] {
				authorFacing[ref] = true
				queue = append(queue, ref)
			}
		}
	}
	return authorFacing
}

func collectExternalSchemaRefs(node any, refs map[string]bool) {
	switch value := node.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok {
			target, _, _ := strings.Cut(ref, "#")
			target = strings.TrimPrefix(target, BaseURI)
			if target != "" {
				refs[target] = true
			}
		}
		for _, child := range value {
			collectExternalSchemaRefs(child, refs)
		}
	case []any:
		for _, child := range value {
			collectExternalSchemaRefs(child, refs)
		}
	}
}

func measureDescriptionCoverage(schema map[string]any) descriptionCoverage {
	var fields []schemaField
	collectSchemaFields(schema, "", "", false, &fields)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].path < fields[j].path
	})

	unconditionalPaths := make(map[string]bool)
	for _, field := range fields {
		if !field.conditional {
			unconditionalPaths[field.instancePath] = true
		}
	}
	collectReferencedUnconditionalPaths(schema, schema, "", false, make(map[string]bool), unconditionalPaths)

	measured := make([]schemaField, 0, len(fields))
	conditionalPaths := make(map[string]int)
	for _, field := range fields {
		if !field.conditional {
			measured = append(measured, field)
			continue
		}
		if unconditionalPaths[field.instancePath] {
			continue
		}
		if index, ok := conditionalPaths[field.instancePath]; ok {
			if measured[index].description == "" {
				measured[index].description = field.description
			}
			continue
		}
		conditionalPaths[field.instancePath] = len(measured)
		measured = append(measured, field)
	}

	coverage := descriptionCoverage{total: len(measured)}
	for _, field := range measured {
		if strings.TrimSpace(field.description) == "" {
			coverage.undocumented = append(coverage.undocumented, field.path)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(coverage.undocumented, "\n")))
	coverage.undocPathHash = hex.EncodeToString(sum[:])
	return coverage
}

type schemaField struct {
	path         string
	instancePath string
	description  string
	conditional  bool
}

func collectSchemaFields(node any, path, instancePath string, conditional bool, fields *[]schemaField) {
	switch value := node.(type) {
	case map[string]any:
		if properties, ok := value["properties"].(map[string]any); ok {
			names := sortedKeys(properties)
			for _, name := range names {
				fieldPath := pointerJoin(path, "properties", name)
				fieldInstancePath := pointerJoin(instancePath, name)
				field, _ := properties[name].(map[string]any)
				description, _ := field["description"].(string)
				*fields = append(*fields, schemaField{
					path:         fieldPath,
					instancePath: fieldInstancePath,
					description:  description,
					conditional:  conditional,
				})
				collectSchemaFields(properties[name], fieldPath, fieldInstancePath, conditional, fields)
			}
		}

		for _, key := range sortedKeys(value) {
			if key == "properties" {
				continue
			}
			childPath := pointerJoin(path, key)
			childInstancePath := instancePath
			childConditional := conditional
			switch key {
			case "$defs":
				definitions, _ := value[key].(map[string]any)
				for _, name := range sortedKeys(definitions) {
					collectSchemaFields(
						definitions[name],
						pointerJoin(childPath, name),
						pointerJoin("#/$defs", name),
						conditional,
						fields,
					)
				}
				continue
			case "items", "contains":
				childInstancePath = pointerJoin(instancePath, "[]")
			case "if", "then", "else", "not":
				childConditional = true
			}
			collectSchemaFields(value[key], childPath, childInstancePath, childConditional, fields)
		}
	case []any:
		for i, item := range value {
			collectSchemaFields(item, pointerJoin(path, strconv.Itoa(i)), instancePath, conditional, fields)
		}
	}
}

func collectReferencedUnconditionalPaths(
	root map[string]any,
	node any,
	instancePath string,
	conditional bool,
	resolving map[string]bool,
	paths map[string]bool,
) {
	switch value := node.(type) {
	case map[string]any:
		// Resolve definitions at their use-site instance path so a conditional
		// constraint can match the author-facing declaration behind a local ref.
		if ref, ok := value["$ref"].(string); ok && strings.HasPrefix(ref, "#/") && !resolving[ref] {
			if target, ok := resolveLocalRef(root, ref); ok {
				resolving[ref] = true
				collectReferencedUnconditionalPaths(root, target, instancePath, conditional, resolving, paths)
				delete(resolving, ref)
			}
		}

		if properties, ok := value["properties"].(map[string]any); ok {
			for _, name := range sortedKeys(properties) {
				fieldInstancePath := pointerJoin(instancePath, name)
				if !conditional {
					paths[fieldInstancePath] = true
				}
				collectReferencedUnconditionalPaths(
					root,
					properties[name],
					fieldInstancePath,
					conditional,
					resolving,
					paths,
				)
			}
		}

		for _, key := range sortedKeys(value) {
			if key == "properties" || key == "$defs" || key == "$ref" {
				continue
			}
			childInstancePath := instancePath
			childConditional := conditional
			switch key {
			case "items", "contains":
				childInstancePath = pointerJoin(instancePath, "[]")
			case "if", "then", "else", "not":
				childConditional = true
			}
			collectReferencedUnconditionalPaths(
				root,
				value[key],
				childInstancePath,
				childConditional,
				resolving,
				paths,
			)
		}
	case []any:
		for _, item := range value {
			collectReferencedUnconditionalPaths(root, item, instancePath, conditional, resolving, paths)
		}
	}
}

func resolveLocalRef(root map[string]any, ref string) (any, bool) {
	var current any = root
	for _, component := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		component = strings.ReplaceAll(component, "~1", "/")
		component = strings.ReplaceAll(component, "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[component]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func pointerJoin(path string, parts ...string) string {
	for _, part := range parts {
		part = strings.ReplaceAll(part, "~", "~0")
		part = strings.ReplaceAll(part, "/", "~1")
		path += "/" + part
	}
	return path
}
