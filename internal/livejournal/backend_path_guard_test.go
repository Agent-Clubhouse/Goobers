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
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			guarded := map[*ast.CallExpr]bool{}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				condition, ok := node.(*ast.IfStmt)
				if !ok {
					return true
				}
				sel, ok := condition.Cond.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "adopted" {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "run" {
					return true
				}
				ast.Inspect(condition.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						guarded[call] = true
					}
					return true
				})
				return true
			})
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				pathRead := (pkg.Name == "journal" && (sel.Sel.Name == "OpenReadOnly" || sel.Sel.Name == "OpenRead" || sel.Sel.Name == "Recorded")) || (pkg.Name == "os" && sel.Sel.Name == "ReadFile")
				if !pathRead {
					return true
				}
				// Adopt only accepts a concrete *journal.Run and refreshAdopted is its
				// borrowed-handle refresh. All other file reads need an adopted guard.
				borrowedOnly := fn.Name.Name == "Adopt" || fn.Name.Name == "refreshAdopted"
				if !guarded[call] && !borrowedOnly {
					t.Errorf("%s:%s bypasses backend with %s.%s", file.Name(), fn.Name.Name, pkg.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}
