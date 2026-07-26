package schemas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
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

var authorFacingSchemas = map[string]bool{
	"gaggle.schema.json":     true,
	"goober.schema.json":     true,
	"invocation.schema.json": true,
	"workflow.schema.json":   true,
}

// Fingerprinting the existing non-author gaps lets this test reject every new
// undocumented path without expanding this issue into unrelated contract prose.
var legacyUndocumentedSchemas = map[string]legacyUndocumented{
	"agent-toolkit-manifest.schema.json": {30, "5e796d83e2044f720341082f13f69e2f241b23cc87d9ddc7365926948f4bc70f"},
	"artifact-pointer.schema.json":       {2, "1f9513580952a5c043ce6d8502e65b7683c1ebe26842f4c366d12bcd47fdfd24"},
	"candidate-findings-v1.schema.json":  {9, "600935ca4f0646452c0d0970b304422d92b404ad6acb2304a81eaf98d89e1c39"},
	"diagnostics.schema.json":            {15, "a3f0cbff08fe67441b672c651d5a5d46f62ddeff43288288021a7ee9d6445362"},
	"features.schema.json":               {9, "5a63e854710cf8efb3add20719ff1900bd99cf314d05e6abc17fa6fb0552a391"},
	"journal-event.schema.json":          {11, "9db2f2e2a6a2686ddd5a06a4e3e3f32b538a4af4afac21b57d988eefeae10511"},
	"journal-run.schema.json":            {17, "c092e8176ef9f8ff56862fb8743991f3e2e74eb0f67064fe2885439381173db3"},
	"manifest.schema.json":               {20, "d7dc7d59b173f9de8e56efc93034723c5420ee583a93713dbb7d9b54f99b2832"},
	"remediation-brief-v1.schema.json":   {53, "68b9a4dd55e1be7fca8b1093698a07791e47914e1dd5c3f02c81f15f07101529"},
	"remediation-brief-v2.schema.json":   {68, "489a2a169f844fc77bc359b75627839a7b43520cabe2c068c0d9e9cdbf52996c"},
	"result.schema.json":                 {8, "1a5331ef336ceae49591b15e92f0bbd7a7a2c7aec005de93077d9d54d3d68f40"},
	"verdict.schema.json":                {8, "58bf8671aec1c9d6aebe05614c4edda155cbc493834128d8274213f05dece68e"},
}

func TestDescriptionCoverage(t *testing.T) {
	files, err := fs.Glob(FS, "*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded schemas found")
	}

	t.Log("JSON Schema description coverage:")
	for _, name := range files {
		data, err := FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}

		coverage := measureDescriptionCoverage(schema)
		documented := coverage.total - len(coverage.undocumented)
		t.Logf("  %-42s %3d/%3d documented (%5.1f%%)",
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
}

func measureDescriptionCoverage(schema map[string]any) descriptionCoverage {
	var fields []schemaField
	collectSchemaFields(schema, "", &fields)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].path < fields[j].path
	})

	coverage := descriptionCoverage{total: len(fields)}
	for _, field := range fields {
		if strings.TrimSpace(field.description) == "" {
			coverage.undocumented = append(coverage.undocumented, field.path)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(coverage.undocumented, "\n")))
	coverage.undocPathHash = hex.EncodeToString(sum[:])
	return coverage
}

type schemaField struct {
	path        string
	description string
}

func collectSchemaFields(node any, path string, fields *[]schemaField) {
	switch value := node.(type) {
	case map[string]any:
		if properties, ok := value["properties"].(map[string]any); ok {
			names := sortedKeys(properties)
			for _, name := range names {
				fieldPath := pointerJoin(path, "properties", name)
				field, _ := properties[name].(map[string]any)
				description, _ := field["description"].(string)
				*fields = append(*fields, schemaField{path: fieldPath, description: description})
				collectSchemaFields(properties[name], fieldPath, fields)
			}
		}

		for _, key := range sortedKeys(value) {
			// Conditional clauses constrain fields declared elsewhere; they do
			// not introduce additional author-visible fields.
			switch key {
			case "properties", "if", "then", "else", "not":
				continue
			}
			collectSchemaFields(value[key], pointerJoin(path, key), fields)
		}
	case []any:
		for i, item := range value {
			collectSchemaFields(item, pointerJoin(path, strconv.Itoa(i)), fields)
		}
	}
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
