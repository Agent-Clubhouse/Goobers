// Command integration discovers, validates, and runs build-tagged integration tests.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/testdep"
)

type scanResult struct {
	packages     []string
	dependencies map[string]bool
}

func main() {
	goCommand := flag.String("go", "go", "Go command used to run integration tests")
	flag.Parse()

	root, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "integration: get working directory: %v\n", err)
		os.Exit(1)
	}
	os.Exit(run(root, *goCommand, os.Stdout, os.Stderr))
}

func run(root, goCommand string, stdout, stderr io.Writer) int {
	result, err := scanIntegration(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "integration: %v\n", err)
		return 1
	}
	if err := validateInventory(result.dependencies); err != nil {
		_, _ = fmt.Fprintf(stderr, "integration: %v\n", err)
		return 1
	}
	if len(result.packages) == 0 {
		_, _ = fmt.Fprintln(stderr, "integration: no integration-tagged tests found")
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "Declared integration dependencies:")
	for _, dependency := range testdep.Dependencies() {
		_, _ = fmt.Fprintf(stdout, "  %s - %s\n", dependency.Name, dependency.InstallHint)
	}

	args := []string{"test", "-v", "-tags=integration", "-run=^TestIntegration"}
	args = append(args, result.packages...)
	command := exec.Command(goCommand, args...)
	command.Dir = root
	command.Env = os.Environ()
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		_, _ = fmt.Fprintf(stderr, "integration: %v\n", err)
		return 1
	}
	return 0
}

func validateInventory(used map[string]bool) error {
	declared := make(map[string]bool)
	for _, dependency := range testdep.Dependencies() {
		declared[dependency.Name] = true
	}

	var problems []string
	for name := range used {
		if !declared[name] {
			problems = append(problems, fmt.Sprintf("dependency %q is required by a test but absent from the inventory", name))
		}
	}
	for name := range declared {
		if !used[name] {
			problems = append(problems, fmt.Sprintf("inventory dependency %q is not required by an integration test", name))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("dependency inventory drift:\n  %s", strings.Join(problems, "\n  "))
}

func scanIntegration(root string) (scanResult, error) {
	result := scanResult{dependencies: make(map[string]bool)}
	packageSet := make(map[string]bool)
	var violations []string

	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			rel, err := filepath.Rel(root, filePath)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			switch rel {
			case ".git", ".goobers", "portal/node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		tagged, err := hasIntegrationTag(data)
		if err != nil {
			return fmt.Errorf("%s: %w", filePath, err)
		}
		if !tagged {
			return nil
		}

		relDir, err := filepath.Rel(root, filepath.Dir(filePath))
		if err != nil {
			return err
		}
		packageSet["./"+filepath.ToSlash(relDir)] = true

		fileDependencies, fileViolations, err := inspectIntegrationFile(filePath, data)
		if err != nil {
			return err
		}
		for _, name := range fileDependencies {
			result.dependencies[name] = true
		}
		violations = append(violations, fileViolations...)
		return nil
	})
	if err != nil {
		return scanResult{}, err
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return scanResult{}, fmt.Errorf("integration dependency guard failed:\n  %s", strings.Join(violations, "\n  "))
	}

	for packagePath := range packageSet {
		result.packages = append(result.packages, packagePath)
	}
	sort.Strings(result.packages)
	return result, nil
}

func hasIntegrationTag(data []byte) (bool, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return false, nil
		}
		if !strings.HasPrefix(line, "//go:build ") {
			continue
		}
		expression, err := constraint.Parse(line)
		if err != nil {
			return false, fmt.Errorf("parse build constraint: %w", err)
		}
		return containsPositiveIntegrationTag(expression, false), nil
	}
	return false, nil
}

func containsPositiveIntegrationTag(expression constraint.Expr, negated bool) bool {
	switch current := expression.(type) {
	case *constraint.TagExpr:
		return current.Tag == "integration" && !negated
	case *constraint.NotExpr:
		return containsPositiveIntegrationTag(current.X, !negated)
	case *constraint.AndExpr:
		return containsPositiveIntegrationTag(current.X, negated) ||
			containsPositiveIntegrationTag(current.Y, negated)
	case *constraint.OrExpr:
		return containsPositiveIntegrationTag(current.X, negated) ||
			containsPositiveIntegrationTag(current.Y, negated)
	default:
		return false
	}
}

func inspectIntegrationFile(filePath string, data []byte) ([]string, []string, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filePath, data, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", filePath, err)
	}

	execAliases := make(map[string]bool)
	testdepAliases := make(map[string]bool)
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: parse import %s: %w", filePath, spec.Path.Value, err)
		}
		alias := path.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		switch importPath {
		case "os/exec":
			execAliases[alias] = true
		case "github.com/goobers/goobers/internal/testdep":
			testdepAliases[alias] = true
		}
	}

	var dependencies []string
	var violations []string
	requireCalls := 0
	integrationTests := 0
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(function.Name.Name, "Test") {
			continue
		}
		if !strings.HasPrefix(function.Name.Name, "TestIntegration") {
			violations = append(violations, fmt.Sprintf(
				"%s: integration tests must use the TestIntegration prefix",
				files.Position(function.Name.Pos()),
			))
			continue
		}
		integrationTests++
		if function.Body == nil || len(function.Body.List) == 0 ||
			!isRequireStatement(function.Body.List[0], testdepAliases) {
			violations = append(violations, fmt.Sprintf(
				"%s: integration tests must call testdep.Require as their first statement",
				files.Position(function.Name.Pos()),
			))
		}
	}
	if integrationTests == 0 {
		violations = append(violations, fmt.Sprintf("%s: integration-tagged file has no TestIntegration function", filePath))
	}

	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, _ := selector.X.(*ast.Ident)
		position := files.Position(call.Pos())

		if receiver != nil && execAliases[receiver.Name] && selector.Sel.Name == "LookPath" {
			violations = append(violations, fmt.Sprintf("%s: direct exec.LookPath is forbidden; use testdep.Require", position))
		}
		if selector.Sel.Name == "Skip" || selector.Sel.Name == "Skipf" || selector.Sel.Name == "SkipNow" {
			violations = append(violations, fmt.Sprintf("%s: raw test skip is forbidden in the integration tier; use testdep.Require", position))
		}
		if receiver == nil || !testdepAliases[receiver.Name] || selector.Sel.Name != "Require" {
			return true
		}

		requireCalls++
		if len(call.Args) < 2 {
			violations = append(violations, fmt.Sprintf("%s: testdep.Require must name at least one dependency", position))
			return true
		}
		for _, argument := range call.Args[1:] {
			literal, ok := argument.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				violations = append(violations, fmt.Sprintf("%s: testdep.Require dependencies must be string literals", files.Position(argument.Pos())))
				continue
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil || name == "" {
				violations = append(violations, fmt.Sprintf("%s: invalid testdep.Require dependency %s", files.Position(argument.Pos()), literal.Value))
				continue
			}
			dependencies = append(dependencies, name)
		}
		return true
	})
	if requireCalls == 0 {
		violations = append(violations, fmt.Sprintf("%s: integration-tagged file has no testdep.Require declaration", filePath))
	}
	return dependencies, violations, nil
}

func isRequireStatement(statement ast.Stmt, aliases map[string]bool) bool {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Require" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && aliases[receiver.Name]
}
