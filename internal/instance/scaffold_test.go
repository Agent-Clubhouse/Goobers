package instance

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
)

func TestInitFresh(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")

	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("expected nothing skipped on a fresh init, got %v", res.Skipped)
	}
	wantCreated := []string{ConfigFileName, ConfigDirName, GagglesDirName, SchedulerDirName, TelemetryDBName}
	gotCreated := append([]string(nil), res.Created...)
	sort.Strings(gotCreated)
	sort.Strings(wantCreated)
	if len(gotCreated) != len(wantCreated) {
		t.Fatalf("created = %v, want %v", res.Created, wantCreated)
	}
	for i := range wantCreated {
		if gotCreated[i] != wantCreated[i] {
			t.Fatalf("created = %v, want %v", res.Created, wantCreated)
		}
	}

	l := NewLayout(root)
	for _, dir := range []string{l.GagglesDir(), l.SchedulerDir(), l.ConfigDir()} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("expected %s to be a directory: %v", dir, err)
		}
	}
	if info, err := os.Stat(l.TelemetryDB()); err != nil || info.IsDir() {
		t.Fatalf("expected %s to be a file: %v", l.TelemetryDB(), err)
	}

	cfg, err := LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatalf("scaffolded instance.yaml did not load: %v", err)
	}
	if len(cfg.Repos) == 0 {
		t.Fatalf("expected scaffolded instance.yaml to include a starter repo entry")
	}

	if _, err := os.Stat(filepath.Join(l.ConfigDir(), "manifest.yaml")); err != nil {
		t.Fatalf("expected seeded config/manifest.yaml: %v", err)
	}
	assertPreviewFeaturesDefaultOff(t, l.ConfigDir())

	set, report, err := LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("seeded config/ did not validate: %v (report: %+v)", err, report)
	}
	if len(set.Gaggles) != 1 || len(set.Goobers) != 1 || len(set.Workflows) != 1 {
		t.Fatalf("unexpected seeded config shape: %+v", set)
	}
}

func TestInitDemoFresh(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	res, err := InitDemo(root)
	if err != nil {
		t.Fatalf("InitDemo: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("fresh demo init skipped entries: %v", res.Skipped)
	}

	l := NewLayout(root)
	assertPreviewFeaturesDefaultOff(t, l.ConfigDir())
	cfg, err := LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 0 || len(cfg.Credentials) != 0 {
		t.Fatalf("demo instance unexpectedly requires connections: %+v", cfg)
	}
	set, report, err := LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Gaggles) != 1 || len(set.Goobers) != 0 || len(set.Workflows) != 1 {
		t.Fatalf("unexpected demo config shape: %+v", set)
	}
	workflow := set.Workflows[0]
	if workflow.Name != "demo" || len(workflow.Spec.Tasks) != 4 || len(workflow.Spec.Gates) != 1 {
		t.Fatalf("unexpected demo workflow: %+v", workflow)
	}
	wantTasks := []string{"curate", "implement", "review", "merge-preview"}
	for i, task := range workflow.Spec.Tasks {
		if task.Name != wantTasks[i] {
			t.Fatalf("demo task %d = %q, want %q", i, task.Name, wantTasks[i])
		}
		if task.Type != apiv1.TaskDeterministic || task.Run == nil ||
			task.Run.Workspace != apiv1.WorkspaceScratch || task.Run.Network != apiv1.NetworkNone {
			t.Fatalf("demo task is not an offline scratch deterministic stage: %+v", task)
		}
		if len(task.Run.Command) != 3 || task.Run.Command[0] != "goobers" ||
			task.Run.Command[1] != "__demo-provider" || task.Run.Command[2] != task.Name {
			t.Fatalf("demo task does not use its mock-provider phase: %+v", task.Run.Command)
		}
	}
	if gate := workflow.Spec.Gates[0]; gate.Name != "review-verdict" || gate.Branches["pass"] != "merge-preview" {
		t.Fatalf("unexpected demo review gate: %+v", gate)
	}
}

