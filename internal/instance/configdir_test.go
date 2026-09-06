package instance

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
)

const (
	validConfigDir = "../../config-examples"
	badConfigDir   = "../../api/validate/testdata/config-bad"
)

func TestLoadConfigDirValid(t *testing.T) {
	set, report, err := LoadConfigDir(validConfigDir)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if set.Manifest == nil {
		t.Fatal("expected a Manifest")
	}
	gotGaggles := map[string]bool{}
	for _, g := range set.Gaggles {
		gotGaggles[g.Name] = true
	}
	if len(set.Gaggles) != 5 || !gotGaggles["acme-web"] || !gotGaggles["acme-web-claude"] || !gotGaggles["dotnet-service"] || !gotGaggles["java-service"] || !gotGaggles["python-service"] {
		t.Fatalf("unexpected gaggles: %+v", set.Gaggles)
	}
	// config-examples ships eighteen goobers (acme-web: coder, curator, docs,
	// implementer, nominator, reviewer; acme-web-claude: the same six roles
	// claude-prefixed to stay globally unique, #2777's additive parallel
	// gaggle; dotnet-service: dotnet-implementer, dotnet-reviewer;
	// java-service: java-implementer, java-reviewer; python-service:
	// python-implementer, python-reviewer) and twenty-one workflows
	// (acme-web's nine, acme-web-claude's same nine names again since
	// workflow names are scoped by gaggle not global, and one implementation
	// reference per polyglot service); check membership, not order.
	gotGoobers := map[string]bool{}
	for _, g := range set.Goobers {
		gotGoobers[g.Name] = true
	}
	wantGoobers := []string{
		"coder", "curator", "docs", "implementer", "nominator", "reviewer",
		"claude-coder", "claude-curator", "claude-docs", "claude-implementer", "claude-nominator", "claude-reviewer",
		"dotnet-implementer", "dotnet-reviewer", "java-implementer", "java-reviewer", "python-implementer", "python-reviewer",
	}
	if len(set.Goobers) != len(wantGoobers) {
		t.Fatalf("unexpected goobers: %+v", set.Goobers)
	}
	for _, name := range wantGoobers {
		if !gotGoobers[name] {
			t.Fatalf("missing goober %q; got: %+v", name, set.Goobers)
		}
	}
	gotWorkflows := map[string]bool{}
	var inlineWorkflow *apiv1.Workflow
	for _, w := range set.Workflows {
		gotWorkflows[w.Name] = true
		if w.Name == "inline-policy-check" && w.Spec.Gaggle == "acme-web" {
			workflow := w
			inlineWorkflow = &workflow
		}
	}
	wantWorkflows := []string{"default-implement", "backlog-assignment", "backlog-curation", "docs-updater", "implementation", "inline-policy-check", "work-nomination", "merge-review", "todo-check", "dotnet-implementation", "java-implementation", "python-implementation"}
	// acme-web-claude reuses acme-web's nine workflow names verbatim (workflow
	// identity is gaggle-scoped, unlike goober names), so the total count is
	// twelve unique names but twenty-one total definitions.
	const wantTotalWorkflows = 21
	if len(set.Workflows) != wantTotalWorkflows {
		t.Fatalf("unexpected workflows: %+v", set.Workflows)
	}
	for _, name := range wantWorkflows {
		if !gotWorkflows[name] {
			t.Fatalf("missing workflow %q; got: %+v", name, set.Workflows)
		}
	}
	if inlineWorkflow == nil || inlineWorkflow.DSLVersion != "2.0" {
		t.Fatalf("inline workflow = %+v, want DSL 2.0 example", inlineWorkflow)
	}
	if len(inlineWorkflow.Spec.Tasks) == 0 || inlineWorkflow.Spec.Tasks[0].Run == nil ||
		inlineWorkflow.Spec.Tasks[0].Run.Script == "" {
		t.Fatalf("inline workflow does not exercise run.script: %+v", inlineWorkflow.Spec.Tasks)
	}
}

