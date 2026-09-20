package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/gaggletemplate"
	"github.com/goobers/goobers/internal/instance"
)

const templateTestSHA = "1111111111111111111111111111111111111111"

func TestTemplateAuthoringGuideExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "guides", "gaggle-templates.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "```yaml\n")
	tree := gaggletemplate.Tree{}
	for i, name := range []string{"gaggle.yaml", "workflows/inspect.yaml"} {
		if len(blocks) <= i+1 {
			t.Fatal("authoring guide is missing its minimal package examples")
		}
		tree[name] = gaggletemplate.File{Mode: 0644, Data: []byte(strings.SplitN(blocks[i+1], "```", 2)[0])}
		shipped, err := os.ReadFile(filepath.Join("..", "..", "templates", "starter", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(strings.ReplaceAll(string(shipped), "\r\n", "\n")) != strings.TrimSpace(string(tree[name].Data)) {
			t.Fatalf("shipped starter %s differs from the authoring guide", name)
		}
	}
	bound, err := gaggletemplate.Bind(tree, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTemplatePackage("orders", bound); err != nil {
		t.Fatalf("documented package is invalid: %v", err)
	}
}

func TestTemplateSourceValidationIncludesReport(t *testing.T) {
	root, source, _ := templateTestSetup(t)
	path := filepath.Join(source, "gaggles", "example", "gaggle.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(string(raw), "spec:\n", "spec:\n  unsupportedTemplateField: true\n", 1)
	if err := os.WriteFile(path, []byte(invalid), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := writableTemplateConfig(root, &instance.Config{}, source); err == nil ||
		!strings.Contains(err.Error(), "unsupportedTemplateField") {
		t.Fatalf("missing actionable source validation report: %v", err)
	}
}

func templateTestSetup(t *testing.T) (root, source string, tree gaggletemplate.Tree) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "runtime")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	source = filepath.Join(t.TempDir(), "source")
	if err := copyTemplateValidationTree(instance.NewLayout(root).ConfigDir(), source); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "instance.yaml.example"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	tree, err = gaggletemplate.ReadTree(filepath.Join(source, "gaggles", "example"))
	if err != nil {
		t.Fatal(err)
	}
	return root, source, tree
}

func fakeTemplateResolver(t *testing.T, tree *gaggletemplate.Tree, fail *bool) {
	t.Helper()
	old := resolveGaggleTemplate
	t.Cleanup(func() { resolveGaggleTemplate = old })
	resolveGaggleTemplate = func(_ context.Context, _ string, source gaggletemplate.Source, _ string) (gaggletemplate.Tree, string, error) {
		if *fail {
			return nil, "", errors.New("source offline")
		}
		bound, err := gaggletemplate.Bind(*tree, source.Gaggle)
		return bound, templateTestSHA, err
	}
}

func importTestTemplate(t *testing.T, root, source string) {
	t.Helper()
	code, stdout, stderr := runArgs(t, "config", "templates", "import",
		"--repository", source, "--directory", "gaggles/example",
		"--gaggle", "orders", "--source", source, root)
	if code != 0 {
		t.Fatalf("import: %d %s %s", code, stdout, stderr)
	}
}

func deployTestTemplate(t *testing.T, root, source string) {
	t.Helper()
	config, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.WorkflowSource = &instance.WorkflowSource{Kind: instance.WorkflowSourceKindLocalDir, Path: source}
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.MaterializeWorkflowSource(root); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateImportBackpropAndDeployment(t *testing.T) {
	root, source, tree := templateTestSetup(t)
	fail := false
	fakeTemplateResolver(t, &tree, &fail)
	importTestTemplate(t, root, source)
	deployTestTemplate(t, root, source)
	runtimeGaggle := filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", "orders", "gaggle.yaml")
	raw, err := os.ReadFile(runtimeGaggle)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "displayName: Example", "displayName: Customized orders", 1)
	if edited == string(raw) {
		t.Fatalf("fixture missing displayName: %s", raw)
	}
	if err := os.WriteFile(runtimeGaggle, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.MaterializeWorkflowSource(root); err == nil || !strings.Contains(err.Error(), "unpersisted") {
		t.Fatalf("deployment overwrote runtime edits: %v", err)
	}
	sourceGaggle := filepath.Join(source, "gaggles", "orders", "gaggle.yaml")
	sourceRaw, err := os.ReadFile(sourceGaggle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceGaggle, []byte(strings.Replace(string(sourceRaw), "enabled: false", "enabled: true", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "config", "templates", "backprop", "--gaggle", "orders", root)
	if code != 0 {
		t.Fatalf("backprop: %d %s %s", code, stdout, stderr)
	}
	if _, err := instance.MaterializeWorkflowSource(root); err != nil {
		t.Fatalf("deploy captured edits: %v", err)
	}
	got, err := os.ReadFile(runtimeGaggle)
	if err != nil || !strings.Contains(string(got), "Customized orders") || !strings.Contains(string(got), "enabled: true") {
		t.Fatal("backprop edit did not survive deployment")
	}
}

func TestTemplatePackageRejectsMissingSkills(t *testing.T) {
	_, _, tree := templateTestSetup(t)
	tree["goobers/extra/goober.yaml"] = gaggletemplate.File{Mode: 0644, Data: []byte(`apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata: {name: extra}
spec:
  gaggle: example
  role: reviewer
  instructions: instructions.md
  skills: [missing-template-skill]
`)}
	tree["goobers/extra/instructions.md"] = gaggletemplate.File{Mode: 0644, Data: []byte("Review the change.")}
	bound, err := gaggletemplate.Bind(tree, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTemplatePackage("orders", bound); err == nil || !strings.Contains(err.Error(), "missing-template-skill") {
		t.Fatalf("missing package dependency accepted: %v", err)
	}
	tree["skills/missing-template-skill/SKILL.md"] = gaggletemplate.File{Mode: 0644,
		Data: []byte("---\nname: missing-template-skill\ndescription: Review support.\n---\nReview the change.\n")}
	bound, err = gaggletemplate.Bind(tree, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTemplatePackage("orders", bound); err != nil {
		t.Fatalf("vendored package dependency rejected: %v", err)
	}
}

func TestTemplateStandaloneImportRecordsDeploymentAndRefusesRuntimeUpdate(t *testing.T) {
	root, source, tree := templateTestSetup(t)
	fail := false
	fakeTemplateResolver(t, &tree, &fail)
	configDir := instance.NewLayout(root).ConfigDir()
	if runtime.GOOS == "windows" {
		configDir = strings.ToUpper(configDir)
	}
	code, _, stderr := runArgs(t, "config", "templates", "import", "--repository", source,
		"--directory", "gaggles/example", "--gaggle", "orders", "--source", configDir, root)
	if code != 0 {
		t.Fatal(stderr)
	}
	if _, err := gaggletemplate.Deployment(filepath.Join(configDir, "gaggles", "orders")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runArgs(t, "config", "templates", "update", "--gaggle", "orders", "--source", configDir, root)
	if code == 0 || !strings.Contains(stderr, "separate") {
		t.Fatalf("runtime source accepted for update: %d %s", code, stderr)
	}
}

func TestTemplateInvalidUpdateLeavesAcceptedTreeUnchanged(t *testing.T) {
	root, source, tree := templateTestSetup(t)
	fail := false
	fakeTemplateResolver(t, &tree, &fail)
	importTestTemplate(t, root, source)
	target := filepath.Join(source, "gaggles", "orders")
	before, err := gaggletemplate.ReadTree(target)
	if err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.ReadFile(filepath.Join(target, ".template", "lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	file := tree["gaggle.yaml"]
	file.Data = []byte(strings.Replace(string(file.Data), "provider: github", "provider: unsupported", 1))
	tree["gaggle.yaml"] = file
	code, _, stderr := runArgs(t, "config", "templates", "update", "--gaggle", "orders", "--source", source, root)
	if code == 0 || !strings.Contains(stderr, "invalid") {
		t.Fatalf("invalid candidate accepted: %d %s", code, stderr)
	}
	after, err := gaggletemplate.ReadTree(target)
	if err != nil || after.Digest() != before.Digest() {
		t.Fatalf("failed validation changed definitions: %v", err)
	}
	lockAfter, err := os.ReadFile(filepath.Join(target, ".template", "lock.json"))
	if err != nil || string(lockAfter) != string(lockBefore) {
		t.Fatalf("failed validation changed accepted ancestry: %v", err)
	}
}

func TestTemplateUpdatePreservesCustomizationsAndRejectsConflicts(t *testing.T) {
	root, source, tree := templateTestSetup(t)
	fail := false
	fakeTemplateResolver(t, &tree, &fail)
	importTestTemplate(t, root, source)
	gaggle := filepath.Join(source, "gaggles", "orders", "gaggle.yaml")
	raw, err := os.ReadFile(gaggle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gaggle, []byte(strings.Replace(string(raw), "enabled: false", "enabled: true", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	upstream := tree["gaggle.yaml"]
	upstream.Data = []byte(strings.Replace(string(upstream.Data), "displayName: Example", "displayName: Better default", 1))
	tree["gaggle.yaml"] = upstream
	code, _, stderr := runArgs(t, "config", "templates", "update", "--gaggle", "orders", "--source", source, root)
	if code != 0 {
		t.Fatal(stderr)
	}
	got, err := os.ReadFile(gaggle)
	if err != nil || !strings.Contains(string(got), "enabled: true") || !strings.Contains(string(got), "Better default") {
		t.Fatalf("merged gaggle: %s %v", got, err)
	}
	if err := os.WriteFile(gaggle, []byte(strings.Replace(string(got), "Better default", "My title", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(filepath.Dir(gaggle), ".template", "lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	upstream.Data = []byte(strings.Replace(string(upstream.Data), "Better default", "New upstream title", 1))
	tree["gaggle.yaml"] = upstream
	code, _, stderr = runArgs(t, "config", "templates", "update", "--gaggle", "orders", "--source", source, root)
	if code == 0 || !strings.Contains(stderr, "conflict") {
		t.Fatalf("expected conflict: %d %s", code, stderr)
	}
	after, err := os.ReadFile(filepath.Join(filepath.Dir(gaggle), ".template", "lock.json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("conflict changed accepted lock")
	}
}

func TestTemplateChecksDeduplicateAndKeepFailuresVisible(t *testing.T) {
	root, source, tree := templateTestSetup(t)
	fail := false
	fakeTemplateResolver(t, &tree, &fail)
	if notices, err := checkGaggleTemplates(context.Background(), root, time.Now()); err != nil || len(notices) != 0 {
		t.Fatalf("legacy check: %v %v", notices, err)
	}
	importTestTemplate(t, root, source)
	deployTestTemplate(t, root, source)
	upstream := tree["gaggle.yaml"]
	upstream.Data = []byte(strings.Replace(string(upstream.Data), "displayName: Example", "displayName: Updated", 1))
	tree["gaggle.yaml"] = upstream
	now := time.Now().UTC()
	notices, err := checkGaggleTemplates(context.Background(), root, now)
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "update-available") {
		t.Fatalf("check: %v %v", notices, err)
	}
	notices, err = checkGaggleTemplates(context.Background(), root, now.Add(time.Minute))
	if err != nil || len(notices) != 0 {
		t.Fatalf("repeat announced: %v %v", notices, err)
	}
	fail = true
	if _, err := checkGaggleTemplates(context.Background(), root, now.Add(2*time.Minute)); err == nil {
		t.Fatal("offline check succeeded")
	}
	status, err := gaggletemplate.ReadStatus(root, "orders")
	if err != nil || status.State != "unknown" || status.LastSuccess.IsZero() || status.Error != "source offline" {
		t.Fatalf("status: %+v %v", status, err)
	}
}
