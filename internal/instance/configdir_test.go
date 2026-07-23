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
	if len(set.Gaggles) != 2 || !gotGaggles["acme-web"] || !gotGaggles["dotnet-service"] {
		t.Fatalf("unexpected gaggles: %+v", set.Gaggles)
	}
	// config-examples ships seven goobers (acme-web: coder, curator,
	// implementer, nominator, reviewer; dotnet-service: dotnet-implementer,
	// dotnet-reviewer) and seven workflows (acme-web's six + the dotnet-service
	// reference's dotnet-implementation, #1093); check membership, not order.
	gotGoobers := map[string]bool{}
	for _, g := range set.Goobers {
		gotGoobers[g.Name] = true
	}
	wantGoobers := []string{"coder", "curator", "implementer", "nominator", "reviewer", "dotnet-implementer", "dotnet-reviewer"}
	if len(set.Goobers) != len(wantGoobers) {
		t.Fatalf("unexpected goobers: %+v", set.Goobers)
	}
	for _, name := range wantGoobers {
		if !gotGoobers[name] {
			t.Fatalf("missing goober %q; got: %+v", name, set.Goobers)
		}
	}
	gotWorkflows := map[string]bool{}
	for _, w := range set.Workflows {
		gotWorkflows[w.Name] = true
	}
	wantWorkflows := []string{"default-implement", "backlog-curation", "implementation", "work-nomination", "merge-review", "todo-check", "dotnet-implementation"}
	if len(set.Workflows) != len(wantWorkflows) {
		t.Fatalf("unexpected workflows: %+v", set.Workflows)
	}
	for _, name := range wantWorkflows {
		if !gotWorkflows[name] {
			t.Fatalf("missing workflow %q; got: %+v", name, set.Workflows)
		}
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
	data = []byte(strings.Replace(string(data), "        pass: local-ci", "        pass: ghost-state", 1))
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
	if len(set.Goobers) != 7 {
		t.Fatalf("asset definition leaked into config set: got %d goobers", len(set.Goobers))
	}
}

func TestLoadConfigDirMissingDir(t *testing.T) {
	set, report, err := LoadConfigDir("../../does/not/exist")
	if err == nil {
		t.Fatalf("expected an error for a missing config directory, got set=%+v report=%+v", set, report)
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