// TestLoadConfigDirStarterHasNoMissingSkillPackageWarnings reproduces the
// cold-start SKILL002 probe ("goobers init scaffolds goobers whose spec.skills reference
// packages init does not create — its own post-init validation prints
// SKILL002 warnings on a virgin scaffold", hit by all five cold-start
// flavors). A virgin copy of the starter template must now validate with
// zero SKILL002 findings because the referenced "implement"/"run-tests"
// skill packages are scaffolded alongside the goober that declares them.
func TestLoadConfigDirStarterHasNoMissingSkillPackageWarnings(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.CopyFS(configDir, os.DirFS("starter")); err != nil {
		t.Fatal(err)
	}

	_, report, err := LoadConfigDir(configDir)
	if err != nil {
		t.Fatalf("LoadConfigDir on a virgin starter scaffold: %v (report: %+v)", err, report)
	}
	for _, warning := range report.Warnings() {
		if warning.Code == validate.WarningMissingSkillPackage {
			t.Fatalf("virgin starter scaffold emitted a missing-skill-package warning: %+v", warning)
		}
	}

	// Negative control: the check must still fire when a skill package is
	// genuinely missing, proving the clean result above comes from the
	// scaffolded packages and not from the check having stopped running.
	if err := os.RemoveAll(filepath.Join(configDir, "gaggles", "example", "skills")); err != nil {
		t.Fatal(err)
	}
	_, report, err = LoadConfigDir(configDir)
	if err != nil {
		t.Fatalf("LoadConfigDir with skill packages removed: %v (report: %+v)", err, report)
	}
	var missing []validate.CodedWarning
	for _, warning := range report.Warnings() {
		if warning.Code == validate.WarningMissingSkillPackage {
			missing = append(missing, warning)
		}
	}
	if len(missing) != 2 {
		t.Fatalf("missing skill warnings with packages removed = %+v, want implement and run-tests", missing)
	}
}

func TestLoadConfigDirInvalid(t *testing.T) {
	set, report, err := LoadConfigDir(badConfigDir)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if set != nil {
		t.Fatalf("expected a nil ConfigSet on invalid config, got %+v", set)
	}
	if report == nil || !report.HasErrors() {
		t.Fatalf("expected a report with errors, got %+v", report)
	}
}

