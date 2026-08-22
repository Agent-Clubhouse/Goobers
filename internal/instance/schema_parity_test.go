package instance

import (
	"bytes"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
)

// instance.yaml has two strict halves that can only drift apart, never paper
// over each other: LoadConfig decodes with DisallowUnknownFields, and the
// published api/schemas/instance.schema.json declares additionalProperties:
// false. The drift direction that bites is a Go field added without a schema
// edit — the daemon then accepts a field the published contract (`goobers
// schema instance`, editor validation, the validate CLI) rejects as unknown,
// so the operator-facing contract silently lies about what the product
// actually loads. The CRD manifests have a reflective guard for exactly this
// class (api/v1alpha1's TestCRDManifestsExposeEveryTypeField, written after
// inputsFrom/docsRoots/requiredCapabilities drifted for real); this is
// instance.yaml's twin, and until it existed nothing forced a new Config
// field into the schema.
//
// Like that twin, it intentionally checks field PRESENCE, not full schema
// equality: enum values, patterns, and the cold-start descriptions are pinned
// by the targeted assertions in api/schemas/instance_schema_test.go, and
// asserting the whole schema here would make every description edit a Go-test
// change.
func TestInstanceSchemaExposesEveryConfigField(t *testing.T) {
	schema := compileEmbeddedInstanceSchema(t)
	var missing []string
	collectMissingSchemaFields(reflect.TypeOf(Config{}), schema, "instance", &missing, map[reflect.Type]bool{})
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("instance.schema.json is missing %d field(s) the strict loader accepts:\n  %s\n\n"+
			"Add each field to api/schemas/instance.schema.json (repos[] execution settings live in "+
			"instance-repository-execution.schema.json). The addition is additive: the loader already accepts these fields.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// compileEmbeddedInstanceSchema compiles the embedded instance contract with
// every sibling schema registered, mirroring api/schemas' own
// compileInstanceSchema, so the cross-file $refs into
// instance-repository-execution.schema.json resolve.
func compileEmbeddedInstanceSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	for _, file := range schemas.Files() {
		raw, err := schemas.FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read embedded schema %s: %v", file, err)
		}
		if err := compiler.AddResource(schemas.BaseURI+file, bytes.NewReader(raw)); err != nil {
			t.Fatalf("register %s: %v", file, err)
		}
	}
	compiled, err := compiler.Compile(schemas.BaseURI + "instance.schema.json")
	if err != nil {
		t.Fatalf("compile instance schema: %v", err)
	}
	return compiled
}

// collectMissingSchemaFields walks a Go struct against a compiled JSON
// Schema, recording every json-visible field with no corresponding property.
// Recursion is guarded against self-referential types.
func collectMissingSchemaFields(goType reflect.Type, schema *jsonschema.Schema, path string, missing *[]string, seen map[reflect.Type]bool) {
	// Unwrap pointers AND collections, descending the schema in lockstep: a
	// []RepoRef must compare against the repo item schema and a
	// map[string]TokenRef against the additionalProperties value schema, not
	// be skipped for not being a struct. The CRD twin documents how getting
	// this wrong silently reduces the whole guard to top-level fields only.
unwrap:
	for goType != nil && schema != nil {
		switch goType.Kind() {
		case reflect.Pointer:
			goType = goType.Elem()
		case reflect.Slice, reflect.Array:
			goType = goType.Elem()
			schema = itemsSchema(schema, map[*jsonschema.Schema]bool{})
		case reflect.Map:
			goType = goType.Elem()
			schema = mapValueSchema(schema, map[*jsonschema.Schema]bool{})
		default:
			break unwrap
		}
	}
	if goType == nil || goType.Kind() != reflect.Struct || schema == nil {
		return
	}
	if seen[goType] {
		return
	}
	seen[goType] = true
	defer delete(seen, goType)

	for i := 0; i < goType.NumField(); i++ {
		field := goType.Field(i)
		if !field.IsExported() {
			continue
		}
		name, inline := configJSONFieldName(field)
		if inline {
			// An embedded struct with no name of its own contributes its
			// fields at this same object level, exactly as encoding/json does.
			collectMissingSchemaFields(field.Type, schema, path, missing, seen)
			continue
		}
		if name == "" {
			continue
		}
		property, ok := schemaProperty(schema, name, map[*jsonschema.Schema]bool{})
		if !ok {
			*missing = append(*missing, path+"."+name)
			continue
		}
		collectMissingSchemaFields(field.Type, property, path+"."+name, missing, seen)
	}
}

// configJSONFieldName returns the wire name encoding/json gives field, and
// whether the field inlines its members at the current level (an embedded
// struct with no name of its own). Unlike the CRD twin's helper it does not
// skip an untagged exported field: encoding/json marshals those under the Go
// field name and the strict loader accepts them, so the schema must carry
// them too.
func configJSONFieldName(field reflect.StructField) (name string, inline bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ = strings.Cut(tag, ",")
	if name != "" {
		return name, false
	}
	if field.Anonymous {
		return "", true
	}
	return field.Name, false
}

// schemaProperty resolves the subschema validating the named property,
// looking through the $ref indirections and applicator branches
// (allOf/anyOf/oneOf/if-then-else) instance.schema.json composes shapes with.
// visited guards against $ref cycles.
func schemaProperty(schema *jsonschema.Schema, name string, visited map[*jsonschema.Schema]bool) (*jsonschema.Schema, bool) {
	if schema == nil || visited[schema] {
		return nil, false
	}
	visited[schema] = true
	if property, ok := schema.Properties[name]; ok {
		return property, true
	}
	for _, next := range composedSchemas(schema) {
		if property, ok := schemaProperty(next, name, visited); ok {
			return property, true
		}
	}
	return nil, false
}

