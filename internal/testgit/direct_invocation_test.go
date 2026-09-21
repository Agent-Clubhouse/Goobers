package testgit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestNoDirectGitCommandsInTests(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		// Test-support files that generate fixtures for tests (e.g.
		// fixture.go under test/<pkg>) aren't named *_test.go — they're
		// ordinary package files imported by the test binary — but they
		// still must route git through the isolated helper.
		isTestSupport := base == "fixture.go"
		if (!strings.HasSuffix(path, "_test.go") && !isTestSupport) || base == filepath.Base(file) {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, position := range directGitInvocations(parsed, fset) {
			t.Errorf("%s:%d invokes git directly; use internal/testgit", path, position.Line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDirectGitInvocationsDetectedInDotImportsAndAliases(t *testing.T) {
	src := `package sample
import (
	"context"
	. "os/exec"
)

func dotImport() {
	Command("git", "status")
}

func alias() {
	cmd := CommandContext
	cmd(context.Background(), "git")
}
`
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	positions := directGitInvocations(parsed, fset)
	if len(positions) != 2 {
		t.Fatalf("detected %d git invocations, want 2: %#v", len(positions), positions)
	}
	if got := positions[0].Line; got != 8 && got != 13 {
		t.Fatalf("first invocation line = %d, want %d or %d", got, 8, 13)
	}
}

func directGitInvocations(parsed *ast.File, fset *token.FileSet) []token.Position {
	execAliases := map[string]bool{}
	dotImportExec := false
	varAliases := map[string]string{}
	for _, imported := range parsed.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil || importPath != "os/exec" {
			continue
		}
		name := "exec"
		if imported.Name != nil {
			name = imported.Name.Name
			if imported.Name.Name == "." {
				dotImportExec = true
				continue
			}
		}
		execAliases[name] = true
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			for i, rhs := range value.Rhs {
				if len(value.Lhs) <= i {
					break
				}
				ident, ok := value.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if method, ok := execCommandAlias(rhs, execAliases, dotImportExec); ok {
					varAliases[ident.Name] = method
				}
			}
		case *ast.ValueSpec:
			for i, val := range value.Values {
				if len(value.Names) <= i {
					break
				}
				ident := value.Names[i]
				if method, ok := execCommandAlias(val, execAliases, dotImportExec); ok {
					varAliases[ident.Name] = method
				}
			}
		}
		return true
	})
	var positions []token.Position
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := call.Fun
		selector, selectorOK := fun.(*ast.SelectorExpr)
		if selectorOK && (selector.Sel.Name == "Command" || selector.Sel.Name == "CommandContext") {
			pkgName, ok := selector.X.(*ast.Ident)
			if ok && execAliases[pkgName.Name] && directGitArg(call, selector.Sel.Name) {
				positions = append(positions, fset.Position(call.Pos()))
			}
			return true
		}
		ident, identOK := fun.(*ast.Ident)
		if identOK {
			if method, ok := varAliases[ident.Name]; ok && directGitArg(call, method) {
				positions = append(positions, fset.Position(call.Pos()))
				return true
			}
			if (ident.Name == "Command" || ident.Name == "CommandContext") && (dotImportExec || varAliases[ident.Name] != "") && directGitArg(call, ident.Name) {
				positions = append(positions, fset.Position(call.Pos()))
			}
		}
		return true
	})
	return positions
}

func execCommandAlias(expr ast.Expr, execAliases map[string]bool, dotImportExec bool) (string, bool) {
	switch value := expr.(type) {
	case *ast.SelectorExpr:
		if value.Sel.Name != "Command" && value.Sel.Name != "CommandContext" {
			return "", false
		}
		pkgName, ok := value.X.(*ast.Ident)
		if !ok || !execAliases[pkgName.Name] {
			return "", false
		}
		return value.Sel.Name, true
	case *ast.Ident:
		if value.Name != "Command" && value.Name != "CommandContext" {
			return "", false
		}
		if dotImportExec {
			return value.Name, true
		}
		return "", false
	default:
		return "", false
	}
}

func directGitArg(call *ast.CallExpr, method string) bool {
	index := 0
	if method == "CommandContext" {
		index = 1
	}
	if len(call.Args) <= index {
		return false
	}
	literal, ok := call.Args[index].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == "git"
}
