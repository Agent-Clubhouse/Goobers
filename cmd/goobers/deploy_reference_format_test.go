package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// fieldFormat is one manifest field whose value carries a declared name
// format, addressed by a path through the decoded document. A "[]" segment
// iterates a list.
type fieldFormat struct {
	kind     string // empty applies to every kind
	path     string
	format   string
	validate func(string) []string
}

var referenceFieldFormats = []fieldFormat{
	{path: "metadata.name", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{path: "metadata.namespace", format: "RFC-1123 label", validate: validation.IsDNS1123Label},
	{kind: "Namespace", path: "metadata.name", format: "RFC-1123 label", validate: validation.IsDNS1123Label},
	{kind: "Service", path: "metadata.name", format: "RFC-1123 label", validate: validation.IsDNS1123Label},
	{kind: "Ingress", path: "spec.ingressClassName", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "Ingress", path: "spec.tls.[].secretName", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "Ingress", path: "spec.rules.[].host", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "Ingress", path: "spec.rules.[].http.paths.[].backend.service.name", format: "RFC-1123 label", validate: validation.IsDNS1123Label},
	{kind: "PersistentVolumeClaim", path: "spec.storageClassName", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "PersistentVolumeClaim", path: "spec.volumeName", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "Deployment", path: "spec.template.spec.serviceAccountName", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "Deployment", path: "spec.template.spec.containers.[].name", format: "RFC-1123 label", validate: validation.IsDNS1123Label},
	{kind: "Deployment", path: "spec.template.spec.initContainers.[].name", format: "RFC-1123 label", validate: validation.IsDNS1123Label},
	{kind: "Deployment", path: "spec.template.spec.volumes.[].name", format: "RFC-1123 label", validate: validation.IsDNS1123Label},
	{kind: "Deployment", path: "spec.template.spec.volumes.[].persistentVolumeClaim.claimName", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "ServiceAccount", path: "metadata.name", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "RoleBinding", path: "subjects.[].name", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "RoleBinding", path: "roleRef.name", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "ClusterRoleBinding", path: "subjects.[].name", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
	{kind: "ClusterRoleBinding", path: "roleRef.name", format: "RFC-1123 subdomain", validate: validation.IsDNS1123Subdomain},
}

// TestDeployReferenceNamedFieldsAreFormatValid is the #3310 value-format lint,
// wired into `make deploy-validate` with the other TestDeployReference checks.
// kustomize renders and kubeconform checks *schema* — neither validates a
// value against its own field's declared name format, so a placeholder like
// `ingressClassName: CHANGE-ME` sails through both and is then rejected
// outright by the API server, taking the whole `kubectl apply -k` of the
// reference down with it. The CHANGE-ME convention's contract is "conspicuous
// until replaced"; a placeholder that cannot even be applied unreplaced breaks
// it, so placeholders in name-format fields are lowercase `change-me`.
func TestDeployReferenceNamedFieldsAreFormatValid(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "reference")
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Helm values, not API objects — the chart, not this tree, owns
			// their shape.
			if entry.Name() == "temporal" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext == ".yaml" || ext == ".yml" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("found no reference manifests under %s", root)
	}

	checked := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, doc := range splitYAMLDocs(raw) {
			var object map[string]any
			if err := yaml.Unmarshal(doc, &object); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			kind, _ := object["kind"].(string)
			// Kustomizations are build inputs, not API objects.
			if kind == "" || kind == "Kustomization" || kind == "Component" {
				continue
			}
			problems, inspected := lintManifestFieldFormats(kind, object)
			checked += inspected
			for _, problem := range problems {
				t.Errorf("%s: %s — the API server rejects this value, so the manifest cannot be applied as shipped (#3310)", path, problem)
			}
		}
	}
	if checked == 0 {
		t.Fatal("lint inspected no fields — the path table no longer matches the reference manifests")
	}
}

// lintManifestFieldFormats reports every declared-format violation in one
// decoded manifest, along with how many fields it actually inspected.
func lintManifestFieldFormats(kind string, object map[string]any) (problems []string, inspected int) {
	for _, field := range referenceFieldFormats {
		if field.kind != "" && field.kind != kind {
			continue
		}
		for _, found := range lookupManifestStrings(object, strings.Split(field.path, ".")) {
			inspected++
			for _, problem := range field.validate(found.value) {
				problems = append(problems, fmt.Sprintf("%s %s = %q is not a valid %s: %s", kind, found.path, found.value, field.format, problem))
			}
		}
	}
	return problems, inspected
}