func TestInitQuickstartFresh(t *testing.T) {
	root := filepath.Join(t.TempDir(), "quickstart")
	res, err := InitQuickstart(root)
	if err != nil {
		t.Fatalf("InitQuickstart: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("fresh quickstart init skipped entries: %v", res.Skipped)
	}

	configDir := NewLayout(root).ConfigDir()
	assertPreviewFeaturesDefaultOff(t, configDir)
	set, report, err := LoadConfigDir(configDir)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}

	if len(set.Gaggles) != 1 || len(set.Goobers) != 2 || len(set.Workflows) != 1 {
		t.Fatalf("unexpected quickstart config shape: %+v", set)
	}
	workflow := set.Workflows[0]
	if workflow.Name != QuickstartTemplate || len(workflow.Spec.Tasks) != 6 || len(workflow.Spec.Gates) != 0 {
		t.Fatalf("unexpected quickstart workflow: %+v", workflow)
	}
	if got, want := set.Gaggles[0].Spec.CICommand, []string{"npm", "run", "ci"}; !slices.Equal(got, want) {
		t.Fatalf("quickstart ciCommand = %v, want %v", got, want)
	}
	if len(workflow.Spec.Triggers) != 1 || workflow.Spec.Triggers[0].Type != apiv1.TriggerManual {
		t.Fatalf("quickstart trigger = %+v, want manual-only", workflow.Spec.Triggers)
	}
	wantTasks := []string{"query-backlog", "implement", "review", "local-ci", "push-branch", "open-pr"}
	for i, task := range workflow.Spec.Tasks {
		if task.Name != wantTasks[i] {
			t.Fatalf("quickstart task %d = %q, want %q", i, task.Name, wantTasks[i])
		}
	}

	// Cold-start SKILL002 probe (see
	// TestLoadConfigDirStarterHasNoMissingSkillPackageWarnings): the
	// quickstart template's implementer/reviewer goobers declare
	// implement/run-tests/review skills, which must resolve to scaffolded
	// packages on a virgin init.
	for _, warning := range report.Warnings() {
		if warning.Code == validate.WarningMissingSkillPackage {
			t.Fatalf("virgin quickstart scaffold emitted a missing-skill-package warning: %+v", warning)
		}
	}
}

func assertPreviewFeaturesDefaultOff(t *testing.T, configDir string) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(configDir, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "goobers.dev/allow-preview-features") {
		t.Fatalf("generated manifest enables preview features by default:\n%s", manifest)
	}
}

func TestInitIdempotent(t *testing.T) {
	root := t.TempDir()

	if _, err := Init(root); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	res, err := Init(root)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if len(res.Created) != 0 {
		t.Fatalf("expected nothing created on a repeated init, got %v", res.Created)
	}
	if len(res.Skipped) != 5 {
		t.Fatalf("expected every piece skipped on a repeated init, got %v", res.Skipped)
	}
}

