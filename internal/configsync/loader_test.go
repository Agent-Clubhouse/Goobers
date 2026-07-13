package configsync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/goobers/goobers/api/v1alpha1"
)

// repo paths reused as fixtures.
const (
	validConfigRepo = "../../config-examples"
	badConfigRepo   = "../../api/validate/testdata/config-bad"
)

func objectsByKind(objs []client.Object) map[string][]client.Object {
	m := map[string][]client.Object{}
	for _, o := range objs {
		k := o.GetObjectKind().GroupVersionKind().Kind
		m[k] = append(m[k], o)
	}
	return m
}

func TestLoad_ValidExampleRepo(t *testing.T) {
	l, err := NewLoader("")
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	set, report, err := l.Load(validConfigRepo)
	if err != nil {
		t.Fatalf("Load: %v (report: %+v)", err, report.Issues)
	}
	if report.HasErrors() {
		t.Fatalf("example repo should validate clean, got: %+v", report.Issues)
	}
	if set.Namespace != DefaultNamespace {
		t.Errorf("namespace = %q, want %q", set.Namespace, DefaultNamespace)
	}

	// config-examples ships one Manifest/Gaggle, four Goobers (coder,
	// curator, implementer, reviewer), and three Workflows
	// (default-implement, backlog-curation — #25, implementation — #27).
	wantByKind := map[string]int{"Manifest": 1, "Gaggle": 1, "Goober": 4, "Workflow": 3}
	by := objectsByKind(set.Objects)
	for kind, want := range wantByKind {
		if len(by[kind]) != want {
			t.Errorf("kind %s: got %d objects, want %d", kind, len(by[kind]), want)
		}
	}

	// Every object is stamped: namespace, apiVersion/kind, managed-by, instance.
	for _, o := range set.Objects {
		if o.GetNamespace() != DefaultNamespace {
			t.Errorf("%s/%s namespace = %q", o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), o.GetNamespace())
		}
		if o.GetObjectKind().GroupVersionKind().GroupVersion().String() != v1alpha1.GroupVersion.String() {
			t.Errorf("%s missing apiVersion", o.GetName())
		}
		if o.GetLabels()[ManagedByLabel] != ManagedByValue {
			t.Errorf("%s missing managed-by label", o.GetName())
		}
		if o.GetLabels()[InstanceLabel] != "acme" {
			t.Errorf("%s instance label = %q, want acme", o.GetName(), o.GetLabels()[InstanceLabel])
		}
	}

	// Manifest is first (dependency ordering).
	if set.Objects[0].GetObjectKind().GroupVersionKind().Kind != "Manifest" {
		t.Errorf("first object should be Manifest, got %s", set.Objects[0].GetObjectKind().GroupVersionKind().Kind)
	}
}

func TestLoad_InvalidConfigRejected(t *testing.T) {
	l, err := NewLoader("")
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	set, report, err := l.Load(badConfigRepo)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got err=%v", err)
	}
	if set != nil {
		t.Error("RenderSet should be nil on invalid config")
	}
	if !report.HasErrors() {
		t.Error("report should carry field-level errors")
	}
}

// TestLoad_ManifestGatesGaggles verifies that a gaggle present on disk but not
// listed in the Manifest is excluded from the desired set (GitOps removal: drop
// it from the manifest and it disappears from the render).
func TestLoad_ManifestGatesGaggles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "manifest.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: inst
spec:
  instance: {name: acme, environment: dev}
  gaggles: [included]
`)
	writeFile(t, filepath.Join(dir, "gaggles", "included"), "gaggle.yaml", gaggleYAML("included"))
	writeGoober(t, filepath.Join(dir, "gaggles", "included", "goobers", "c"), "c", "included")
	// This gaggle is on disk but NOT in the manifest -> must be excluded.
	writeFile(t, filepath.Join(dir, "gaggles", "excluded"), "gaggle.yaml", gaggleYAML("excluded"))
	writeGoober(t, filepath.Join(dir, "gaggles", "excluded", "goobers", "d"), "d", "excluded")

	l, _ := NewLoader("")
	set, report, err := l.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v (%+v)", err, report.Issues)
	}
	for _, o := range set.Objects {
		name := o.GetName()
		if name == "excluded" || name == "d" {
			t.Errorf("object %q should be excluded (not in manifest)", name)
		}
	}
	by := objectsByKind(set.Objects)
	if len(by["Gaggle"]) != 1 || by["Gaggle"][0].GetName() != "included" {
		t.Errorf("expected only the included gaggle, got %v", by["Gaggle"])
	}
	if len(by["Goober"]) != 1 || by["Goober"][0].GetName() != "c" {
		t.Errorf("expected only goober c, got %v", by["Goober"])
	}
}

// TestLoad_IgnoresOutputDir verifies a nested render-output dir is excluded from
// the load: even though it contains duplicate (already-rendered) CRs that would
// otherwise trip the validator, Load(dir, out) succeeds and yields the source set.
func TestLoad_IgnoresOutputDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "manifest.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata: {name: inst}
spec:
  instance: {name: acme, environment: dev}
  gaggles: [web]
`)
	writeFile(t, filepath.Join(dir, "gaggles", "web"), "gaggle.yaml", gaggleYAML("web"))

	// Simulate a prior render living under the config root: a duplicate Gaggle
	// that, if ingested, would cause a duplicate-name validation error.
	out := filepath.Join(dir, "rendered")
	writeFile(t, out, "gaggle-web.yaml", gaggleYAML("web"))
	writeFile(t, out, "manifest-inst.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata: {name: inst}
spec: {instance: {name: acme, environment: dev}, gaggles: [web]}
`)

	l, _ := NewLoader("")
	set, report, err := l.Load(dir, out)
	if err != nil {
		t.Fatalf("Load with ignored output should succeed, got %v (%+v)", err, report.Issues)
	}
	by := objectsByKind(set.Objects)
	if len(by["Gaggle"]) != 1 {
		t.Errorf("expected exactly 1 Gaggle (output dir ignored), got %d", len(by["Gaggle"]))
	}
}

func TestLoad_NoManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gaggles", "g"), "gaggle.yaml", gaggleYAML("g"))
	l, _ := NewLoader("")
	_, _, err := l.Load(dir)
	if err == nil {
		t.Fatal("expected error when no Manifest present")
	}
}

// --- helpers ---

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gaggleYAML(name string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: ` + name + `
spec:
  project: {provider: github, owner: acme, name: web}
  backlog: {provider: github, project: acme/web}
  isolation: {namespace: gaggle-` + name + `}
`
}

// writeGoober writes a goober.yaml plus the instructions.md file the validator
// requires to exist on disk relative to the goober definition.
func writeGoober(t *testing.T, dir, name, gaggle string) {
	t.Helper()
	writeFile(t, dir, "goober.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: `+name+`
spec:
  gaggle: `+gaggle+`
  role: `+name+`
  instructions: instructions.md
`)
	writeFile(t, dir, "instructions.md", "# "+name+"\n")
}
