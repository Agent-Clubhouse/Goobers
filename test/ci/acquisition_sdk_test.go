package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func sdkAcquisitionSites(root string) ([]string, error) {
	var sites []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", ".codex", ".clubhouse":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found, err := temporalAcquisitionSites(filepath.ToSlash(rel), file)
		sites = append(sites, found...)
		return err
	})
	return sites, err
}

// Resolve the actual import path, not its local alias. A new direct SDK call
// bypassing our local-binary helper must be reviewed and declared even when it
// lives in a new package, a test, or a platform-specific file.
func temporalAcquisitionSites(path string, file *ast.File) ([]string, error) {
	alias := ""
	for _, imp := range file.Imports {
		value, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		if value != "go.temporal.io/sdk/testsuite" {
			continue
		}
		alias = "testsuite"
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		if alias == "." {
			return nil, fmt.Errorf("%s: dot import hides Temporal acquisition calls", path)
		}
	}
	if alias == "" {
		return nil, nil
	}
	var sites []string
	counts := make(map[string]int)
	for _, declaration := range file.Decls {
		scope := "package"
		if function, ok := declaration.(*ast.FuncDecl); ok {
			scope = function.Name.Name
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			// Inspect references as well as calls: assigning the SDK function
			// to a local variable must not bypass acquisition discovery.
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "StartDevServer" {
				return true
			}
			name, ok := selector.X.(*ast.Ident)
			if ok && name.Name == alias {
				counts[scope]++
				sites = append(sites, fmt.Sprintf("%s#%s[%d]:temporal-cli", path, scope, counts[scope]))
			}
			return true
		})
	}
	return sites, nil
}

func TestSDKAcquisitionGlobalReferencesHaveDistinctSites(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "globals.go", `package example
import sdk "go.temporal.io/sdk/testsuite"
var first = sdk.StartDevServer
var second = sdk.StartDevServer
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites, err := temporalAcquisitionSites("globals.go", file)
	if err != nil || len(sites) != 2 || sites[0] == sites[1] {
		t.Fatalf("global references collapsed: %v, %v", sites, err)
	}
}

func TestSDKAcquisitionDiscoveryFindsIndirectCalls(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`package example; import sdk "go.temporal.io/sdk/testsuite"; var start = sdk.StartDevServer`,
		`package example; import sdk "go.temporal.io/sdk/testsuite"; func start() { f := sdk.StartDevServer; f(ctx, opts) }`,
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "indirect.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		sites, err := temporalAcquisitionSites("indirect.go", file)
		if err != nil || len(sites) != 1 {
			t.Fatalf("indirect SDK acquisition missed: %v, %v", sites, err)
		}
	}
}

func TestSDKAcquisitionDiscoveryResolvesAliasAndIgnoresProse(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "new_test.go", `package example
import renamed "go.temporal.io/sdk/testsuite"
// testsuite.StartDevServer is only prose here.
func TestNew() { renamed.StartDevServer(ctx, opts) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites, err := temporalAcquisitionSites("new_test.go", file)
	if err != nil || len(sites) != 1 || sites[0] != "new_test.go#TestNew[1]:temporal-cli" {
		t.Fatalf("discovered %v, %v", sites, err)
	}
	if acquisitionDrift(nil, sites) == nil {
		t.Fatal("new direct SDK acquisition passed without a declaration")
	}
}