// itemsSchema unwraps arrays so a []RepoRef compares against the repo item
// schema rather than the array wrapper, following $refs to find it.
func itemsSchema(schema *jsonschema.Schema, visited map[*jsonschema.Schema]bool) *jsonschema.Schema {
	if schema == nil || visited[schema] {
		return nil
	}
	visited[schema] = true
	if schema.Items2020 != nil {
		return schema.Items2020
	}
	if items, ok := schema.Items.(*jsonschema.Schema); ok {
		return items
	}
	for _, next := range composedSchemas(schema) {
		if items := itemsSchema(next, visited); items != nil {
			return items
		}
	}
	return nil
}

// mapValueSchema unwraps maps so a map[string]TokenRef compares against the
// additionalProperties value schema, following $refs to find it.
func mapValueSchema(schema *jsonschema.Schema, visited map[*jsonschema.Schema]bool) *jsonschema.Schema {
	if schema == nil || visited[schema] {
		return nil
	}
	visited[schema] = true
	if value, ok := schema.AdditionalProperties.(*jsonschema.Schema); ok {
		return value
	}
	for _, next := range composedSchemas(schema) {
		if value := mapValueSchema(next, visited); value != nil {
			return value
		}
	}
	return nil
}

// composedSchemas lists the subschemas that can contribute keywords to the
// same object level as schema itself.
func composedSchemas(schema *jsonschema.Schema) []*jsonschema.Schema {
	var next []*jsonschema.Schema
	for _, ref := range []*jsonschema.Schema{schema.Ref, schema.RecursiveRef, schema.DynamicRef, schema.If, schema.Then, schema.Else} {
		if ref != nil {
			next = append(next, ref)
		}
	}
	next = append(next, schema.AllOf...)
	next = append(next, schema.AnyOf...)
	next = append(next, schema.OneOf...)
	return next
}

// TestInstanceSchemaParityGuardCatchesDrift proves the guard actually fires
// (the repo norm for shipped guards — see v_current's
// TestCheckFrozenPatchLogCatchesDrift): a synthetic struct/schema pair where
// one json-tagged field has no schema property must be reported, including
// through the two traversal shapes whose breakage silently neuters a walker
// like this — array items behind a $ref and map values behind
// additionalProperties. Otherwise the completeness test above could rot into
// a no-op alongside the real schema.
func TestInstanceSchemaParityGuardCatchesDrift(t *testing.T) {
	type entry struct {
		Kept    string `json:"kept"`
		Retired string `json:"retired"`
		Skipped string `json:"-"`
	}
	type root struct {
		Nested []entry          `json:"nested"`
		ByName map[string]entry `json:"byName"`
	}

	compile := func(t *testing.T, document string) *jsonschema.Schema {
		t.Helper()
		compiler := jsonschema.NewCompiler()
		compiler.Draft = jsonschema.Draft2020
		if err := compiler.AddResource("synthetic.schema.json", strings.NewReader(document)); err != nil {
			t.Fatalf("register synthetic schema: %v", err)
		}
		compiled, err := compiler.Compile("synthetic.schema.json")
		if err != nil {
			t.Fatalf("compile synthetic schema: %v", err)
		}
		return compiled
	}

	walk := func(t *testing.T, document string) []string {
		t.Helper()
		var missing []string
		collectMissingSchemaFields(reflect.TypeOf(root{}), compile(t, document), "root", &missing, map[reflect.Type]bool{})
		sort.Strings(missing)
		return missing
	}

	t.Run("complete schema yields no findings", func(t *testing.T) {
		missing := walk(t, `{
			"type": "object",
			"properties": {
				"nested": {"type": "array", "items": {"$ref": "#/$defs/entry"}},
				"byName": {"type": "object", "additionalProperties": {"$ref": "#/$defs/entry"}}
			},
			"$defs": {
				"entry": {
					"type": "object",
					"properties": {"kept": {"type": "string"}, "retired": {"type": "string"}}
				}
			}
		}`)
		if len(missing) != 0 {
			t.Fatalf("complete schema was reported as drifted: %v", missing)
		}
	})

	t.Run("a field the schema lost is reported through arrays and maps", func(t *testing.T) {
		// "retired" is gone from the shared entry definition, so it must be
		// reported along BOTH navigation shapes; "-" must never be reported.
		missing := walk(t, `{
			"type": "object",
			"properties": {
				"nested": {"type": "array", "items": {"$ref": "#/$defs/entry"}},
				"byName": {"type": "object", "additionalProperties": {"$ref": "#/$defs/entry"}}
			},
			"$defs": {
				"entry": {
					"type": "object",
					"properties": {"kept": {"type": "string"}}
				}
			}
		}`)
		want := []string{"root.byName.retired", "root.nested.retired"}
		if !reflect.DeepEqual(missing, want) {
			t.Fatalf("missing fields = %v, want %v", missing, want)
		}
	})

	t.Run("a missing top-level property is reported once, not descended into", func(t *testing.T) {
		missing := walk(t, `{
			"type": "object",
			"properties": {
				"nested": {"type": "array", "items": {"$ref": "#/$defs/entry"}}
			},
			"$defs": {
				"entry": {
					"type": "object",
					"properties": {"kept": {"type": "string"}, "retired": {"type": "string"}}
				}
			}
		}`)
		want := []string{"root.byName"}
		if !reflect.DeepEqual(missing, want) {
			t.Fatalf("missing fields = %v, want %v", missing, want)
		}
	})
}
