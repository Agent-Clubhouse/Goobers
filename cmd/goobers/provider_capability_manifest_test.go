package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/providerstage"
)

type providerCapabilityCallGraph struct {
	functions    map[string]*ast.BlockStmt
	capabilities map[string]capability.Capability
}

func TestProviderCapabilityManifestCoversCommandImplementations(t *testing.T) {
	graph := loadProviderCapabilityCallGraph(t)

	for _, command := range cliCommands {
		name := command.names[0]
		entry, manifested := providerstage.Lookup(name)

		root := cliHandlerName(t, command.run)
		used, unresolved := graph.capabilityUses(root)
		if len(unresolved) > 0 {
			t.Errorf("%s has providerToken uses the drift check cannot resolve: %s", name, strings.Join(unresolved, ", "))
		}
		if len(used) == 0 {
			continue
		}
		if !manifested {
			t.Errorf("built-in subcommand %q uses provider capabilities %v but has no manifest entry", name, sortedCapabilityNames(used))
			continue
		}

		declared := make(map[capability.Capability]bool, len(entry.Capabilities))
		for _, use := range entry.Capabilities {
			declared[use.Capability] = true
		}
		if missing := undeclaredCapabilityUses(used, declared); len(missing) > 0 {
			t.Errorf("built-in subcommand %q uses capabilities absent from its manifest: %v", name, missing)
		}
	}
}

func TestProviderCapabilityDriftCheckRejectsSyntheticUndeclaredUse(t *testing.T) {
	graph := providerCapabilityCallGraph{
		functions: parseProviderCapabilityFunctions(t, "synthetic.go", `package main
func runSynthetic() {
	providerToken(capability.GitHubIssuesWrite)
}`),
		capabilities: map[string]capability.Capability{
			"GitHubIssuesWrite": capability.GitHubIssuesWrite,
		},
	}
	used, unresolved := graph.capabilityUses("runSynthetic")
	if len(unresolved) > 0 {
		t.Fatalf("synthetic use was not resolved: %v", unresolved)
	}
	missing := undeclaredCapabilityUses(used, map[capability.Capability]bool{
		capability.GitHubPRWrite: true,
	})
	if len(missing) != 1 || missing[0] != capability.GitHubIssuesWrite {
		t.Fatalf("undeclaredCapabilityUses = %v, want [%s]", missing, capability.GitHubIssuesWrite)
	}
}

func loadProviderCapabilityCallGraph(t *testing.T) providerCapabilityCallGraph {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob command sources: %v", err)
	}
	functions := map[string]*ast.BlockStmt{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for name, body := range parseProviderCapabilityFile(t, file) {
			functions[name] = body
		}
	}
	return providerCapabilityCallGraph{
		functions:    functions,
		capabilities: parseCapabilityConstants(t, filepath.Join("..", "..", "internal", "capability", "capability.go")),
	}
}

func parseProviderCapabilityFunctions(t *testing.T, name, source string) map[string]*ast.BlockStmt {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return functionBodies(file)
}

func parseProviderCapabilityFile(t *testing.T, path string) map[string]*ast.BlockStmt {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return functionBodies(file)
}

func functionBodies(file *ast.File) map[string]*ast.BlockStmt {
	functions := map[string]*ast.BlockStmt{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Body == nil {
			continue
		}
		functions[function.Name.Name] = function.Body
	}
	return functions
}

func parseCapabilityConstants(t *testing.T, path string) map[string]capability.Capability {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse capability registry: %v", err)
	}
	capabilities := map[string]capability.Capability{}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("unquote capability %s: %v", value.Names[0].Name, err)
			}
			capabilities[value.Names[0].Name] = capability.Capability(text)
		}
	}
	return capabilities
}

func cliHandlerName(t *testing.T, handler cliCommandHandler) string {
	t.Helper()
	function := runtime.FuncForPC(reflect.ValueOf(handler).Pointer())
	if function == nil {
		t.Fatal("CLI command handler has no runtime function")
	}
	name := function.Name()
	return name[strings.LastIndex(name, ".")+1:]
}

func (g providerCapabilityCallGraph) capabilityUses(root string) (map[capability.Capability]bool, []string) {
	used := map[capability.Capability]bool{}
	visited := map[string]bool{}
	var unresolved []string

	var walk func(string)
	walk = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		body, ok := g.functions[name]
		if !ok {
			return
		}

		callees := map[string]bool{}
		ast.Inspect(body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if callee.Name != "providerToken" {
				callees[callee.Name] = true
				return true
			}
			if len(call.Args) != 1 {
				unresolved = append(unresolved, name+": unexpected providerToken argument count")
				return true
			}
			selector, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				unresolved = append(unresolved, name+": non-constant providerToken capability")
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "capability" {
				unresolved = append(unresolved, name+": non-capability providerToken argument")
				return true
			}
			capabilityValue, ok := g.capabilities[selector.Sel.Name]
			if !ok {
				unresolved = append(unresolved, name+": unknown capability."+selector.Sel.Name)
				return true
			}
			used[capabilityValue] = true
			return true
		})
		for callee := range callees {
			walk(callee)
		}
	}

	walk(root)
	sort.Strings(unresolved)
	return used, unresolved
}

func undeclaredCapabilityUses(used, declared map[capability.Capability]bool) []capability.Capability {
	var missing []capability.Capability
	for capabilityValue := range used {
		if !declared[capabilityValue] {
			missing = append(missing, capabilityValue)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing
}

func sortedCapabilityNames(values map[capability.Capability]bool) []capability.Capability {
	names := make([]capability.Capability, 0, len(values))
	for value := range values {
		names = append(names, value)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}
