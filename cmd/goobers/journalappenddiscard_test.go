package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIntentionalJournalAppendDiscardsUseObservableHelper keeps the explicit
// best-effort boundary complete. A new ignored Append in the scheduler or
// daemon would otherwise recreate #4873 without touching the counter.
func TestIntentionalJournalAppendDiscardsUseObservableHelper(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate structural test")
	}
	repo := filepath.Dir(filepath.Dir(filepath.Dir(current)))
	for _, relative := range []string{"cmd/goobers", "internal/localscheduler"} {
		directory := filepath.Join(repo, relative)
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			files := token.NewFileSet()
			parsed, err := parser.ParseFile(files, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				switch statement := node.(type) {
				case *ast.AssignStmt:
					if len(statement.Lhs) == 1 && isBlankIdentifier(statement.Lhs[0]) && len(statement.Rhs) == 1 && isAppendCall(statement.Rhs[0]) {
						t.Errorf("%s: ignored Append must use AppendBestEffort", files.Position(statement.Pos()))
					}
				case *ast.ExprStmt:
					if isAppendCall(statement.X) {
						t.Errorf("%s: discarded Append result must use AppendBestEffort", files.Position(statement.Pos()))
					}
				}
				return true
			})
		}
	}
}

func isBlankIdentifier(expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "_"
}

func isAppendCall(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "Append"
}
