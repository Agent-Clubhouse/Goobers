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
		execAliases := map[string]bool{}
		for _, imported := range parsed.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil || importPath != "os/exec" {
				continue
			}
			name := "exec"
			if imported.Name != nil {
				name = imported.Name.Name
			}
			execAliases[name] = true
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Command" && selector.Sel.Name != "CommandContext") {
				return true
			}
			packageName, ok := selector.X.(*ast.Ident)
			if !ok || !execAliases[packageName.Name] {
				return true
			}
			index := 0
			if selector.Sel.Name == "CommandContext" {
				index = 1
			}
			if len(call.Args) <= index {
				return true
			}
			literal, ok := call.Args[index].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err == nil && value == "git" {
				position := fset.Position(call.Pos())
				t.Errorf("%s:%d invokes git directly; use internal/testgit", path, position.Line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
