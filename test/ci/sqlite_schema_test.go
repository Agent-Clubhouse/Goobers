package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// auditedSQLOpeners is the reviewed inventory of production database/sql open
// sites. Adding any Open/OpenDB call fails CI until its schema policy is added
// here deliberately. Read-only pools are listed too: they validate or inherit
// the schema version established by their package's writer.
var auditedSQLOpeners = []string{
	"internal/cancelreceipt/store.go:Open:Open",
	"internal/readmodel/existing_reader.go:OpenExistingReader:Open",
	"internal/readmodel/intake/intake.go:Open:Open",
	"internal/readmodel/rebuild.go:reopenLocked:Open",
	"internal/readmodel/store.go:Open:Open",
	"internal/readmodel/store.go:openReaderPool:Open",
	"internal/telemetry/rollup/db.go:Open:Open",
	"internal/telemetry/rollup/db.go:OpenExistingReader:Open",
	"internal/telemetry/rollup/db.go:openReaderPool:Open",
	"internal/triggerqueue/store.go:Open:Open",
}

func TestEmbeddedSQLiteStoreOpenersAreAudited(t *testing.T) {
	got, err := discoverSQLOpeners(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), auditedSQLOpeners...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("production database/sql openers changed without a schema-version audit\n got: %v\nwant: %v", got, want)
	}
}

func TestDiscoverSQLOpenersCatchesNonLexicalOpenForms(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "internal", "newstore", "store.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package newstore
import dbsql "database/sql"
const driver = "sqlite"
func Open() {
	_, _ = dbsql.
		Open(driver, "store.db")
	_ = dbsql.OpenDB(nil)
}
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := discoverSQLOpeners(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/newstore/store.go:Open:Open",
		"internal/newstore/store.go:Open:OpenDB",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discovered openers = %v, want %v", got, want)
	}
}

func discoverSQLOpeners(root string) ([]string, error) {
	var openers []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "node_modules" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		aliases := map[string]bool{}
		dotImport := false
		for _, imported := range file.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil || value != "database/sql" {
				continue
			}
			alias := "sql"
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			if alias == "." {
				dotImport = true
			} else if alias != "_" {
				aliases[alias] = true
			}
		}
		if len(aliases) == 0 && !dotImport {
			return nil
		}
		file, err = parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				method := ""
				switch called := call.Fun.(type) {
				case *ast.SelectorExpr:
					name, ok := called.X.(*ast.Ident)
					if ok && aliases[name.Name] && (called.Sel.Name == "Open" || called.Sel.Name == "OpenDB") {
						method = called.Sel.Name
					}
				case *ast.Ident:
					if dotImport && (called.Name == "Open" || called.Name == "OpenDB") {
						method = called.Name
					}
				}
				if method != "" {
					openers = append(openers, fmt.Sprintf("%s:%s:%s", filepath.ToSlash(rel), fn.Name.Name, method))
				}
				return true
			})
		}
		return nil
	})
	sort.Strings(openers)
	return openers, err
}
