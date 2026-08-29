package v1alpha1

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The CRD manifests are a PUBLISHED contract. A field that exists in the Go
// types but is absent from the manifests is a contract that silently lies about
// the DSL: at tier 3 the operator would reject a definition that validates
// perfectly at tiers 1-2, breaking CFG-022's "identical schemas at every tier".
//
// This drifted for real. `inputsFrom`, `docsRoots` and `requiredCapabilities`
// were all in the Go types and the JSON schema but missing from the manifests,
// and two autonomous attempts at #562 were parked on exactly that gap before
// anyone noticed the manifests were stale rather than the code being wrong.
//
// The guard is deliberately a REFLECTIVE test rather than a
// regenerate-then-diff step like portal-contract-diff: running controller-gen
// in CI would add a network dependency (it is fetched via `go run
// sigs.k8s.io/controller-tools/...@version`) to every build. Comparing the Go
// types against the committed YAML is hermetic, fast, and catches the class of
// drift that actually bit us — a field added to a type and never regenerated.
//
// It intentionally checks field PRESENCE, not full schema equality: validation
// markers, descriptions and CEL rules are covered by the targeted assertions in
// crd_validation_test.go, and asserting the whole schema here would make every
// doc-comment edit a manifest change.
func TestCRDManifestsExposeEveryTypeField(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		root any
	}{
		{"Workflow", "../../config/crd/bases/goobers.dev_workflows.yaml", WorkflowSpec{}},
		{"Goober", "../../config/crd/bases/goobers.dev_goobers.yaml", GooberSpec{}},
		{"Gaggle", "../../config/crd/bases/goobers.dev_gaggles.yaml", GaggleSpec{}},
		{"Manifest", "../../config/crd/bases/goobers.dev_manifests.yaml", ManifestSpec{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := loadCRDSpecSchema(t, tc.path)
			var missing []string
			collectMissingFields(reflect.TypeOf(tc.root), schema, "spec", &missing, map[reflect.Type]bool{})
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s CRD manifest is missing %d field(s) present in the Go types:\n  %s\n\nRun `make manifests` and commit the result.",
					tc.name, len(missing), strings.Join(missing, "\n  "))
			}
		})
	}
}

func loadCRDSpecSchema(t *testing.T, path string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CRD %s: %v", path, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode CRD %s: %v", path, err)
	}
	if len(crd.Spec.Versions) == 0 || crd.Spec.Versions[0].Schema == nil {
		t.Fatalf("CRD %s has no versioned schema", path)
	}
	spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		t.Fatalf("CRD %s has no spec schema", path)
	}
	return &spec
}

// collectMissingFields walks a Go struct against an OpenAPI schema, recording
// every json-tagged field with no corresponding property. Recursion is guarded
// against self-referential types.
func collectMissingFields(goType reflect.Type, schema *apiextensionsv1.JSONSchemaProps, path string, missing *[]string, seen map[reflect.Type]bool) {
	// Unwrap pointers AND collections: a []Task must compare against the task
	// item schema, not be skipped for not being a struct. Getting this wrong
	// silently reduces the whole guard to top-level fields only — which is
	// exactly what it did on the first attempt, hiding the very
	// spec.tasks.inputsFrom gap this test was written for.
unwrap:
	for goType != nil {
		switch goType.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			goType = goType.Elem()
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
		name := jsonFieldName(field)
		if name == "" {
			continue
		}
		property, ok := schema.Properties[name]
		if !ok {
			*missing = append(*missing, path+"."+name)
			continue
		}
		collectMissingFields(field.Type, elementSchema(&property), path+"."+name, missing, seen)
	}
}

// elementSchema unwraps arrays so a []Task compares against the task item
// schema rather than the array wrapper.
func elementSchema(schema *apiextensionsv1.JSONSchemaProps) *apiextensionsv1.JSONSchemaProps {
	if schema == nil {
		return nil
	}
	if schema.Type == "array" && schema.Items != nil && schema.Items.Schema != nil {
		return schema.Items.Schema
	}
	return schema
}

func jsonFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		// An embedded/inline field carries no name of its own.
		return ""
	}
	return name
}
