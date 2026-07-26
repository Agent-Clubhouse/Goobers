// Command nophonehome rejects hardcoded production egress destinations and
// implicit telemetry exporters.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type egressAPI struct {
	destination int
	implicit    bool
}

var egressAPIs = map[string]map[string]egressAPI{
	"net": {
		"Dial":        {destination: 1},
		"DialTimeout": {destination: 1},
	},
	"net/http": {
		"Get":                   {destination: 0},
		"Head":                  {destination: 0},
		"NewRequest":            {destination: 1},
		"NewRequestWithContext": {destination: 2},
		"Post":                  {destination: 0},
		"PostForm":              {destination: 0},
	},
	"net/smtp": {
		"Dial":     {destination: 0},
		"SendMail": {destination: 0},
	},
	"google.golang.org/grpc": {
		"Dial":        {destination: 0},
		"DialContext": {destination: 1},
		"NewClient":   {destination: 0},
	},
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc": {
		"New":             {implicit: true},
		"WithEndpoint":    {destination: 0},
		"WithEndpointURL": {destination: 0},
	},
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp": {
		"New":             {implicit: true},
		"WithEndpoint":    {destination: 0},
		"WithEndpointURL": {destination: 0},
	},
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc": {
		"New":             {implicit: true},
		"WithEndpoint":    {destination: 0},
		"WithEndpointURL": {destination: 0},
	},
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp": {
		"New":             {implicit: true},
		"WithEndpoint":    {destination: 0},
		"WithEndpointURL": {destination: 0},
	},
}

type implicitEgressApproval struct {
	location       string
	options        string
	endpointSource string
}

var approvedImplicitEgress = map[string]implicitEgressApproval{
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc.New": {
		location:       "internal/telemetry/client.go:spanExporters",
		options:        "opts",
		endpointSource: "cfg.OTLPEndpoint",
	},
}

type finding struct {
	path    string
	line    int
	column  int
	message string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s", f.path, f.line, f.column, f.message)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/nophonehome [repository-root]")
		return 2
	}
	root := "."
	if len(args) == 1 {
		root = args[0]
	}
	findings, err := scan(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "no-phone-home: scan: %v\n", err)
		return 1
	}
	if len(findings) != 0 {
		for _, finding := range findings {
			_, _ = fmt.Fprintln(stderr, finding)
		}
		_, _ = fmt.Fprintln(stderr, "no-phone-home: remove the built-in destination; telemetry export is allowed only to an endpoint the user explicitly configures (SEC-048)")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "no-phone-home: no hardcoded egress destinations or implicit telemetry exporters")
	return 0
}

func scan(root string) ([]finding, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	var findings []finding
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skippedDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		fileFindings, err := scanFile(root, path)
		if err != nil {
			return err
		}
		findings = append(findings, fileFindings...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		if findings[i].line != findings[j].line {
			return findings[i].line < findings[j].line
		}
		return findings[i].column < findings[j].column
	})
	return findings, nil
}

func skippedDirectory(name string) bool {
	switch name {
	case ".git", ".goobers", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func scanFile(root, path string) ([]finding, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, fmt.Errorf("make %s relative to %s: %w", path, root, err)
	}
	rel = filepath.ToSlash(rel)
	imports, importFindings := monitoredImports(files, parsed, rel)
	fileBindings := staticBindings(parsed)
	findings := append([]finding(nil), importFindings...)

	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		implicitCalls := make(map[string][]*ast.CallExpr)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath := imports[pkg.Name]
			api, monitored := egressAPIs[importPath][selector.Sel.Name]
			if !monitored {
				return true
			}
			position := files.Position(call.Pos())
			callName := importPath + "." + selector.Sel.Name
			if api.implicit {
				implicitCalls[callName] = append(implicitCalls[callName], call)
				return true
			}
			if api.destination >= len(call.Args) {
				return true
			}
			bindings := bindingsAt(function, call, fileBindings)
			if destination, ok := staticString(call.Args[api.destination], bindings, nil); ok && strings.TrimSpace(destination) != "" {
				findings = append(findings, finding{
					path: rel, line: position.Line, column: position.Column,
					message: fmt.Sprintf("hardcoded network destination %q passed to %s", destination, callName),
				})
			}
			return true
		})
		location := rel + ":" + function.Name.Name
		for callName, calls := range implicitCalls {
			approval, approved := approvedImplicitEgress[callName]
			approved = approved &&
				approval.location == location &&
				len(calls) == 1 &&
				explicitlyConfiguredImplicitCall(function, calls[0], imports, fileBindings, callName, approval)
			if approved {
				continue
			}
			for _, call := range calls {
				position := files.Position(call.Pos())
				findings = append(findings, finding{
					path: rel, line: position.Line, column: position.Column,
					message: "implicit network destination via " + callName,
				})
			}
		}
	}
	findings = append(findings, telemetryEndpointDefaults(files, parsed, rel, fileBindings)...)
	return findings, nil
}

