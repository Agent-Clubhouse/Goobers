package journal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestErrorBackedProducersUseStructuredHelpers(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		filepath.Clean("internal/journal/errorcause.go"): true,
	}
	for _, dir := range []string{"api", "cmd", "internal"} {
		base := filepath.Join(root, dir)
		if err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", "node_modules", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.Clean(rel)
			if allowed[rel] {
				return nil
			}
			if err := rejectFlattenedErrorModelMessages(rel, path); err != nil {
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func rejectFlattenedErrorModelMessages(rel, path string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return err
	}
	var badPos token.Position
	var badReason string
	ast.Inspect(file, func(node ast.Node) bool {
		if badReason != "" {
			return false
		}
		lit, ok := node.(*ast.CompositeLit)
		if !ok || !isErrorModelType(lit.Type) {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok || keyName(kv.Key) != "Message" {
				continue
			}
			if flattensDirectErrorCall(kv.Value) {
				badPos = fset.Position(kv.Value.Pos())
				badReason = "direct .Error()"
				return false
			}
			if flattensErrorWithSprintf(kv.Value) {
				badPos = fset.Position(kv.Value.Pos())
				badReason = "fmt.Sprintf %v/%s with error-like argument"
				return false
			}
		}
		return true
	})
	if badReason == "" {
		return nil
	}
	return &flattenedErrorProducerError{file: rel, line: badPos.Line, reason: badReason}
}

type flattenedErrorProducerError struct {
	file   string
	line   int
	reason string
}

func (e *flattenedErrorProducerError) Error() string {
	return e.file + ":" + strconv.Itoa(e.line) + " flattens an error into ErrorInfo/ErrorDetail Message via " + e.reason + "; use ErrorInfoFor/ErrorDetailFor"
}

func isErrorModelType(expr ast.Expr) bool {
	switch typ := expr.(type) {
	case *ast.Ident:
		return typ.Name == "ErrorDetail" || typ.Name == "ErrorInfo"
	case *ast.SelectorExpr:
		pkg, ok := typ.X.(*ast.Ident)
		if !ok {
			return false
		}
		return (pkg.Name == "journal" && typ.Sel.Name == "ErrorDetail") ||
			(pkg.Name == "apiv1" && typ.Sel.Name == "ErrorInfo")
	default:
		return false
	}
}

func keyName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func flattensDirectErrorCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Error"
}

func flattensErrorWithSprintf(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || !isFmtSprintf(call.Fun) || len(call.Args) < 2 {
		return false
	}
	format, ok := stringLiteral(call.Args[0])
	if !ok || (!strings.Contains(format, "%v") && !strings.Contains(format, "%s")) {
		return false
	}
	for _, arg := range call.Args[1:] {
		if isErrorLikeExpr(arg) {
			return true
		}
	}
	return false
}

func isFmtSprintf(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt"
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

func isErrorLikeExpr(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.Ident:
		return isErrorLikeName(v.Name)
	case *ast.SelectorExpr:
		return v.Sel != nil && isErrorLikeName(v.Sel.Name)
	case *ast.CallExpr:
		return flattensDirectErrorCall(v)
	default:
		return false
	}
}

func isErrorLikeName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "err" || lower == "cause" || strings.HasSuffix(lower, "err") || strings.HasSuffix(lower, "error")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found while locating repository root")
		}
		dir = parent
	}
}