// TestLoadConfigDirRejectsDuplicateKey guards #3643 at the config-as-code
// loader boundary: sigs.k8s.io/yaml's non-strict YAML-to-JSON conversion
// silently kept the LAST of a duplicate mapping key, so a later duplicate
// "tools:" here could silently replace the first's tool list while schema
// validation only ever saw the merged, already-deduplicated document.
func TestLoadConfigDirRejectsDuplicateKey(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(validConfigDir)); err != nil {
		t.Fatal(err)
	}
	goober := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "goober.yaml")
	data, err := os.ReadFile(goober)
	if err != nil {
		t.Fatal(err)
	}
	const original = "  tools:\n    - github\n    - shell\n"
	if !strings.Contains(string(data), original) {
		t.Fatalf("fixture %s does not contain expected tools: block", goober)
	}
	duplicated := strings.Replace(string(data), original, original+"  tools:\n    - \"*\"\n", 1)
	if err := os.WriteFile(goober, []byte(duplicated), 0o644); err != nil {
		t.Fatal(err)
	}

	set, report, err := LoadConfigDir(root)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if set != nil {
		t.Fatalf("expected a nil ConfigSet on invalid config, got %+v", set)
	}
	if report == nil || !report.HasErrors() {
		t.Fatalf("expected a report with errors, got %+v", report)
	}
	var found bool
	for _, issue := range report.Issues {
		if strings.Contains(issue.Message, `duplicate key "tools"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("report did not name the duplicate key: %+v", report.Issues)
	}
}

func TestLoadConfigDirForComparisonReturnsParseableInvalidSet(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(validConfigDir)); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(root, "gaggles", "acme-web", "workflows", "implementation.yaml")
	data, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "        pass: push-branch", "        pass: ghost-state", 1))
	if err := os.WriteFile(workflow, data, 0o644); err != nil {
		t.Fatal(err)
	}

	set, report, err := LoadConfigDirForComparison(root)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if set == nil || len(set.Workflows) == 0 {
		t.Fatalf("expected parseable workflows with validation error, got %+v", set)
	}
	if report == nil || !report.HasErrors() {
		t.Fatalf("expected a report with errors, got %+v", report)
	}
}

// TestLoadConfigDirRejectsCompilerOnlyDefect is the #3664 regression at the
// loader boundary: a definition the versioned compiler refuses — here a
// contextFrom source naming no state in the workflow — used to load cleanly
// through LoadConfigDir even though `goobers validate` and daemon startup
// both rejected it, leaving GitOps and config-sync consumers with the weaker
// verdict. Canonical loading now fails closed on it like any other invalid
// config.
func TestLoadConfigDirRejectsCompilerOnlyDefect(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(validConfigDir)); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(root, "gaggles", "acme-web", "workflows", "implementation.yaml")
	data, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(data), "      contextFrom:\n        - query-backlog", "      contextFrom:\n        - ghost-stage", 1)
	if patched == string(data) {
		t.Fatal("fixture no longer contains the contextFrom list this test patches")
	}
	if err := os.WriteFile(workflow, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	set, report, err := LoadConfigDir(root)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if set != nil {
		t.Fatalf("expected a nil ConfigSet on an invalid directory, got %+v", set)
	}
	if report == nil || !report.HasErrors() {
		t.Fatalf("expected a report with errors, got %+v", report)
	}
	var found bool
	for _, issue := range report.Issues {
		if strings.Contains(issue.Message, `contextFrom source "ghost-stage" is not a defined task or gate`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("report did not name the unresolved contextFrom source: %+v", report.Issues)
	}
}

func TestLoadConfigDirIgnoresAssetDefinitions(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(validConfigDir)); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "goober.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(filepath.Dir(source), "assets", "duplicate.yaml")
	if err := os.MkdirAll(filepath.Dir(asset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, data, 0o644); err != nil {
		t.Fatal(err)
	}
	set, report, err := LoadConfigDir(root)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Goobers) != 18 {
		t.Fatalf("asset definition leaked into config set: got %d goobers", len(set.Goobers))
	}
}

func TestLoadConfigDirIgnoresSkillPackageYAML(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(validConfigDir)); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "goober.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	supportFile := filepath.Join(root, "gaggles", "acme-web", "skills", "implement", "references", "cases.yaml")
	if err := os.MkdirAll(filepath.Dir(supportFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(supportFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	set, report, err := LoadConfigDir(root)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Goobers) != 18 {
		t.Fatalf("skill support file leaked into config set: got %d goobers", len(set.Goobers))
	}
}

func TestLoadConfigDirIgnoresHiddenRepositoryMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(validConfigDir)); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	toolkitManifest := filepath.Join(root, ".goobers", "agent-toolkit", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(toolkitManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolkitManifest, manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	set, report, err := LoadConfigDir(root)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if set.Manifest == nil {
		t.Fatalf("hidden metadata changed loaded config: %+v", set)
	}
}

func TestLoadConfigDirMissingDir(t *testing.T) {
	set, report, err := LoadConfigDir("../../does/not/exist")
	if err == nil {
		t.Fatalf("expected an error for a missing config directory, got set=%+v report=%+v", set, report)
	}
}

func TestReadDocsIncludesSymlinkedYAML(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "linked.yaml")
	if err := os.WriteFile(target, []byte(`kind: Manifest
metadata: {name: linked}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked.yaml")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	docs, err := readDocs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].kind != "Manifest" || docs[0].name != "linked" || docs[0].file != "linked.yaml" {
		t.Fatalf("readDocs = %+v, want linked Manifest", docs)
	}
}

func TestReadDocsMissingRootReturnsError(t *testing.T) {
	if _, err := readDocs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("readDocs missing root succeeded, want an error")
	}
}

func TestCallersDoNotDiscardConfigReports(t *testing.T) {
	root := filepath.Clean("../..")
	loaders := map[string]bool{
		"LoadConfigDir":                 true,
		"LoadConfigSource":              true,
		"LoadConfigDirForComparison":    true,
		"LoadConfigSourceForComparison": true,
		"loadConfigDirectory":           true,
		"configLoader":                  true,
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) < 2 || len(assignment.Rhs) != 1 {
				return true
			}
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch function := call.Fun.(type) {
			case *ast.Ident:
				name = function.Name
			case *ast.SelectorExpr:
				name = function.Sel.Name
			}
			if !loaders[name] {
				return true
			}
			identifier, ok := assignment.Lhs[1].(*ast.Ident)
			if ok && identifier.Name == "_" {
				t.Errorf("%s: config validation report returned by %s is discarded", fset.Position(identifier.Pos()), name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