func TestInitPreservesExistingConfigDir(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	if err := os.MkdirAll(l.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(l.ConfigDir(), "custom.yaml")
	if err := os.WriteFile(custom, []byte("kind: Manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	found := false
	for _, s := range res.Skipped {
		if s == ConfigDirName {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected config dir to be skipped (pre-existing content), got created=%v skipped=%v", res.Created, res.Skipped)
	}
	if _, err := os.Stat(filepath.Join(l.ConfigDir(), "manifest.yaml")); err == nil {
		t.Fatal("starter config should not have been seeded over an existing config/ dir")
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("pre-existing config file was not preserved: %v", err)
	}
}

func TestInitPreservesExistingInstanceYAML(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: mine
    name: repo
    token:
      env: MY_TOKEN
`
	if err := os.WriteFile(l.ConfigFile(), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	found := false
	for _, s := range res.Skipped {
		if s == ConfigFileName {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected instance.yaml to be skipped, got created=%v skipped=%v", res.Created, res.Skipped)
	}
	cfg, err := LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Owner != "mine" {
		t.Fatalf("pre-existing instance.yaml was overwritten: %+v", cfg.Repos)
	}
}

func TestSeedQuickstartConfigSourceRejectsConflictingManagedFile(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(manifest, []byte("user-owned\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := SeedQuickstartConfigSource(root)
	if err == nil || !strings.Contains(err.Error(), "manifest.yaml differs from the quickstart template") {
		t.Fatalf("SeedQuickstartConfigSource error = %v, want managed-file conflict", err)
	}
	data, readErr := os.ReadFile(manifest)
	if readErr != nil || string(data) != "user-owned\n" {
		t.Fatalf("conflicting manifest changed: data=%q err=%v", data, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, GuidedSourceInstanceFile)); !os.IsNotExist(statErr) {
		t.Fatalf("preflight conflict created another managed file: %v", statErr)
	}
}

func TestSeedQuickstartConfigSourceRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "gaggles")); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	_, err := SeedQuickstartConfigSource(root)
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("SeedQuickstartConfigSource error = %v, want symlink rejection", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("seed wrote outside the config source: %v", entries)
	}
	if _, statErr := os.Stat(filepath.Join(root, GuidedSourceInstanceFile)); !os.IsNotExist(statErr) {
		t.Fatalf("symlink preflight created another managed file: %v", statErr)
	}
}

func TestSeedQuickstartConfigSourceRejectsInvalidCompletedTree(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "invalid.yaml"), []byte("not: [valid\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := SeedQuickstartConfigSource(root)
	if err == nil || !strings.Contains(err.Error(), "validate seeded config source") {
		t.Fatalf("SeedQuickstartConfigSource error = %v, want validation failure", err)
	}
}

func TestInitRefusesForeignConfigDirWithoutWriting(t *testing.T) {
	// A source checkout of this repository is the canonical trap (#2513): its
	// tracked config/ holds CRD manifests, not instance config.
	root := t.TempDir()
	crd := filepath.Join(root, ConfigDirName, "crd", "bases", "widgets.yaml")
	if err := os.MkdirAll(filepath.Dir(crd), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n"
	if err := os.WriteFile(crd, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Init(root)
	if err == nil {
		t.Fatalf("Init adopted a foreign %s directory", ConfigDirName)
	}
	var conflict *TargetConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Init error = %T %v, want *TargetConflictError", err, err)
	}
	abs, absErr := filepath.Abs(root)
	if absErr != nil {
		t.Fatal(absErr)
	}
	for _, want := range []string{abs, "kind: Manifest", "goobers init ./my-instance"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Init error = %q, missing %q", err, want)
		}
	}
	// Refusal must leave the target untouched.
	for _, name := range []string{ConfigFileName, GagglesDirName, SchedulerDirName, TelemetryDBName} {
		if _, statErr := os.Stat(filepath.Join(root, name)); !os.IsNotExist(statErr) {
			t.Fatalf("refused Init wrote %s, stat error = %v", name, statErr)
		}
	}
}

func TestInitAdoptsConfigFirstLayoutWithManifest(t *testing.T) {
	// Authoring config/ before running init is a supported layout: a Manifest
	// document marks the directory as genuine Goobers config.
	root := t.TempDir()
	manifest := filepath.Join(root, ConfigDirName, "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "apiVersion: goobers.dev/v1alpha1\nkind: Manifest\nmetadata:\n  name: preauthored\n"
	if err := os.WriteFile(manifest, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	found := false
	for _, skipped := range res.Skipped {
		if skipped == ConfigDirName {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s to be skipped (adopted), got skipped=%v created=%v", ConfigDirName, res.Skipped, res.Created)
	}
	data, err := os.ReadFile(manifest)
	if err != nil || string(data) != doc {
		t.Fatalf("pre-authored manifest changed: data=%q err=%v", data, err)
	}
}
