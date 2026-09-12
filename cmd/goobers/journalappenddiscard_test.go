package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These three discards target run-journal interfaces, not *journal.InstanceLog.
// They predate #4873 and intentionally preserve their operations when a
// secondary run annotation cannot be written. Keeping the registry exact makes
// the repository-wide syntactic guard fail if another discard is introduced or
// one of these audited exceptions disappears.
var auditedNonInstanceAppendDiscards = map[string]int{
	"internal/executor/shell.go:refuseGuardedCredentialAccess": 1,
	"internal/runner/baseline2971.go:releaseBaselineParks":     1,
	"internal/runner/run.go:recordUnpushedDiff":                1,
}

// TestIntentionalJournalAppendDiscardsUseObservableHelper keeps the explicit
// best-effort boundary complete. A new ignored Append anywhere in production
// Go would otherwise recreate #4873 outside the directories that happened to
// contain the original findings, without touching the counter.
func TestIntentionalJournalAppendDiscardsUseObservableHelper(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate structural test")
	}
	repo := filepath.Dir(filepath.Dir(filepath.Dir(current)))
	seenExceptions := make(map[string]int, len(auditedNonInstanceAppendDiscards))
	err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		files := token.NewFileSet()
		parsed, err := parser.ParseFile(files, path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			key := filepath.ToSlash(relative) + ":" + function.Name.Name
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if !isDiscardedAppend(node) {
					return true
				}
				if expected := auditedNonInstanceAppendDiscards[key]; expected > 0 {
					seenExceptions[key]++
					if seenExceptions[key] > expected {
						t.Errorf("%s: additional discarded Append exceeds audited non-instance count %d", files.Position(node.Pos()), expected)
					}
					return true
				}
				t.Errorf("%s: discarded Append must use the observable best-effort boundary", files.Position(node.Pos()))
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for exception, expected := range auditedNonInstanceAppendDiscards {
		if seenExceptions[exception] != expected {
			t.Errorf("audited non-instance Append discard %q count = %d, want %d", exception, seenExceptions[exception], expected)
		}
	}
}

func TestDiscardedAppendGuardAllowsPropagatedResults(t *testing.T) {
	const source = `package fixture
type appender interface { Append(int) error }
func handled(a appender) error {
	if err := a.Append(1); err != nil { return err }
	err := a.Append(2)
	if err != nil { return err }
	return a.Append(3)
}
func discarded(a appender) {
	_ = a.Append(4)
	a.Append(5)
	defer a.Append(6)
	go a.Append(7)
}`
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var discarded int
	ast.Inspect(parsed, func(node ast.Node) bool {
		if isDiscardedAppend(node) {
			discarded++
		}
		return true
	})
	if discarded != 4 {
		t.Fatalf("discarded Append calls = %d, want blank-assigned, expression, deferred, and launched calls", discarded)
	}
}

func isDiscardedAppend(node ast.Node) bool {
	switch statement := node.(type) {
	case *ast.AssignStmt:
		return len(statement.Lhs) == 1 && isBlankIdentifier(statement.Lhs[0]) && len(statement.Rhs) == 1 && isAppendCall(statement.Rhs[0])
	case *ast.ExprStmt:
		return isAppendCall(statement.X)
	case *ast.DeferStmt:
		return isAppendCall(statement.Call)
	case *ast.GoStmt:
		return isAppendCall(statement.Call)
	default:
		return false
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