// The uppercase CHANGE-ME placeholder #3310 reported is exactly what this lint
// exists to catch: kustomize and kubeconform both accept it.
func TestLintManifestFieldFormatsRejectsUppercasePlaceholders(t *testing.T) {
	ingress := map[string]any{
		"kind": "Ingress",
		"metadata": map[string]any{
			"name":      "goobers-api",
			"namespace": "goobers-system",
		},
		"spec": map[string]any{
			"ingressClassName": "CHANGE-ME",
			"tls": []any{
				map[string]any{"secretName": "goobers-api-tls"},
			},
		},
	}

	problems, inspected := lintManifestFieldFormats("Ingress", ingress)
	if inspected == 0 {
		t.Fatal("inspected no fields")
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly the ingressClassName violation", problems)
	}
	if !strings.Contains(problems[0], "spec.ingressClassName") || !strings.Contains(problems[0], "CHANGE-ME") {
		t.Errorf("problem = %q, want it to name the offending field and value", problems[0])
	}

	ingress["spec"].(map[string]any)["ingressClassName"] = "change-me"
	if problems, _ := lintManifestFieldFormats("Ingress", ingress); len(problems) != 0 {
		t.Errorf("lowercase placeholder reported %v, want no problems", problems)
	}
}

func TestLookupManifestStringsWalksListsAndSkipsMissingFields(t *testing.T) {
	object := map[string]any{
		"spec": map[string]any{
			"ingressClassName": "change-me",
			"rules": []any{
				map[string]any{"host": "a.example.com"},
				map[string]any{"host": "b.example.com"},
				map[string]any{"other": "no host here"},
			},
			"replicas": 0,
		},
	}

	got := lookupManifestStrings(object, strings.Split("spec.rules.[].host", "."))
	if len(got) != 2 || got[0].value != "a.example.com" || got[1].value != "b.example.com" {
		t.Fatalf("hosts = %+v, want the two hosts", got)
	}
	if got[1].path != "spec.rules[1].host" {
		t.Errorf("path = %q, want %q", got[1].path, "spec.rules[1].host")
	}
	if got := lookupManifestStrings(object, strings.Split("spec.storageClassName", ".")); len(got) != 0 {
		t.Errorf("missing field yielded %+v, want nothing", got)
	}
	if got := lookupManifestStrings(object, strings.Split("spec.replicas", ".")); len(got) != 0 {
		t.Errorf("non-string field yielded %+v, want nothing", got)
	}
	if got := lookupManifestStrings(object, strings.Split("spec.ingressClassName.nested", ".")); len(got) != 0 {
		t.Errorf("over-long path yielded %+v, want nothing", got)
	}
}

type manifestString struct {
	path  string
	value string
}

// lookupManifestStrings resolves a dotted path through a decoded manifest,
// where a "[]" segment fans out over a list. Fields the document does not
// carry, and values that are not strings, simply yield nothing: a manifest
// that omits an optional field has no format to violate.
func lookupManifestStrings(node any, path []string) []manifestString {
	return appendManifestStrings(nil, node, "", path)
}

func appendManifestStrings(out []manifestString, node any, rendered string, path []string) []manifestString {
	if len(path) == 0 {
		if value, ok := node.(string); ok {
			out = append(out, manifestString{path: rendered, value: value})
		}
		return out
	}
	segment, rest := path[0], path[1:]
	if segment == "[]" {
		items, ok := node.([]any)
		if !ok {
			return out
		}
		for i, item := range items {
			out = appendManifestStrings(out, item, rendered+"["+strconv.Itoa(i)+"]", rest)
		}
		return out
	}
	object, ok := node.(map[string]any)
	if !ok {
		return out
	}
	child, ok := object[segment]
	if !ok {
		return out
	}
	if rendered != "" {
		rendered += "."
	}
	return appendManifestStrings(out, child, rendered+segment, rest)
}