func monitoredImports(files *token.FileSet, parsed *ast.File, rel string) (map[string]string, []finding) {
	imports := make(map[string]string)
	var findings []finding
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if _, monitored := egressAPIs[importPath]; !monitored {
			continue
		}
		if spec.Name != nil && spec.Name.Name == "_" {
			continue
		}
		if spec.Name != nil && spec.Name.Name == "." {
			position := files.Position(spec.Pos())
			findings = append(findings, finding{
				path: rel, line: position.Line, column: position.Column,
				message: "dot import obscures monitored network package " + importPath,
			})
			continue
		}
		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = importPath
	}
	return imports, findings
}

func staticBindings(parsed *ast.File) map[string]ast.Expr {
	bindings := make(map[string]ast.Expr)
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range values.Names {
				if i < len(values.Values) {
					bindings[name.Name] = values.Values[i]
				}
			}
		}
	}
	return bindings
}

func bindingsAt(function *ast.FuncDecl, target ast.Node, fileBindings map[string]ast.Expr) map[string]ast.Expr {
	bindings := make(map[string]ast.Expr, len(fileBindings))
	for name, expression := range fileBindings {
		bindings[name] = expression
	}
	forgetFieldNames(bindings, function.Recv)
	forgetFieldNames(bindings, function.Type.Params)
	forgetFieldNames(bindings, function.Type.Results)

	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) != 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})

	callScopes := make(map[*ast.BlockStmt]bool)
	for node := target; node != nil; node = parents[node] {
		if block, ok := node.(*ast.BlockStmt); ok {
			callScopes[block] = true
		}
	}

	ast.Inspect(function.Body, func(node ast.Node) bool {
		if node == nil || node.Pos() >= target.Pos() {
			return false
		}
		block := enclosingBlock(node, parents)
		if block == nil || !callScopes[block] || node.End() >= target.Pos() {
			return true
		}
		switch value := node.(type) {
		case *ast.AssignStmt:
			for i, left := range value.Lhs {
				name, ok := left.(*ast.Ident)
				if !ok || name.Name == "_" {
					continue
				}
				if i < len(value.Rhs) {
					bindings[name.Name] = value.Rhs[i]
				} else {
					delete(bindings, name.Name)
				}
			}
			return false
		case *ast.ValueSpec:
			for i, name := range value.Names {
				if i < len(value.Values) {
					bindings[name.Name] = value.Values[i]
				} else {
					delete(bindings, name.Name)
				}
			}
			return false
		default:
			return true
		}
	})
	return bindings
}

func forgetFieldNames(bindings map[string]ast.Expr, fields *ast.FieldList) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		for _, name := range field.Names {
			delete(bindings, name.Name)
		}
	}
}

func enclosingBlock(node ast.Node, parents map[ast.Node]ast.Node) *ast.BlockStmt {
	for node = parents[node]; node != nil; node = parents[node] {
		if block, ok := node.(*ast.BlockStmt); ok {
			return block
		}
	}
	return nil
}

func explicitlyConfiguredImplicitCall(
	function *ast.FuncDecl,
	call *ast.CallExpr,
	imports map[string]string,
	fileBindings map[string]ast.Expr,
	callName string,
	approval implicitEgressApproval,
) bool {
	if len(call.Args) != 2 || !call.Ellipsis.IsValid() {
		return false
	}
	options, ok := call.Args[1].(*ast.Ident)
	if !ok || options.Name != approval.options {
		return false
	}
	importPath := strings.TrimSuffix(callName, ".New")
	configured := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if configured || node == nil || node.Pos() >= call.Pos() {
			return false
		}
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, left := range assignment.Lhs {
			if expressionName(left) != approval.options || i >= len(assignment.Rhs) {
				continue
			}
			appendCall, ok := assignment.Rhs[i].(*ast.CallExpr)
			if !ok || expressionName(appendCall.Fun) != "append" || len(appendCall.Args) < 2 ||
				expressionName(appendCall.Args[0]) != approval.options {
				continue
			}
			for _, expression := range appendCall.Args[1:] {
				optionCall, ok := expression.(*ast.CallExpr)
				if !ok {
					continue
				}
				selector, ok := optionCall.Fun.(*ast.SelectorExpr)
				if !ok || len(optionCall.Args) == 0 {
					continue
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok || imports[pkg.Name] != importPath {
					continue
				}
				api, monitored := egressAPIs[importPath][selector.Sel.Name]
				if !monitored || api.implicit || api.destination >= len(optionCall.Args) {
					continue
				}
				bindings := bindingsAt(function, optionCall, fileBindings)
				if derivesFrom(optionCall.Args[api.destination], bindings, approval.endpointSource, nil) {
					configured = true
					return false
				}
			}
		}
		return !configured
	})
	return configured
}

