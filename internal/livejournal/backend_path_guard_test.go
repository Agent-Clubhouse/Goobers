package livejournal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// File-only reads belong to explicit borrowed-file paths. Owned runs must use
// the backend reader, including on retries after their writer handle retired.
func TestOwnedBackendReadsDoNotBypassJournalBackend(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, violation := range backendPathViolations(parsed) {
			t.Errorf("%s: %s", file.Name(), violation)
		}
	}
}

// Inspect references rather than calls: os.Stat/Open, aliases and method values
// must not create another file-only path for an owned backend run.
func backendPathViolations(parsed *ast.File) []string {
	var violations []string
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		guarded := map[ast.Node]bool{}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			condition, ok := node.(*ast.IfStmt)
			if !ok || !isRunSelector(condition.Cond, "adopted") {
				return true
			}
			ast.Inspect(condition.Body, func(n ast.Node) bool {
				guarded[n] = true
				return true
			})
			return true
		})
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok || guarded[node] {
				return true
			}
			if isRunSelector(sel, "dir") {
				violations = append(violations, fn.Name.Name+": run.dir outside if run.adopted")
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			pathRead := (pkg.Name == "journal" && (sel.Sel.Name == "OpenReadOnly" || sel.Sel.Name == "OpenRead" || sel.Sel.Name == "Recorded")) || (pkg.Name == "os" && sel.Sel.Name == "ReadFile")
			// Adopt accepts a concrete *journal.Run; its initial read uses the
			// borrowed handle's local dir before constructing a liveRun.
			if pathRead && fn.Name.Name != "Adopt" {
				violations = append(violations, fn.Name.Name+": unguarded "+pkg.Name+"."+sel.Sel.Name)
			}
			return true
		})
	}
	return violations
}

func isRunSelector(node ast.Node, field string) bool {
	sel, ok := node.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != field {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "run"
}

func TestBackendPathGuardRejectsMutations(t *testing.T) {
	for _, mutation := range []string{
		"journal.OpenReadOnly(run.dir)",
		"journal.Recorded(run.dir)",
		"os.Stat(run.dir)",
		"os.Open(run.dir)",
		"dir := run.dir; os.Open(dir)",
		"open := journal.OpenReadOnly; open(run.dir)",
		"open := journal.OpenReadOnly; open(dir)",
		"if run.adopted {} else { os.Open(run.dir) }",
		"if other.adopted { os.Open(run.dir) }",
		"if !run.adopted { os.Open(run.dir) }",
	} {
		t.Run(mutation, func(t *testing.T) {
			parsed, err := parser.ParseFile(token.NewFileSet(), "mutation.go", "package livejournal; func read() {"+mutation+"}", 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(backendPathViolations(parsed)) == 0 {
				t.Fatal("guard accepted owned-backend file read")
			}
		})
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "adopted.go", "package livejournal; func read() { if run.adopted { open := journal.OpenReadOnly; open(run.dir); os.Stat(run.dir) } }", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := backendPathViolations(parsed); len(got) != 0 {
		t.Fatalf("guard rejected explicit adopted-file path: %v", got)
	}
}
