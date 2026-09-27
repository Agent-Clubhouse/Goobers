package testgit

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
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
	if positions[0].Line != 8 || positions[1].Line != 13 {
		t.Fatalf("invocation lines = %d, %d; want 8, 13", positions[0].Line, positions[1].Line)
	}
}

func TestDirectGitInvocationsRespectAliasBindings(t *testing.T) {
	src := `package sample
import "os/exec"

func aliased() {
	cmd := exec.Command
	cmd("git")
	cmd = func(string) {}
	cmd("git")
}

func shadowed(cmd func(string)) {
	cmd("git")
}
`
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	positions := directGitInvocations(parsed, fset)
	if len(positions) != 1 {
		t.Fatalf("detected %d git invocations, want 1: %#v", len(positions), positions)
	}
	if got := positions[0].Line; got != 6 {
		t.Fatalf("invocation line = %d, want 6", got)
	}
}

func directGitInvocations(parsed *ast.File, fset *token.FileSet) []token.Position {
	execAliases := map[string]bool{}
	dotImportExec := false
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
	visitor := directGitInvocationVisitor{
		fset:          fset,
		execAliases:   execAliases,
		dotImportExec: dotImportExec,
		info: types.Info{
			Defs: make(map[*ast.Ident]types.Object),
			Uses: make(map[*ast.Ident]types.Object),
		},
		varAliases: make(map[types.Object]string),
		positions:  nil,
	}
	_, _ = (&types.Config{Importer: importer.Default()}).Check(
		"sample",
		fset,
		[]*ast.File{parsed},
		&visitor.info,
	)
	ast.Walk(&visitor, parsed)
	return visitor.positions
}

type directGitInvocationVisitor struct {
	fset          *token.FileSet
	execAliases   map[string]bool
	dotImportExec bool
	info          types.Info
	varAliases    map[types.Object]string
	positions     []token.Position
}

func (v *directGitInvocationVisitor) Visit(node ast.Node) ast.Visitor {
	switch value := node.(type) {
	case *ast.AssignStmt:
		for i, lhs := range value.Lhs {
			if i >= len(value.Rhs) {
				break
			}
			ident, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			method, ok := v.execCommandAlias(value.Rhs[i])
			if ok {
				v.varAliases[v.binding(ident)] = method
			} else {
				delete(v.varAliases, v.binding(ident))
			}
		}
	case *ast.ValueSpec:
		for i, ident := range value.Names {
			if i >= len(value.Values) {
				continue
			}
			method, ok := v.execCommandAlias(value.Values[i])
			if ok {
				v.varAliases[v.binding(ident)] = method
			}
		}
	case *ast.CallExpr:
		v.checkCall(value)
	}
	return v
}

func (v *directGitInvocationVisitor) checkCall(call *ast.CallExpr) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if ok && (selector.Sel.Name == "Command" || selector.Sel.Name == "CommandContext") {
		pkgName, ok := selector.X.(*ast.Ident)
		if ok && v.execAliases[pkgName.Name] && directGitArg(call, selector.Sel.Name) {
			v.positions = append(v.positions, v.fset.Position(call.Pos()))
		}
		return
	}
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return
	}
	method, alias := v.varAliases[v.binding(ident)]
	if !alias && (ident.Name == "Command" || ident.Name == "CommandContext") &&
		(v.dotImportExec && isExecCommand(v.binding(ident))) {
		alias = v.dotImportExec
		method = ident.Name
	}
	if alias && directGitArg(call, method) {
		v.positions = append(v.positions, v.fset.Position(call.Pos()))
	}
}

func isExecCommand(object types.Object) bool {
	if object == nil || object.Pkg() == nil {
		return false
	}
	return object.Pkg().Path() == "os/exec" &&
		(object.Name() == "Command" || object.Name() == "CommandContext")
}

func (v *directGitInvocationVisitor) binding(ident *ast.Ident) types.Object {
	if object := v.info.Defs[ident]; object != nil {
		return object
	}
	return v.info.Uses[ident]
}

func (v *directGitInvocationVisitor) execCommandAlias(expr ast.Expr) (string, bool) {
	switch value := expr.(type) {
	case *ast.SelectorExpr:
		if value.Sel.Name != "Command" && value.Sel.Name != "CommandContext" {
			return "", false
		}
		pkgName, ok := value.X.(*ast.Ident)
		if !ok || !v.execAliases[pkgName.Name] {
			return "", false
		}
		return value.Sel.Name, true
	case *ast.Ident:
		if method, ok := v.varAliases[v.binding(value)]; ok {
			return method, true
		}
		if value.Name != "Command" && value.Name != "CommandContext" {
			return "", false
		}
		if v.dotImportExec {
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