func derivesFrom(expression ast.Expr, bindings map[string]ast.Expr, source string, seen map[string]bool) bool {
	if expressionName(expression) == source {
		return true
	}
	switch value := expression.(type) {
	case *ast.Ident:
		if seen == nil {
			seen = make(map[string]bool)
		}
		if seen[value.Name] {
			return false
		}
		binding, ok := bindings[value.Name]
		if !ok {
			return false
		}
		seen[value.Name] = true
		derived := derivesFrom(binding, bindings, source, seen)
		delete(seen, value.Name)
		return derived
	case *ast.CallExpr:
		for _, argument := range value.Args {
			if derivesFrom(argument, bindings, source, seen) {
				return true
			}
		}
	case *ast.ParenExpr:
		return derivesFrom(value.X, bindings, source, seen)
	case *ast.BinaryExpr:
		return derivesFrom(value.X, bindings, source, seen) ||
			derivesFrom(value.Y, bindings, source, seen)
	case *ast.UnaryExpr:
		return derivesFrom(value.X, bindings, source, seen)
	}
	return false
}

func staticString(expression ast.Expr, bindings map[string]ast.Expr, seen map[string]bool) (string, bool) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(value.Value)
		return text, err == nil
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", false
		}
		left, leftOK := staticString(value.X, bindings, seen)
		right, rightOK := staticString(value.Y, bindings, seen)
		if !leftOK || !rightOK {
			return "", false
		}
		return left + right, true
	case *ast.ParenExpr:
		return staticString(value.X, bindings, seen)
	case *ast.Ident:
		if seen == nil {
			seen = make(map[string]bool)
		}
		if seen[value.Name] {
			return "", false
		}
		binding, ok := bindings[value.Name]
		if !ok {
			return "", false
		}
		seen[value.Name] = true
		text, ok := staticString(binding, bindings, seen)
		delete(seen, value.Name)
		return text, ok
	default:
		return "", false
	}
}

func telemetryEndpointDefaults(files *token.FileSet, parsed *ast.File, rel string, bindings map[string]ast.Expr) []finding {
	var findings []finding
	report := func(name string, expression ast.Expr) {
		if !sensitiveEndpointName(name) {
			return
		}
		value, ok := staticString(expression, bindings, nil)
		if !ok || strings.TrimSpace(value) == "" || strings.HasSuffix(strings.ToLower(name), "env") {
			return
		}
		position := files.Position(expression.Pos())
		findings = append(findings, finding{
			path: rel, line: position.Line, column: position.Column,
			message: fmt.Sprintf("default telemetry/reporting endpoint %q assigned to %s", value, name),
		})
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			for i, left := range value.Lhs {
				if i < len(value.Rhs) {
					report(expressionName(left), value.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, name := range value.Names {
				if i < len(value.Values) {
					report(name.Name, value.Values[i])
				}
			}
		case *ast.CompositeLit:
			typeName := expressionName(value.Type)
			if !strings.Contains(strings.ToLower(typeName), "telemetry") &&
				!strings.Contains(strings.ToLower(typeName), "otlp") {
				return true
			}
			for _, element := range value.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if ok {
					report(typeName+"."+expressionName(field.Key), field.Value)
				}
			}
		}
		return true
	})
	return findings
}

func expressionName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		prefix := expressionName(value.X)
		if prefix == "" {
			return value.Sel.Name
		}
		return prefix + "." + value.Sel.Name
	default:
		return ""
	}
}

func sensitiveEndpointName(name string) bool {
	name = strings.ToLower(name)
	if !strings.Contains(name, "endpoint") {
		return false
	}
	for _, marker := range []string{"analytics", "beacon", "crash", "diagnostic", "otlp", "report", "telemetry", "usage"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}
