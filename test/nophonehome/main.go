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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type egressAPI struct {
	destination int
	implicit    bool
	request     bool
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

var httpClientEgressAPIs = map[string]egressAPI{
	"Get":      {destination: 0},
	"Head":     {destination: 0},
	"Post":     {destination: 0},
	"PostForm": {destination: 0},
	"Do":       {destination: 0, request: true},
}

var netDialerEgressAPIs = map[string]egressAPI{
	"Dial":        {destination: 1},
	"DialContext": {destination: 2},
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
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc.New": {
		location:       "internal/telemetry/metrics.go:metricExporter",
		options:        "opts",
		endpointSource: "cfg.OTLPEndpoint",
	},
}

var reportingSDKMarkers = []string{
	"airbrake",
	"appcenter",
	"bugsnag",
	"crashlytics",
	"datadog",
	"honeycomb",
	"newrelic",
	"raygun",
	"rollbar",
	"sentry",
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
		var fileFindings []finding
		switch {
		case strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go"):
			fileFindings, err = scanGoFile(root, path)
		case isProductionScript(path):
			fileFindings, err = scanScriptFile(root, path)
		case isCommandFile(path):
			fileFindings, err = scanCommandFile(root, path)
		default:
			return nil
		}
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

func isProductionScript(path string) bool {
	normalized := strings.ToLower(filepath.ToSlash(path))
	extension := filepath.Ext(normalized)
	switch extension {
	case ".js", ".jsx", ".ts", ".tsx":
	default:
		return false
	}
	base := strings.TrimSuffix(filepath.Base(normalized), extension)
	if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".spec") {
		return false
	}
	for _, marker := range []string{"/test/", "/tests/", "/__tests__/"} {
		if strings.Contains(normalized, marker) {
			return false
		}
	}
	return true
}

func isCommandFile(path string) bool {
	normalized := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(normalized)
	switch filepath.Ext(base) {
	case ".sh", ".bash", ".zsh", ".ps1", ".psm1":
		return true
	case ".yml", ".yaml":
		return strings.Contains(normalized, "/.github/workflows/")
	case ".mk":
		return true
	default:
		return base == "makefile"
	}
}

func scanGoFile(root, path string) ([]finding, error) {
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
		switch value := declaration.(type) {
		case *ast.FuncDecl:
			if value.Body != nil {
				findings = append(
					findings,
					scanGoCallScope(files, parsed, rel, imports, fileBindings, value.Body, value)...,
				)
			}
		case *ast.GenDecl:
			for _, spec := range value.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, initializer := range values.Values {
					findings = append(
						findings,
						scanGoCallScope(files, parsed, rel, imports, fileBindings, initializer, nil)...,
					)
				}
			}
		}
	}
	findings = append(findings, telemetryEndpointDefaults(files, parsed, rel, fileBindings)...)
	return findings, nil
}

// localEgressWrappers names same-file functions whose bodies reach a monitored
// egress, process or reporting-SDK call. Callers pass literals to these exactly
// as they would to the sink itself, so a scan that only inspects selector calls
// misses `post("https://maintainer.invalid/usage")` whenever post's body calls
// http.Post.
//
// The closure is deliberately shallow — one level of "does this body contain a
// monitored call" — rather than a full call graph. For a guard that is the safe
// direction: a wrapper that merely forwards to another wrapper is not matched
// here, but nothing that is matched can be a false negative.
func localEgressWrappers(parsed *ast.File, imports map[string]string) map[string]bool {
	wrappers := map[string]bool{}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || function.Recv != nil {
			continue
		}
		reaches := false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if reaches {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, _, monitored := monitoredProcessCall(selector, imports); monitored {
				reaches = true
				return false
			}
			if _, monitored := monitoredReportingSDKCall(selector, imports); monitored {
				reaches = true
				return false
			}
			if pkg, isIdent := selector.X.(*ast.Ident); isIdent {
				if _, monitored := egressAPIs[imports[pkg.Name]][selector.Sel.Name]; monitored {
					reaches = true
					return false
				}
			}
			return true
		})
		if reaches {
			wrappers[function.Name.Name] = true
		}
	}
	return wrappers
}

func scanGoCallScope(
	files *token.FileSet,
	parsed *ast.File,
	rel string,
	imports map[string]string,
	fileBindings map[string]ast.Expr,
	scope ast.Node,
	function *ast.FuncDecl,
) []finding {
	var findings []finding
	implicitCalls := make(map[string][]*ast.CallExpr)
	wrappers := localEgressWrappers(parsed, imports)
	ast.Inspect(scope, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			// A call through a bare identifier is not a monitored sink itself,
			// but it may be a local wrapper that reaches one. Handing a
			// prohibited literal to such a wrapper is the same egress as
			// calling the sink directly, so reject it here rather than
			// requiring the scan to model the wrapper's body.
			if callee, isIdent := call.Fun.(*ast.Ident); isIdent && wrappers[callee.Name] {
				bindings := bindingsAtScope(scope, function, call, fileBindings)
				position := files.Position(call.Pos())
				for _, argument := range call.Args {
					destination, found := processURLDestination(argument, bindings, true)
					if !found {
						continue
					}
					findings = append(findings, finding{
						path: rel, line: position.Line, column: position.Column,
						message: fmt.Sprintf(
							"hardcoded network destination %q passed to %s, which reaches a monitored egress call",
							destination, callee.Name),
					})
				}
			}
			return true
		}
		bindings := bindingsAtScope(scope, function, call, fileBindings)
		if firstArgument, callName, monitored := monitoredProcessCall(selector, imports); monitored {
			position := files.Position(call.Pos())
			arguments, inspectPartial := processURLArguments(call.Args[firstArgument:], bindings)
			for _, argument := range arguments {
				destination, found := processURLDestination(argument, bindings, inspectPartial)
				if !found {
					destination, found = conditionalEgressDestination(
						scope,
						function,
						call,
						argument,
						fileBindings,
						imports,
						egressAPI{},
						true,
						false,
					)
				}
				if found {
					findings = append(findings, finding{
						path: rel, line: position.Line, column: position.Column,
						message: fmt.Sprintf("hardcoded network destination %q passed to %s", destination, callName),
					})
				}
			}
			return true
		}
		if callName, monitored := monitoredReportingSDKCall(selector, imports); monitored {
			position := files.Position(call.Pos())
			destination, _, hardcoded := reportingSDKConfiguration(call.Args, bindings)
			switch {
			case hardcoded:
				findings = append(findings, finding{
					path: rel, line: position.Line, column: position.Column,
					message: fmt.Sprintf("hardcoded reporting SDK destination %q passed to %s", destination, callName),
				})
			default:
				findings = append(findings, finding{
					path: rel, line: position.Line, column: position.Column,
					message: "non-OTLP reporting SDK initialization via " + callName,
				})
			}
			return true
		}
		api, callName, monitored := monitoredEgressCall(
			parsed,
			function,
			scope,
			call,
			selector,
			imports,
			bindings,
		)
		if !monitored {
			return true
		}
		position := files.Position(call.Pos())
		if api.implicit {
			implicitCalls[callName] = append(implicitCalls[callName], call)
			return true
		}
		if api.destination >= len(call.Args) {
			return true
		}
		destination, found := egressDestination(api, call.Args[api.destination], bindings, imports)
		if !found {
			destination, found = conditionalEgressDestination(
				scope,
				function,
				call,
				call.Args[api.destination],
				fileBindings,
				imports,
				api,
				false,
				strings.Contains(callName, "go.opentelemetry.io/otel/exporters/"),
			)
		}
		if found {
			findings = append(findings, finding{
				path: rel, line: position.Line, column: position.Column,
				message: fmt.Sprintf("hardcoded network destination %q passed to %s", destination, callName),
			})
		}
		return true
	})
	location := ""
	if function != nil {
		location = rel + ":" + function.Name.Name
	}
	for callName, calls := range implicitCalls {
		approval, approved := approvedImplicitEgress[callName]
		approved = approved &&
			function != nil &&
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
	return findings
}

func monitoredReportingSDKCall(selector *ast.SelectorExpr, imports map[string]string) (string, bool) {
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	importPath := imports[pkg.Name]
	if !reportingSDKImport(importPath) {
		return "", false
	}
	switch strings.ToLower(selector.Sel.Name) {
	case "configure", "init", "initialize", "new", "newclient", "setup", "start":
		return importPath + "." + selector.Sel.Name, true
	default:
		return "", false
	}
}

func reportingSDKImport(importPath string) bool {
	lower := strings.ToLower(importPath)
	for _, marker := range reportingSDKMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func reportingSDKConfiguration(
	expressions []ast.Expr,
	bindings map[string]ast.Expr,
) (destination string, configured, hardcoded bool) {
	for _, expression := range expressions {
		destination, expressionConfigured, expressionHardcoded := reportingSDKExpression(
			expression,
			bindings,
			false,
			nil,
		)
		if expressionHardcoded {
			return destination, true, true
		}
		configured = configured || expressionConfigured
	}
	return "", configured, false
}

func reportingSDKExpression(
	expression ast.Expr,
	bindings map[string]ast.Expr,
	destinationField bool,
	seen map[string]bool,
) (destination string, configured, hardcoded bool) {
	switch value := expression.(type) {
	case *ast.Ident:
		if seen == nil {
			seen = make(map[string]bool)
		}
		if seen[value.Name] {
			return "", destinationField, false
		}
		binding, ok := bindings[value.Name]
		if !ok {
			return "", destinationField, false
		}
		seen[value.Name] = true
		destination, configured, hardcoded = reportingSDKExpression(binding, bindings, destinationField, seen)
		delete(seen, value.Name)
		return destination, configured, hardcoded
	case *ast.CompositeLit:
		for _, element := range value.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			destination, fieldConfigured, fieldHardcoded := reportingSDKExpression(
				field.Value,
				bindings,
				reportingSDKDestinationField(expressionName(field.Key)),
				seen,
			)
			if fieldHardcoded {
				return destination, true, true
			}
			configured = configured || fieldConfigured
		}
		return "", configured, false
	case *ast.CallExpr:
		callDestinationField := destinationField || reportingSDKDestinationField(expressionName(value.Fun))
		if callDestinationField && userConfiguredLookupCall(value.Fun) {
			return "", true, false
		}
		for _, argument := range value.Args {
			destination, argumentConfigured, argumentHardcoded := reportingSDKExpression(
				argument,
				bindings,
				callDestinationField,
				seen,
			)
			if argumentHardcoded {
				return destination, true, true
			}
			configured = configured || argumentConfigured
		}
		return "", configured, false
	case *ast.ParenExpr:
		return reportingSDKExpression(value.X, bindings, destinationField, seen)
	}
	if destinationField {
		var destination string
		ast.Inspect(expression, func(node ast.Node) bool {
			if destination != "" {
				return false
			}
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(literal.Value)
			if err == nil && strings.TrimSpace(text) != "" {
				destination = text
			}
			return true
		})
		if destination != "" {
			return destination, true, true
		}
		return "", true, false
	}
	if text, ok := staticString(expression, bindings, nil); ok {
		if destination, found := hardcodedLeadingURLPrefix(text); found {
			return destination, true, true
		}
	}
	return "", false, false
}

func userConfiguredLookupCall(expression ast.Expr) bool {
	name := strings.ToLower(expressionName(expression))
	return strings.HasSuffix(name, ".getenv") || strings.HasSuffix(name, ".lookupenv")
}

func reportingSDKDestinationField(name string) bool {
	name = strings.ToLower(name)
	for _, marker := range []string{"collector", "dsn", "endpoint", "host", "server", "url"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func monitoredProcessCall(selector *ast.SelectorExpr, imports map[string]string) (int, string, bool) {
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || imports[pkg.Name] != "os/exec" {
		return 0, "", false
	}
	switch selector.Sel.Name {
	case "Command":
		return 0, "os/exec.Command", true
	case "CommandContext":
		return 1, "os/exec.CommandContext", true
	default:
		return 0, "", false
	}
}

func processURLArguments(arguments []ast.Expr, bindings map[string]ast.Expr) ([]ast.Expr, bool) {
	if len(arguments) < 2 {
		return nil, false
	}
	command, ok := staticString(arguments[0], bindings, nil)
	if !ok {
		return arguments[1:], false
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(command)), ".exe")
	switch name {
	case "curl", "wget", "http", "httpie", "powershell", "pwsh",
		"sh", "bash", "dash", "ksh", "zsh", "cmd":
		return arguments[1:], true
	default:
		return arguments[1:], false
	}
}

func processURLDestination(
	expression ast.Expr,
	bindings map[string]ast.Expr,
	inspectPartial bool,
) (string, bool) {
	if text, ok := staticString(expression, bindings, nil); ok {
		return hardcodedURLPrefix(text)
	}
	if !inspectPartial {
		return "", false
	}
	text, ok := partialString(expression, bindings, nil)
	if !ok {
		return "", false
	}
	return hardcodedURLPrefix(text)
}

func egressDestination(
	api egressAPI,
	expression ast.Expr,
	bindings map[string]ast.Expr,
	imports map[string]string,
) (string, bool) {
	if api.request {
		return requestURLDestination(expression, bindings, imports, nil)
	}
	if destination, ok := staticString(expression, bindings, nil); ok && strings.TrimSpace(destination) != "" {
		return destination, true
	}
	if destination, ok := staticURLDestination(expression, bindings, nil); ok {
		return destination, true
	}
	return nestedProhibitedURL(expression, bindings, nil)
}

func nestedProhibitedURL(
	expression ast.Expr,
	bindings map[string]ast.Expr,
	seen map[string]bool,
) (string, bool) {
	if destination, ok := staticURLDestination(expression, bindings, nil); ok &&
		(isReportingDestination(destination) || isMaintainerOwnedDestination(destination)) {
		return destination, true
	}
	switch value := expression.(type) {
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
		defer delete(seen, value.Name)
		return nestedProhibitedURL(binding, bindings, seen)
	case *ast.SelectorExpr:
		if seen == nil {
			seen = make(map[string]bool)
		}
		name := expressionName(value)
		if seen[name] {
			return "", false
		}
		seen[name] = true
		defer delete(seen, name)
		if binding, ok := bindings[name]; ok {
			return nestedProhibitedURL(binding, bindings, seen)
		}
		binding, ok := boundCompositeField(value.X, value.Sel.Name, bindings, make(map[string]bool))
		if !ok {
			return "", false
		}
		return nestedProhibitedURL(binding, bindings, seen)
	case *ast.CallExpr:
		for _, argument := range value.Args {
			if destination, ok := nestedProhibitedURL(argument, bindings, seen); ok {
				return destination, true
			}
		}
	case *ast.BinaryExpr:
		if destination, ok := nestedProhibitedURL(value.X, bindings, seen); ok {
			return destination, true
		}
		return nestedProhibitedURL(value.Y, bindings, seen)
	case *ast.ParenExpr:
		return nestedProhibitedURL(value.X, bindings, seen)
	case *ast.UnaryExpr:
		return nestedProhibitedURL(value.X, bindings, seen)
	}
	return "", false
}

func requestURLDestination(
	expression ast.Expr,
	bindings map[string]ast.Expr,
	imports map[string]string,
	seen map[string]bool,
) (string, bool) {
	switch value := expression.(type) {
	case *ast.Ident:
		if seen == nil {
			seen = make(map[string]bool)
		}
		if seen[value.Name] {
			return "", false
		}
		seen[value.Name] = true
		defer delete(seen, value.Name)
		if assignedURL, ok := bindings[value.Name+".URL"]; ok {
			return requestURLValueDestination(assignedURL, bindings, imports, seen)
		}
		if binding, ok := bindings[value.Name]; ok {
			return requestURLDestination(binding, bindings, imports, seen)
		}
	case *ast.UnaryExpr:
		return requestURLDestination(value.X, bindings, imports, seen)
	case *ast.ParenExpr:
		return requestURLDestination(value.X, bindings, imports, seen)
	case *ast.CallExpr:
		selector, ok := value.Fun.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || imports[pkg.Name] != "net/http" {
			return "", false
		}
		switch selector.Sel.Name {
		case "NewRequest":
			if len(value.Args) > 1 {
				return egressDestination(egressAPI{destination: 1}, value.Args[1], bindings, imports)
			}
		case "NewRequestWithContext":
			if len(value.Args) > 2 {
				return egressDestination(egressAPI{destination: 2}, value.Args[2], bindings, imports)
			}
		}
	case *ast.CompositeLit:
		for _, element := range value.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if !ok || expressionName(field.Key) != "URL" {
				continue
			}
			return requestURLValueDestination(field.Value, bindings, imports, seen)
		}
	}
	return "", false
}

func requestURLValueDestination(
	expression ast.Expr,
	bindings map[string]ast.Expr,
	imports map[string]string,
	seen map[string]bool,
) (string, bool) {
	switch value := expression.(type) {
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
		destination, found := requestURLValueDestination(binding, bindings, imports, seen)
		delete(seen, value.Name)
		return destination, found
	case *ast.UnaryExpr:
		return requestURLValueDestination(value.X, bindings, imports, seen)
	case *ast.ParenExpr:
		return requestURLValueDestination(value.X, bindings, imports, seen)
	case *ast.CallExpr:
		selector, ok := value.Fun.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || imports[pkg.Name] != "net/url" || len(value.Args) == 0 {
			return "", false
		}
		switch selector.Sel.Name {
		case "Parse", "ParseRequestURI":
			return egressDestination(egressAPI{}, value.Args[0], bindings, imports)
		}
	case *ast.CompositeLit:
		if !isURLType(value.Type, imports) {
			return "", false
		}
		var scheme, host string
		for _, element := range value.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			text, static := staticString(field.Value, bindings, nil)
			if !static {
				continue
			}
			switch expressionName(field.Key) {
			case "Scheme":
				scheme = text
			case "Host":
				host = text
			}
		}
		return hardcodedLeadingURLPrefix(scheme + "://" + host)
	}
	return "", false
}

func isURLType(expression ast.Expr, imports map[string]string) bool {
	switch value := expression.(type) {
	case *ast.StarExpr:
		return isURLType(value.X, imports)
	case *ast.ParenExpr:
		return isURLType(value.X, imports)
	case *ast.SelectorExpr:
		pkg, ok := value.X.(*ast.Ident)
		return ok && imports[pkg.Name] == "net/url" && value.Sel.Name == "URL"
	default:
		return false
	}
}

func monitoredEgressCall(
	parsed *ast.File,
	function *ast.FuncDecl,
	scope ast.Node,
	call *ast.CallExpr,
	selector *ast.SelectorExpr,
	imports map[string]string,
	bindings map[string]ast.Expr,
) (egressAPI, string, bool) {
	if pkg, ok := selector.X.(*ast.Ident); ok {
		importPath := imports[pkg.Name]
		if api, monitored := egressAPIs[importPath][selector.Sel.Name]; monitored {
			return api, importPath + "." + selector.Sel.Name, true
		}
	}
	if api, monitored := netDialerEgressAPIs[selector.Sel.Name]; monitored {
		typedDialers := netDialerIdentifiersAt(parsed, function, scope, call, imports)
		if isNetDialerReceiver(selector.X, imports, bindings, typedDialers, nil) {
			return api, "net.Dialer." + selector.Sel.Name, true
		}
	}
	api, monitored := httpClientEgressAPIs[selector.Sel.Name]
	if !monitored {
		return egressAPI{}, "", false
	}
	typedClients := httpClientIdentifiersAt(parsed, function, scope, call, imports)
	if !isHTTPClientReceiver(selector.X, imports, bindings, typedClients, nil) {
		return egressAPI{}, "", false
	}
	return api, "net/http.Client." + selector.Sel.Name, true
}

func netDialerIdentifiersAt(
	parsed *ast.File,
	function *ast.FuncDecl,
	scope ast.Node,
	target ast.Node,
	imports map[string]string,
) map[string]bool {
	identifiers := make(map[string]bool)
	structFields := netDialerStructFields(parsed, imports)
	for typeName, fields := range structFields {
		for fieldName := range fields {
			if fieldName == "" {
				identifiers[typeName] = true
			} else {
				identifiers[typeName+"."+fieldName] = true
			}
		}
	}
	addFields := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, name := range field.Names {
				addNetDialerIdentifiers(identifiers, name.Name, field.Type, imports, structFields)
			}
		}
	}
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
			for _, name := range values.Names {
				addNetDialerIdentifiers(identifiers, name.Name, values.Type, imports, structFields)
			}
		}
	}
	if function != nil {
		addFields(function.Recv)
		addFields(function.Type.Params)
	}
	ast.Inspect(scope, func(node ast.Node) bool {
		if node == nil || node.Pos() >= target.Pos() {
			return false
		}
		switch value := node.(type) {
		case *ast.FuncLit:
			if containsNode(value.Body, target) {
				addFields(value.Type.Params)
			}
			return true
		case *ast.ValueSpec:
			for _, name := range value.Names {
				addNetDialerIdentifiers(identifiers, name.Name, value.Type, imports, structFields)
			}
			return false
		default:
			return true
		}
	})
	return identifiers
}

func netDialerStructFields(parsed *ast.File, imports map[string]string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	embeddedTypes := make(map[string][]string)
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range structType.Fields.List {
				if len(field.Names) == 0 && !isNetDialerType(field.Type, imports) {
					if embedded := namedTypeName(field.Type); embedded != "" {
						embeddedTypes[typeSpec.Name.Name] = append(embeddedTypes[typeSpec.Name.Name], embedded)
					}
					continue
				}
				if !isNetDialerType(field.Type, imports) {
					continue
				}
				if result[typeSpec.Name.Name] == nil {
					result[typeSpec.Name.Name] = make(map[string]bool)
				}
				if len(field.Names) == 0 {
					result[typeSpec.Name.Name][""] = true
					continue
				}
				for _, name := range field.Names {
					result[typeSpec.Name.Name][name.Name] = true
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for typeName, embedded := range embeddedTypes {
			for _, embeddedType := range embedded {
				if !result[embeddedType][""] || result[typeName][""] {
					continue
				}
				if result[typeName] == nil {
					result[typeName] = make(map[string]bool)
				}
				result[typeName][""] = true
				result[typeName][embeddedType] = true
				changed = true
			}
		}
	}
	return result
}

func addNetDialerIdentifiers(
	identifiers map[string]bool,
	name string,
	expression ast.Expr,
	imports map[string]string,
	structFields map[string]map[string]bool,
) {
	if isNetDialerType(expression, imports) {
		identifiers[name] = true
		return
	}
	for fieldName := range structFields[namedTypeName(expression)] {
		if fieldName == "" {
			identifiers[name] = true
		} else {
			identifiers[name+"."+fieldName] = true
		}
	}
}

func isNetDialerType(expression ast.Expr, imports map[string]string) bool {
	switch value := expression.(type) {
	case *ast.StarExpr:
		return isNetDialerType(value.X, imports)
	case *ast.ParenExpr:
		return isNetDialerType(value.X, imports)
	case *ast.SelectorExpr:
		pkg, ok := value.X.(*ast.Ident)
		return ok && imports[pkg.Name] == "net" && value.Sel.Name == "Dialer"
	default:
		return false
	}
}

func isNetDialerReceiver(
	expression ast.Expr,
	imports map[string]string,
	bindings map[string]ast.Expr,
	typedDialers map[string]bool,
	seen map[string]bool,
) bool {
	if typedDialers[expressionName(expression)] {
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
		resolved := isNetDialerReceiver(binding, imports, bindings, typedDialers, seen)
		delete(seen, value.Name)
		return resolved
	case *ast.SelectorExpr:
		object, ok := value.X.(*ast.Ident)
		if !ok {
			return false
		}
		binding, bound := bindings[object.Name]
		if !bound {
			return false
		}
		return typedDialers[boundTypeName(binding)+"."+value.Sel.Name]
	case *ast.CompositeLit:
		return isNetDialerType(value.Type, imports) || typedDialers[namedTypeName(value.Type)]
	case *ast.UnaryExpr:
		return value.Op == token.AND &&
			isNetDialerReceiver(value.X, imports, bindings, typedDialers, seen)
	case *ast.ParenExpr:
		return isNetDialerReceiver(value.X, imports, bindings, typedDialers, seen)
	case *ast.CallExpr:
		identifier, ok := value.Fun.(*ast.Ident)
		return ok && identifier.Name == "new" && len(value.Args) == 1 &&
			isNetDialerType(value.Args[0], imports)
	default:
		return false
	}
}

func httpClientIdentifiersAt(
	parsed *ast.File,
	function *ast.FuncDecl,
	scope ast.Node,
	target ast.Node,
	imports map[string]string,
) map[string]bool {
	identifiers := make(map[string]bool)
	structFields := httpClientStructFields(parsed, imports)
	for typeName, fields := range structFields {
		for fieldName := range fields {
			if fieldName == "" {
				identifiers[typeName] = true
			} else {
				identifiers[typeName+"."+fieldName] = true
			}
		}
	}
	addFields := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, name := range field.Names {
				addHTTPClientIdentifiers(identifiers, name.Name, field.Type, imports, structFields)
			}
		}
	}
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
			for _, name := range values.Names {
				addHTTPClientIdentifiers(identifiers, name.Name, values.Type, imports, structFields)
			}
		}
	}
	if function != nil {
		addFields(function.Recv)
		addFields(function.Type.Params)
	}
	ast.Inspect(scope, func(node ast.Node) bool {
		if node == nil || node.Pos() >= target.Pos() {
			return false
		}
		switch value := node.(type) {
		case *ast.FuncLit:
			if containsNode(value.Body, target) {
				addFields(value.Type.Params)
			}
			return true
		case *ast.ValueSpec:
			for _, name := range value.Names {
				addHTTPClientIdentifiers(identifiers, name.Name, value.Type, imports, structFields)
			}
			return false
		default:
			return true
		}
	})
	return identifiers
}

func httpClientStructFields(parsed *ast.File, imports map[string]string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	embeddedTypes := make(map[string][]string)
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range structType.Fields.List {
				if len(field.Names) == 0 && !isHTTPClientType(field.Type, imports) {
					if embedded := namedTypeName(field.Type); embedded != "" {
						embeddedTypes[typeSpec.Name.Name] = append(embeddedTypes[typeSpec.Name.Name], embedded)
					}
					continue
				}
				if !isHTTPClientType(field.Type, imports) {
					continue
				}
				if result[typeSpec.Name.Name] == nil {
					result[typeSpec.Name.Name] = make(map[string]bool)
				}
				if len(field.Names) == 0 {
					result[typeSpec.Name.Name][""] = true
					continue
				}
				for _, name := range field.Names {
					result[typeSpec.Name.Name][name.Name] = true
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for typeName, embedded := range embeddedTypes {
			for _, embeddedType := range embedded {
				if !result[embeddedType][""] || result[typeName][""] {
					continue
				}
				if result[typeName] == nil {
					result[typeName] = make(map[string]bool)
				}
				result[typeName][""] = true
				result[typeName][embeddedType] = true
				changed = true
			}
		}
	}
	return result
}

func addHTTPClientIdentifiers(
	identifiers map[string]bool,
	name string,
	expression ast.Expr,
	imports map[string]string,
	structFields map[string]map[string]bool,
) {
	if isHTTPClientType(expression, imports) {
		identifiers[name] = true
		return
	}
	for fieldName := range structFields[namedTypeName(expression)] {
		if fieldName == "" {
			identifiers[name] = true
		} else {
			identifiers[name+"."+fieldName] = true
		}
	}
}

func namedTypeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return namedTypeName(value.X)
	case *ast.ParenExpr:
		return namedTypeName(value.X)
	case *ast.IndexExpr:
		return namedTypeName(value.X)
	case *ast.IndexListExpr:
		return namedTypeName(value.X)
	default:
		return ""
	}
}

func isHTTPClientType(expression ast.Expr, imports map[string]string) bool {
	switch value := expression.(type) {
	case *ast.StarExpr:
		return isHTTPClientType(value.X, imports)
	case *ast.ParenExpr:
		return isHTTPClientType(value.X, imports)
	case *ast.SelectorExpr:
		pkg, ok := value.X.(*ast.Ident)
		return ok && imports[pkg.Name] == "net/http" && value.Sel.Name == "Client"
	default:
		return false
	}
}

func isHTTPClientReceiver(
	expression ast.Expr,
	imports map[string]string,
	bindings map[string]ast.Expr,
	typedClients map[string]bool,
	seen map[string]bool,
) bool {
	if typedClients[expressionName(expression)] {
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
		resolved := isHTTPClientReceiver(binding, imports, bindings, typedClients, seen)
		delete(seen, value.Name)
		return resolved
	case *ast.SelectorExpr:
		pkg, ok := value.X.(*ast.Ident)
		if ok && imports[pkg.Name] == "net/http" && value.Sel.Name == "DefaultClient" {
			return true
		}
		if !ok {
			return false
		}
		binding, bound := bindings[pkg.Name]
		if !bound {
			return false
		}
		return typedClients[boundTypeName(binding)+"."+value.Sel.Name]
	case *ast.CompositeLit:
		return isHTTPClientType(value.Type, imports) || typedClients[namedTypeName(value.Type)]
	case *ast.UnaryExpr:
		return value.Op == token.AND &&
			isHTTPClientReceiver(value.X, imports, bindings, typedClients, seen)
	case *ast.ParenExpr:
		return isHTTPClientReceiver(value.X, imports, bindings, typedClients, seen)
	case *ast.CallExpr:
		identifier, ok := value.Fun.(*ast.Ident)
		return ok && identifier.Name == "new" && len(value.Args) == 1 &&
			isHTTPClientType(value.Args[0], imports)
	default:
		return false
	}
}

func boundTypeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.CompositeLit:
		return namedTypeName(value.Type)
	case *ast.UnaryExpr:
		return boundTypeName(value.X)
	case *ast.ParenExpr:
		return boundTypeName(value.X)
	default:
		return ""
	}
}

func monitoredImports(files *token.FileSet, parsed *ast.File, rel string) (map[string]string, []finding) {
	imports := make(map[string]string)
	var findings []finding
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if _, monitored := egressAPIs[importPath]; !monitored &&
			importPath != "os/exec" && importPath != "net/url" &&
			!reportingSDKImport(importPath) {
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
		name := importPackageName(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		} else if reportingSDKImport(importPath) {
			name = strings.TrimSuffix(name, "-go")
		}
		imports[name] = importPath
	}
	return imports, findings
}

// isMajorVersionElement reports whether element is the /vN suffix Go modules
// append for major versions 2 and above.
func isMajorVersionElement(element string) bool {
	if len(element) < 2 || element[0] != 'v' {
		return false
	}
	for _, digit := range element[1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// importPackageName is the identifier an unaliased import binds, which is not
// always the last path element. A module at major version 2 or above carries a
// /vN suffix that is part of the module path and not part of the package name:
// "github.com/bugsnag/bugsnag-go/v2" binds "bugsnag", not "v2". Keying the
// import table on the raw basename therefore hid every versioned reporting SDK
// from the scan, because the recorded name never matched the identifier the
// call site actually used.
func importPackageName(importPath string) string {
	name := filepath.Base(importPath)
	if !isMajorVersionElement(name) {
		return name
	}
	trimmed := filepath.Base(strings.TrimSuffix(importPath, "/"+name))
	if trimmed == "" || trimmed == "." || trimmed == string(filepath.Separator) {
		return name
	}
	return trimmed
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
	var scope ast.Node
	if function != nil {
		scope = function.Body
	}
	return bindingsAtScope(scope, function, target, fileBindings)
}

func bindingsAtScope(
	scope ast.Node,
	function *ast.FuncDecl,
	target ast.Node,
	fileBindings map[string]ast.Expr,
) map[string]ast.Expr {
	bindings := make(map[string]ast.Expr, len(fileBindings))
	for name, expression := range fileBindings {
		bindings[name] = expression
	}
	if function != nil {
		forgetFieldNames(bindings, function.Recv)
		forgetFieldNames(bindings, function.Type.Params)
		forgetFieldNames(bindings, function.Type.Results)
	}

	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	if scope != nil {
		ast.Inspect(scope, func(node ast.Node) bool {
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
	}
	for node := target; node != nil; node = parents[node] {
		if literal, ok := node.(*ast.FuncLit); ok {
			forgetFieldNames(bindings, literal.Type.Params)
			forgetFieldNames(bindings, literal.Type.Results)
		}
	}

	callScopes := make(map[*ast.BlockStmt]bool)
	for node := target; node != nil; node = parents[node] {
		if block, ok := node.(*ast.BlockStmt); ok {
			callScopes[block] = true
		}
	}

	if scope != nil {
		ast.Inspect(scope, func(node ast.Node) bool {
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
					name := expressionName(left)
					if name == "" || name == "_" {
						continue
					}
					if i < len(value.Rhs) {
						bindings[name] = value.Rhs[i]
					} else {
						delete(bindings, name)
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
	}
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

func conditionalEgressDestination(
	scope ast.Node,
	function *ast.FuncDecl,
	target ast.Node,
	expression ast.Expr,
	fileBindings map[string]ast.Expr,
	imports map[string]string,
	api egressAPI,
	requireURL bool,
	rejectAny bool,
) (string, bool) {
	bindings := bindingsAtScope(scope, function, target, fileBindings)
	names := referencedBindingNames(expression, bindings, target.Pos())
	if len(names) == 0 {
		return "", false
	}

	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	if scope != nil {
		ast.Inspect(scope, func(node ast.Node) bool {
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
	}
	callScopes := make(map[*ast.BlockStmt]bool)
	for node := target; node != nil; node = parents[node] {
		if block, ok := node.(*ast.BlockStmt); ok {
			callScopes[block] = true
		}
	}

	lastDominatingAssignment := make(map[string]token.Pos)
	if scope != nil {
		ast.Inspect(scope, func(node ast.Node) bool {
			if node == nil || node.Pos() >= target.Pos() {
				return false
			}
			block := enclosingBlock(node, parents)
			if block == nil || !callScopes[block] || node.End() >= target.Pos() {
				return true
			}
			switch value := node.(type) {
			case *ast.AssignStmt:
				for _, left := range value.Lhs {
					name := expressionName(left)
					if value.Pos() < names[name] {
						lastDominatingAssignment[name] = value.Pos()
					}
				}
				return false
			case *ast.ValueSpec:
				for _, name := range value.Names {
					if value.Pos() < names[name.Name] {
						lastDominatingAssignment[name.Name] = value.Pos()
					}
				}
				return false
			default:
				return true
			}
		})
	}

	var destination string
	if scope != nil {
		ast.Inspect(scope, func(node ast.Node) bool {
			if destination != "" || node == nil || node.Pos() >= target.Pos() {
				return false
			}
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || assignment.Tok != token.ASSIGN || assignment.End() >= target.Pos() {
				return true
			}
			block := enclosingBlock(assignment, parents)
			if block == nil || callScopes[block] {
				return true
			}
			for index, left := range assignment.Lhs {
				name := expressionName(left)
				if assignment.Pos() >= names[name] || assignment.Pos() <= lastDominatingAssignment[name] ||
					index >= len(assignment.Rhs) {
					continue
				}
				assignmentBindings := bindingsAtScope(scope, function, assignment, fileBindings)
				var found bool
				if requireURL {
					destination, found = processURLDestination(assignment.Rhs[index], assignmentBindings, true)
				} else {
					destination, found = egressDestination(api, assignment.Rhs[index], assignmentBindings, imports)
				}
				if found && (rejectAny ||
					isReportingDestination(destination) ||
					isMaintainerOwnedDestination(destination)) {
					return false
				}
				destination = ""
			}
			return false
		})
	}
	return destination, destination != ""
}

func referencedBindingNames(
	expression ast.Expr,
	bindings map[string]ast.Expr,
	before token.Pos,
) map[string]token.Pos {
	names := make(map[string]token.Pos)
	var collect func(ast.Expr, token.Pos)
	collect = func(current ast.Expr, currentBefore token.Pos) {
		ast.Inspect(current, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			name := identifier.Name
			if names[name] >= currentBefore {
				return false
			}
			names[name] = currentBefore
			binding, bound := bindings[name]
			if bound {
				bindingBefore := binding.Pos()
				if bindingBefore <= 0 || bindingBefore > currentBefore {
					bindingBefore = currentBefore
				}
				collect(binding, bindingBefore)
			}
			return false
		})
	}
	collect(expression, before)
	return names
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
	if !approvedSourceUnmodifiedBeforeCall(function, call, approval.endpointSource) {
		return false
	}
	importPath := strings.TrimSuffix(callName, ".New")
	state, found := implicitConfigurationBeforeCall(
		function.Body.List,
		implicitConfiguration{reaches: true},
		call,
		func(assignment *ast.AssignStmt, configured bool) bool {
			return applyImplicitOptionsAssignment(
				assignment,
				configured,
				function,
				imports,
				fileBindings,
				importPath,
				approval,
			)
		},
	)
	return found && state.configured
}

func approvedSourceUnmodifiedBeforeCall(function *ast.FuncDecl, call *ast.CallExpr, source string) bool {
	state, found := implicitConfigurationBeforeCall(
		function.Body.List,
		implicitConfiguration{configured: true, reaches: true},
		call,
		func(assignment *ast.AssignStmt, unmodified bool) bool {
			return unmodified && !assignmentMutatesSource(assignment, source)
		},
	)
	return found && state.configured
}

func assignmentMutatesSource(assignment *ast.AssignStmt, source string) bool {
	for _, left := range assignment.Lhs {
		name := expressionName(left)
		if name == source ||
			strings.HasPrefix(source, name+".") ||
			strings.HasPrefix(name, source+".") {
			return true
		}
	}
	return false
}

type implicitConfiguration struct {
	configured bool
	reaches    bool
}

func implicitConfigurationBeforeCall(
	statements []ast.Stmt,
	state implicitConfiguration,
	call *ast.CallExpr,
	applyAssignment func(*ast.AssignStmt, bool) bool,
) (implicitConfiguration, bool) {
	for _, statement := range statements {
		if !state.reaches {
			return state, false
		}
		if containsNode(statement, call) {
			return implicitConfigurationInsideStatement(statement, state, call, applyAssignment)
		}
		state = implicitConfigurationAfterStatement(statement, state, applyAssignment)
	}
	return state, false
}

func implicitConfigurationInsideStatement(
	statement ast.Stmt,
	state implicitConfiguration,
	call *ast.CallExpr,
	applyAssignment func(*ast.AssignStmt, bool) bool,
) (implicitConfiguration, bool) {
	switch value := statement.(type) {
	case *ast.BlockStmt:
		return implicitConfigurationBeforeCall(value.List, state, call, applyAssignment)
	case *ast.IfStmt:
		if containsNode(value.Body, call) {
			return implicitConfigurationBeforeCall(value.Body.List, state, call, applyAssignment)
		}
		if value.Else != nil && containsNode(value.Else, call) {
			return implicitConfigurationInsideStatement(value.Else, state, call, applyAssignment)
		}
	case *ast.SwitchStmt:
		for _, clause := range value.Body.List {
			if containsNode(clause, call) {
				return implicitConfigurationInsideStatement(clause, state, call, applyAssignment)
			}
		}
	case *ast.TypeSwitchStmt:
		for _, clause := range value.Body.List {
			if containsNode(clause, call) {
				return implicitConfigurationInsideStatement(clause, state, call, applyAssignment)
			}
		}
	case *ast.SelectStmt:
		for _, clause := range value.Body.List {
			if containsNode(clause, call) {
				return implicitConfigurationInsideStatement(clause, state, call, applyAssignment)
			}
		}
	case *ast.CaseClause:
		return implicitConfigurationBeforeCall(value.Body, state, call, applyAssignment)
	case *ast.CommClause:
		return implicitConfigurationBeforeCall(value.Body, state, call, applyAssignment)
	case *ast.ForStmt:
		if value.Init != nil {
			state = implicitConfigurationAfterStatement(value.Init, state, applyAssignment)
		}
		if containsNode(value.Body, call) {
			return implicitConfigurationBeforeCall(value.Body.List, state, call, applyAssignment)
		}
	case *ast.RangeStmt:
		if containsNode(value.Body, call) {
			return implicitConfigurationBeforeCall(value.Body.List, state, call, applyAssignment)
		}
	case *ast.LabeledStmt:
		return implicitConfigurationInsideStatement(value.Stmt, state, call, applyAssignment)
	}
	return state, true
}

func implicitConfigurationAfterStatement(
	statement ast.Stmt,
	state implicitConfiguration,
	applyAssignment func(*ast.AssignStmt, bool) bool,
) implicitConfiguration {
	if !state.reaches {
		return state
	}
	switch value := statement.(type) {
	case *ast.AssignStmt:
		state.configured = applyAssignment(value, state.configured)
	case *ast.BlockStmt:
		return implicitConfigurationAfterStatements(value.List, state, applyAssignment)
	case *ast.IfStmt:
		thenState := implicitConfigurationAfterStatements(value.Body.List, state, applyAssignment)
		elseState := state
		if value.Else != nil {
			elseState = implicitConfigurationAfterStatement(value.Else, state, applyAssignment)
		}
		return mergeImplicitConfigurations(thenState, elseState)
	case *ast.ReturnStmt:
		state.reaches = false
	}
	return state
}

func implicitConfigurationAfterStatements(
	statements []ast.Stmt,
	state implicitConfiguration,
	applyAssignment func(*ast.AssignStmt, bool) bool,
) implicitConfiguration {
	for _, statement := range statements {
		state = implicitConfigurationAfterStatement(statement, state, applyAssignment)
		if !state.reaches {
			break
		}
	}
	return state
}

func mergeImplicitConfigurations(left, right implicitConfiguration) implicitConfiguration {
	switch {
	case !left.reaches:
		return right
	case !right.reaches:
		return left
	default:
		return implicitConfiguration{configured: left.configured && right.configured, reaches: true}
	}
}

func applyImplicitOptionsAssignment(
	assignment *ast.AssignStmt,
	configured bool,
	function *ast.FuncDecl,
	imports map[string]string,
	fileBindings map[string]ast.Expr,
	importPath string,
	approval implicitEgressApproval,
) bool {
	for index, left := range assignment.Lhs {
		if expressionName(left) != approval.options || index >= len(assignment.Rhs) {
			continue
		}
		appendCall, ok := assignment.Rhs[index].(*ast.CallExpr)
		if !ok || expressionName(appendCall.Fun) != "append" || len(appendCall.Args) < 2 ||
			expressionName(appendCall.Args[0]) != approval.options {
			configured = false
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
			if exclusivelyDerivedFrom(optionCall.Args[api.destination], bindings, approval.endpointSource, nil) {
				configured = true
			}
		}
	}
	return configured
}

func containsNode(outer, inner ast.Node) bool {
	return outer != nil && inner != nil && outer.Pos() <= inner.Pos() && outer.End() >= inner.End()
}

func exclusivelyDerivedFrom(expression ast.Expr, bindings map[string]ast.Expr, source string, seen map[string]bool) bool {
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
		derived := exclusivelyDerivedFrom(binding, bindings, source, seen)
		delete(seen, value.Name)
		return derived
	case *ast.CallExpr:
		if len(value.Args) == 0 {
			return false
		}
		for _, argument := range value.Args {
			if !exclusivelyDerivedFrom(argument, bindings, source, seen) {
				return false
			}
		}
		return true
	case *ast.ParenExpr:
		return exclusivelyDerivedFrom(value.X, bindings, source, seen)
	case *ast.BinaryExpr:
		return exclusivelyDerivedFrom(value.X, bindings, source, seen) &&
			exclusivelyDerivedFrom(value.Y, bindings, source, seen)
	case *ast.UnaryExpr:
		return exclusivelyDerivedFrom(value.X, bindings, source, seen)
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
	case *ast.SelectorExpr:
		if seen == nil {
			seen = make(map[string]bool)
		}
		name := expressionName(value)
		if seen[name] {
			return "", false
		}
		seen[name] = true
		defer delete(seen, name)
		if binding, ok := bindings[name]; ok {
			return staticString(binding, bindings, seen)
		}
		binding, ok := boundCompositeField(value.X, value.Sel.Name, bindings, make(map[string]bool))
		if !ok {
			return "", false
		}
		return staticString(binding, bindings, seen)
	case *ast.CallExpr:
		if expressionName(value.Fun) != "fmt.Sprintf" || len(value.Args) == 0 {
			return "", false
		}
		format, ok := staticString(value.Args[0], bindings, seen)
		if !ok {
			return "", false
		}
		arguments := make([]any, 0, len(value.Args)-1)
		for _, argument := range value.Args[1:] {
			text, static := staticString(argument, bindings, seen)
			if !static {
				return "", false
			}
			arguments = append(arguments, text)
		}
		return fmt.Sprintf(format, arguments...), true
	default:
		return "", false
	}
}

func boundCompositeField(
	expression ast.Expr,
	fieldName string,
	bindings map[string]ast.Expr,
	seen map[string]bool,
) (ast.Expr, bool) {
	switch value := expression.(type) {
	case *ast.Ident:
		if seen[value.Name] {
			return nil, false
		}
		binding, ok := bindings[value.Name]
		if !ok {
			return nil, false
		}
		seen[value.Name] = true
		defer delete(seen, value.Name)
		return boundCompositeField(binding, fieldName, bindings, seen)
	case *ast.SelectorExpr:
		name := expressionName(value)
		if seen[name] {
			return nil, false
		}
		seen[name] = true
		defer delete(seen, name)
		if binding, ok := bindings[name]; ok {
			return boundCompositeField(binding, fieldName, bindings, seen)
		}
		binding, ok := boundCompositeField(value.X, value.Sel.Name, bindings, seen)
		if !ok {
			return nil, false
		}
		return boundCompositeField(binding, fieldName, bindings, seen)
	case *ast.CompositeLit:
		for _, element := range value.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if ok && expressionName(field.Key) == fieldName {
				return field.Value, true
			}
		}
	case *ast.ParenExpr:
		return boundCompositeField(value.X, fieldName, bindings, seen)
	case *ast.UnaryExpr:
		return boundCompositeField(value.X, fieldName, bindings, seen)
	case *ast.StarExpr:
		return boundCompositeField(value.X, fieldName, bindings, seen)
	}
	return nil, false
}

func staticURLDestination(expression ast.Expr, bindings map[string]ast.Expr, seen map[string]bool) (string, bool) {
	text, ok := partialString(expression, bindings, seen)
	if !ok {
		return "", false
	}
	return hardcodedLeadingURLPrefix(text)
}

func partialString(expression ast.Expr, bindings map[string]ast.Expr, seen map[string]bool) (string, bool) {
	if text, ok := staticString(expression, bindings, nil); ok {
		return text, true
	}
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "${dynamic}", true
		}
		text, err := strconv.Unquote(value.Value)
		return text, err == nil
	case *ast.Ident:
		if seen == nil {
			seen = make(map[string]bool)
		}
		if seen[value.Name] {
			return "${dynamic}", true
		}
		binding, ok := bindings[value.Name]
		if !ok {
			return "${dynamic}", true
		}
		seen[value.Name] = true
		text, resolved := partialString(binding, bindings, seen)
		delete(seen, value.Name)
		return text, resolved
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "${dynamic}", true
		}
		left, leftOK := partialString(value.X, bindings, seen)
		right, rightOK := partialString(value.Y, bindings, seen)
		return left + right, leftOK && rightOK
	case *ast.ParenExpr:
		return partialString(value.X, bindings, seen)
	case *ast.UnaryExpr:
		return partialString(value.X, bindings, seen)
	case *ast.CallExpr:
		if expressionName(value.Fun) != "fmt.Sprintf" || len(value.Args) == 0 {
			return "${dynamic}", true
		}
		format, ok := staticString(value.Args[0], bindings, nil)
		if !ok {
			return "${dynamic}", true
		}
		arguments := make([]any, 0, len(value.Args)-1)
		for _, argument := range value.Args[1:] {
			text, resolved := partialString(argument, bindings, seen)
			if !resolved {
				return "", false
			}
			arguments = append(arguments, text)
		}
		return fmt.Sprintf(format, arguments...), true
	default:
		return "${dynamic}", true
	}
}

func hardcodedLeadingURLPrefix(text string) (string, bool) {
	text = strings.TrimLeft(text, " \t\r\n")
	lower := strings.ToLower(text)
	for _, scheme := range []string{"http://", "https://", "ws://", "wss://", "grpc://"} {
		if strings.HasPrefix(lower, scheme) {
			return hardcodedURLPrefix(text)
		}
	}
	return "", false
}

func hardcodedURLPrefix(text string) (string, bool) {
	lower := strings.ToLower(text)
	start := -1
	for _, scheme := range []string{"http://", "https://", "ws://", "wss://", "grpc://"} {
		if index := strings.Index(lower, scheme); index >= 0 && (start < 0 || index < start) {
			start = index
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(text)
	for index, character := range text[start:] {
		if strings.ContainsRune(" \t\r\n\"'`", character) {
			end = start + index
			break
		}
	}
	if interpolation := strings.Index(text[start:end], "${"); interpolation >= 0 {
		end = start + interpolation
	}
	candidate := strings.TrimRight(text[start:end], ",;)]}")
	schemeEnd := strings.Index(candidate, "://") + len("://")
	originEnd := len(candidate)
	if separator := strings.IndexAny(candidate[schemeEnd:], "/?#"); separator >= 0 {
		originEnd = schemeEnd + separator
	}
	parsed, err := url.Parse(candidate[:originEnd])
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	return candidate, true
}

func telemetryEndpointDefaults(files *token.FileSet, parsed *ast.File, rel string, fileBindings map[string]ast.Expr) []finding {
	var findings []finding
	report := func(name string, expression ast.Expr) {
		if !sensitiveEndpointName(name) {
			return
		}
		bindings := fileBindings
		if function := containingFunction(parsed, expression); function != nil {
			bindings = bindingsAt(function, expression, fileBindings)
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

func containingFunction(parsed *ast.File, node ast.Node) *ast.FuncDecl {
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Body != nil &&
			function.Body.Pos() <= node.Pos() && node.End() <= function.Body.End() {
			return function
		}
	}
	return nil
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

func scanCommandFile(root, path string) ([]finding, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, fmt.Errorf("make %s relative to %s: %w", path, root, err)
	}
	rel = filepath.ToSlash(rel)

	lines := strings.Split(string(source), "\n")
	var findings []finding
	var command strings.Builder
	bindings := make(map[string]string)
	yamlBindings := strings.HasSuffix(strings.ToLower(path), ".yml") ||
		strings.HasSuffix(strings.ToLower(path), ".yaml")
	commandLine := 0
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if command.Len() == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			commandLine = index + 1
		}
		if command.Len() != 0 {
			command.WriteByte(' ')
		}
		command.WriteString(trimmed)
		if strings.HasSuffix(trimmed, `\`) || strings.HasSuffix(trimmed, "`") {
			continue
		}

		text := command.String()
		command.Reset()
		updateCommandBindings(text, bindings, yamlBindings)
		text = expandCommandBindings(text, bindings)
		name := commandEgressName(text)
		if name == "" {
			continue
		}
		destination, ok := reportingURLInCommand(text)
		if !ok {
			continue
		}
		findings = append(findings, finding{
			path: rel, line: commandLine, column: 1,
			message: fmt.Sprintf("hardcoded telemetry/reporting destination %q passed to command %s", destination, name),
		})
	}
	return findings, nil
}

func updateCommandBindings(text string, bindings map[string]string, yaml bool) {
	name, value, ok := commandBinding(text, yaml)
	if !ok {
		return
	}
	value = expandCommandBindings(strings.TrimSpace(value), bindings)
	if value == "" {
		return
	}
	if len(value) >= 2 {
		switch {
		case value[0] == '"' && value[len(value)-1] == '"':
			value = value[1 : len(value)-1]
		case value[0] == '\'' && value[len(value)-1] == '\'':
			value = value[1 : len(value)-1]
		}
	}
	bindings[name] = value
}

func commandBinding(text string, yaml bool) (string, string, bool) {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"- ", "@", "export "} {
		if strings.HasPrefix(strings.ToLower(text), prefix) {
			text = strings.TrimSpace(text[len(prefix):])
		}
	}
	for _, operator := range []string{":=", "?=", "+=", "="} {
		if index := strings.Index(text, operator); index >= 0 {
			name := normalizeCommandBindingName(text[:index])
			if validCommandBindingName(name) {
				return name, text[index+len(operator):], true
			}
		}
	}
	if index := strings.Index(text, ":"); yaml && index >= 0 {
		name := normalizeCommandBindingName(text[:index])
		return name, text[index+1:], validCommandBindingName(name)
	}
	return "", "", false
}

func normalizeCommandBindingName(name string) string {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "export ") {
		name = strings.TrimSpace(name[len("export "):])
		lower = strings.ToLower(name)
	}
	if strings.HasPrefix(lower, "$env:") {
		name = name[len("$env:"):]
	} else {
		name = strings.TrimPrefix(name, "$")
	}
	return strings.Trim(strings.TrimSpace(name), `"'`)
}

func validCommandBindingName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if character == '_' || character == '-' || character == '.' ||
			character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func expandCommandBindings(text string, bindings map[string]string) string {
	return expandCommandBindingsSeen(text, bindings, make(map[string]bool))
}

func expandCommandBindingsSeen(text string, bindings map[string]string, seen map[string]bool) string {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return len(names[i]) > len(names[j])
	})
	for _, name := range names {
		if seen[name] || !commandBindingReferenced(text, name) {
			continue
		}
		seen[name] = true
		value := expandCommandBindingsSeen(bindings[name], bindings, seen)
		delete(seen, name)
		for _, pattern := range []string{
			"${" + name + "}",
			"$(" + name + ")",
			"$env:" + name,
			"${env:" + name + "}",
			"${{ env." + name + " }}",
		} {
			text = replaceCommandBinding(text, pattern, value, false)
		}
		text = replaceCommandBinding(text, "$"+name, value, true)
	}
	return text
}

func commandBindingReferenced(text, name string) bool {
	for _, pattern := range []string{
		"${" + name + "}",
		"$(" + name + ")",
		"$env:" + name,
		"${env:" + name + "}",
		"${{ env." + name + " }}",
	} {
		if commandBindingPatternPresent(text, pattern, false) {
			return true
		}
	}
	return commandBindingPatternPresent(text, "$"+name, true)
}

func commandBindingPatternPresent(text, pattern string, requireBoundary bool) bool {
	for {
		index := strings.Index(text, pattern)
		if index < 0 {
			return false
		}
		end := index + len(pattern)
		if (index == 0 || text[index-1] != '$') &&
			(!requireBoundary || end == len(text) || !commandVariableByte(text[end])) {
			return true
		}
		text = text[end:]
	}
}

func replaceCommandBinding(text, pattern, value string, requireBoundary bool) string {
	var result strings.Builder
	for {
		index := strings.Index(text, pattern)
		if index < 0 {
			result.WriteString(text)
			return result.String()
		}
		end := index + len(pattern)
		if index > 0 && text[index-1] == '$' ||
			requireBoundary && end < len(text) && commandVariableByte(text[end]) {
			result.WriteString(text[:end])
			text = text[end:]
			continue
		}
		result.WriteString(text[:index])
		result.WriteString(value)
		text = text[end:]
	}
}

func commandVariableByte(value byte) bool {
	return value == '_' ||
		value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func commandEgressName(text string) string {
	lower := strings.ToLower(text)
	for _, name := range []string{
		"invoke-restmethod",
		"invoke-webrequest",
		"start-bitstransfer",
		"curl",
		"http",
		"httpie",
		"iwr",
		"wget",
	} {
		for start := 0; ; {
			index := strings.Index(lower[start:], name)
			if index < 0 {
				break
			}
			index += start
			beforeOK := index == 0 || !commandIdentifierByte(lower[index-1])
			end := index + len(name)
			afterOK := end == len(lower) || !commandIdentifierByte(lower[end])
			if beforeOK && afterOK {
				return name
			}
			start = index + len(name)
		}
	}
	return ""
}

func commandIdentifierByte(value byte) bool {
	return value == '_' || value == '-' ||
		value >= 'a' && value <= 'z' ||
		value >= '0' && value <= '9'
}

func reportingURLInCommand(text string) (string, bool) {
	lower := strings.ToLower(text)
	for offset := 0; offset < len(text); {
		start := -1
		for _, scheme := range []string{"http://", "https://", "ws://", "wss://", "grpc://"} {
			if index := strings.Index(lower[offset:], scheme); index >= 0 &&
				(start < 0 || index < start) {
				start = index
			}
		}
		if start < 0 {
			return "", false
		}
		start += offset
		destination, ok := hardcodedURLPrefix(text[start:])
		if ok && (isReportingDestination(destination) || isMaintainerOwnedDestination(destination)) {
			return destination, true
		}
		offset = start + 1
	}
	return "", false
}

func isReportingDestination(destination string) bool {
	words := strings.FieldsFunc(strings.ToLower(destination), func(character rune) bool {
		return character < 'a' || character > 'z'
	})
	for _, word := range words {
		switch word {
		case "analytics", "beacon", "collect", "crash", "diagnostic",
			"maintainer", "metrics", "report", "telemetry", "usage":
			return true
		}
	}
	return false
}

func isMaintainerOwnedDestination(destination string) bool {
	parsed, err := url.Parse(destination)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	const owner = "agent-clubhouse"
	if host == owner || strings.HasPrefix(host, owner+".") || strings.HasSuffix(host, "."+owner) {
		return true
	}
	switch host {
	case "github.com", "raw.githubusercontent.com", "api.github.com":
		segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		return len(segments) != 0 && strings.EqualFold(segments[0], owner)
	default:
		return false
	}
}

type scriptTokenKind int

const (
	scriptPunctuation scriptTokenKind = iota
	scriptIdentifier
	scriptString
)

type scriptToken struct {
	kind         scriptTokenKind
	text         string
	line         int
	column       int
	staticString bool
}

type scriptScope struct {
	declared   map[string]bool
	bindings   map[string]string
	urls       map[string]string
	isFunction bool
}

func scanScriptFile(root, path string) ([]finding, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, fmt.Errorf("make %s relative to %s: %w", path, root, err)
	}
	rel = filepath.ToSlash(rel)
	tokens := tokenizeScript(source)
	xhrInstances := xmlHTTPRequestInstances(tokens)
	scopes := []*scriptScope{newScriptScope(true)}
	pendingFunction := -1
	var findings []finding
	for index := 0; index < len(tokens); index++ {
		current := tokens[index]
		if current.kind == scriptIdentifier && current.text == "function" {
			pendingFunction = index
		}
		switch current.text {
		case "{":
			scope := newScriptScope(false)
			if pendingFunction >= 0 && scriptFunctionBody(tokens, pendingFunction, index) {
				scope.isFunction = true
				declareScriptFunctionParameters(scope, tokens, pendingFunction, index)
				pendingFunction = -1
			}
			scopes = append(scopes, scope)
			continue
		case "}":
			if len(scopes) > 1 {
				scopes = scopes[:len(scopes)-1]
			}
			continue
		}
		if current.kind == scriptIdentifier &&
			(current.text == "const" || current.text == "let" || current.text == "var") {
			if index+1 >= len(tokens) || tokens[index+1].kind != scriptIdentifier {
				continue
			}
			name := tokens[index+1].text
			scope := scriptDeclarationScope(scopes, current.text)
			setScriptBinding(scope, name, "", false, "", false)
			_, valueIndex, ok := scriptDeclaration(tokens, index)
			if ok {
				bindings, urls := visibleScriptBindings(scopes)
				findings = bindScriptValue(
					findings,
					rel,
					tokens,
					name,
					valueIndex,
					scope,
					bindings,
					urls,
				)
			}
		}
		if valueIndex, ok := scriptAssignment(tokens, index); ok {
			bindings, urls := visibleScriptBindings(scopes)
			scope := scriptAssignmentScope(scopes, current.text)
			findings = bindScriptValue(
				findings,
				rel,
				tokens,
				current.text,
				valueIndex,
				scope,
				bindings,
				urls,
			)
		}
		bindings, urlBindings := visibleScriptBindings(scopes)
		destinationArgument, monitored := scriptEgressCall(tokens, index, xhrInstances)
		if !monitored {
			continue
		}
		argument := scriptCallArgument(tokens, index+1, destinationArgument)
		if argument < 0 {
			continue
		}
		destination, next, static := scriptStaticString(tokens, argument, bindings, nil)
		if !static || !scriptCallArgumentBoundary(tokens, next) || strings.TrimSpace(destination) == "" {
			end := scriptCallArgumentEnd(tokens, argument)
			var found bool
			destination, found = scriptURLDestination(tokens, argument, end, bindings, urlBindings)
			if !found {
				continue
			}
		}
		findings = append(findings, finding{
			path: rel, line: current.line, column: current.column,
			message: fmt.Sprintf("hardcoded network destination %q passed to JavaScript %s", destination, current.text),
		})
	}
	return findings, nil
}

func newScriptScope(function bool) *scriptScope {
	return &scriptScope{
		declared:   make(map[string]bool),
		bindings:   make(map[string]string),
		urls:       make(map[string]string),
		isFunction: function,
	}
}

func visibleScriptBindings(scopes []*scriptScope) (map[string]string, map[string]string) {
	bindings := make(map[string]string)
	urls := make(map[string]string)
	for _, scope := range scopes {
		for name := range scope.declared {
			delete(bindings, name)
			delete(urls, name)
		}
		for name, value := range scope.bindings {
			bindings[name] = value
		}
		for name, value := range scope.urls {
			urls[name] = value
		}
	}
	return bindings, urls
}

func scriptDeclarationScope(scopes []*scriptScope, declaration string) *scriptScope {
	if declaration != "var" {
		return scopes[len(scopes)-1]
	}
	for index := len(scopes) - 1; index >= 0; index-- {
		if scopes[index].isFunction {
			return scopes[index]
		}
	}
	return scopes[0]
}

func scriptAssignmentScope(scopes []*scriptScope, name string) *scriptScope {
	functionScope := 0
	for index := len(scopes) - 1; index >= 0; index-- {
		if scopes[index].isFunction {
			functionScope = index
			break
		}
	}
	for index := len(scopes) - 1; index >= 0; index-- {
		if !scopes[index].declared[name] {
			continue
		}
		if index < functionScope {
			return scopes[functionScope]
		}
		return scopes[index]
	}
	return scopes[functionScope]
}

func setScriptBinding(
	scope *scriptScope,
	name, value string,
	static bool,
	destination string,
	hasDestination bool,
) {
	scope.declared[name] = true
	delete(scope.bindings, name)
	delete(scope.urls, name)
	if static {
		scope.bindings[name] = value
	}
	if hasDestination {
		scope.urls[name] = destination
	}
}

func bindScriptValue(
	findings []finding,
	rel string,
	tokens []scriptToken,
	name string,
	valueIndex int,
	scope *scriptScope,
	bindings, urls map[string]string,
) []finding {
	value, next, static := scriptStaticString(tokens, valueIndex, bindings, nil)
	static = static && scriptExpressionBoundary(tokens, next)
	end := scriptExpressionEnd(tokens, valueIndex)
	destination, hasDestination := scriptURLDestination(tokens, valueIndex, end, bindings, urls)
	setScriptBinding(scope, name, value, static, destination, hasDestination)
	if static && sensitiveEndpointName(name) && strings.TrimSpace(value) != "" {
		findings = append(findings, finding{
			path: rel, line: tokens[valueIndex].line, column: tokens[valueIndex].column,
			message: fmt.Sprintf("default telemetry/reporting endpoint %q assigned to %s", value, name),
		})
	}
	return findings
}

func scriptFunctionBody(tokens []scriptToken, function, body int) bool {
	open := -1
	for index := function + 1; index < body; index++ {
		if tokens[index].text == "(" {
			open = index
			break
		}
	}
	if open < 0 {
		return false
	}
	close := matchingScriptDelimiter(tokens, open, len(tokens))
	return close >= 0 && close < body
}

func declareScriptFunctionParameters(scope *scriptScope, tokens []scriptToken, function, body int) {
	open := -1
	for index := function + 1; index < body; index++ {
		if tokens[index].text == "(" {
			open = index
			break
		}
	}
	if open < 0 {
		return
	}
	close := matchingScriptDelimiter(tokens, open, body)
	if close < 0 {
		return
	}
	expectName := true
	depth := 0
	for index := open + 1; index < close; index++ {
		switch tokens[index].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		case ",":
			if depth == 0 {
				expectName = true
			}
		default:
			if depth == 0 && expectName && tokens[index].kind == scriptIdentifier {
				expectName = false
				setScriptBinding(scope, tokens[index].text, "", false, "", false)
			}
		}
	}
}

func tokenizeScript(source []byte) []scriptToken {
	var tokens []scriptToken
	index, line, column := 0, 1, 1
	advance := func() byte {
		value := source[index]
		index++
		if value == '\n' {
			line++
			column = 1
		} else {
			column++
		}
		return value
	}
	for index < len(source) {
		current := source[index]
		if current == ' ' || current == '\t' || current == '\r' || current == '\n' {
			advance()
			continue
		}
		if current == '/' && index+1 < len(source) && source[index+1] == '/' {
			advance()
			advance()
			for index < len(source) && source[index] != '\n' {
				advance()
			}
			continue
		}
		if current == '/' && index+1 < len(source) && source[index+1] == '*' {
			advance()
			advance()
			for index < len(source) {
				if source[index] == '*' && index+1 < len(source) && source[index+1] == '/' {
					advance()
					advance()
					break
				}
				advance()
			}
			continue
		}
		if current == '/' && scriptRegexCanStart(tokens) {
			advance()
			inCharacterClass := false
		regex:
			for index < len(source) {
				character := advance()
				if character == '\\' && index < len(source) {
					advance()
					continue
				}
				if character == '\n' {
					break
				}
				switch character {
				case '[':
					inCharacterClass = true
				case ']':
					inCharacterClass = false
				case '/':
					if !inCharacterClass {
						for index < len(source) && scriptIdentifierPart(source[index]) {
							advance()
						}
						break regex
					}
				}
			}
			continue
		}
		startLine, startColumn := line, column
		if current == '"' || current == '\'' || current == '`' {
			quote := advance()
			var value strings.Builder
			static := true
			for index < len(source) {
				character := advance()
				if character == '\\' && index < len(source) {
					value.WriteByte(advance())
					continue
				}
				if character == quote {
					break
				}
				if quote == '`' && character == '$' && index < len(source) && source[index] == '{' {
					static = false
				}
				value.WriteByte(character)
			}
			tokens = append(tokens, scriptToken{
				kind: scriptString, text: value.String(), line: startLine, column: startColumn,
				staticString: static,
			})
			continue
		}
		if scriptIdentifierStart(current) {
			start := index
			for index < len(source) && scriptIdentifierPart(source[index]) {
				advance()
			}
			tokens = append(tokens, scriptToken{
				kind: scriptIdentifier, text: string(source[start:index]), line: startLine, column: startColumn,
			})
			continue
		}
		tokens = append(tokens, scriptToken{
			kind: scriptPunctuation, text: string(advance()), line: startLine, column: startColumn,
		})
	}
	return tokens
}

func scriptRegexCanStart(tokens []scriptToken) bool {
	if len(tokens) == 0 {
		return true
	}
	previous := tokens[len(tokens)-1]
	if previous.kind == scriptIdentifier {
		switch previous.text {
		case "await", "case", "delete", "do", "else", "in", "instanceof", "new",
			"of", "return", "throw", "typeof", "void", "yield":
			return true
		default:
			return false
		}
	}
	switch previous.text {
	case "(", "[", "{", "=", ",", ";", ":", "!", "?", "+", "-", "*", "%",
		"&", "|", "^", "~", "<", ">":
		return true
	default:
		return false
	}
}

func scriptIdentifierStart(value byte) bool {
	return value == '_' || value == '$' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func scriptIdentifierPart(value byte) bool {
	return scriptIdentifierStart(value) || value >= '0' && value <= '9'
}

func scriptDeclaration(tokens []scriptToken, declaration int) (int, int, bool) {
	name := declaration + 1
	if name >= len(tokens) || tokens[name].kind != scriptIdentifier {
		return 0, 0, false
	}
	for index := name + 1; index < len(tokens) && index <= name+8; index++ {
		switch tokens[index].text {
		case "=":
			return name, index + 1, index+1 < len(tokens)
		case ";", ",":
			return 0, 0, false
		}
	}
	return 0, 0, false
}

func scriptAssignment(tokens []scriptToken, identifier int) (int, bool) {
	if identifier+2 >= len(tokens) || tokens[identifier].kind != scriptIdentifier ||
		tokens[identifier+1].text != "=" {
		return 0, false
	}
	if tokens[identifier+2].text == "=" || tokens[identifier+2].text == ">" {
		return 0, false
	}
	if identifier > 0 {
		switch tokens[identifier-1].text {
		case ".", "const", "let", "var", "!", "<", ">", "=":
			return 0, false
		}
	}
	return identifier + 2, true
}

func scriptEgressCall(tokens []scriptToken, index int, xhrInstances map[string]bool) (int, bool) {
	if index+1 >= len(tokens) || tokens[index].kind != scriptIdentifier || tokens[index+1].text != "(" {
		return 0, false
	}
	switch tokens[index].text {
	case "fetch", "WebSocket", "EventSource":
		return 0, true
	case "sendBeacon":
		return 0, index >= 2 && tokens[index-1].text == "." && tokens[index-2].text == "navigator"
	case "get", "head", "post", "put", "patch", "delete", "request":
		return 0, index >= 2 && tokens[index-1].text == "." && tokens[index-2].text == "axios"
	case "open":
		// XMLHttpRequest.open(method, url): the destination is the SECOND
		// argument, unlike every other monitored sink here.
		return 1, xmlHTTPRequestReceiver(tokens, index, xhrInstances)
	default:
		return 0, false
	}
}

// xmlHTTPRequestReceiver reports whether the `.open(` at index is called on an
// XMLHttpRequest, either constructed inline (`new XMLHttpRequest().open(...)`)
// or through an identifier previously bound to one.
func xmlHTTPRequestReceiver(tokens []scriptToken, index int, xhrInstances map[string]bool) bool {
	if index < 2 || tokens[index-1].text != "." {
		return false
	}
	switch receiver := tokens[index-2]; {
	case receiver.kind == scriptIdentifier && xhrInstances[receiver.text]:
		// xhr.open(...) where xhr was bound to a construction.
		return true
	case receiver.text == ")":
		// new XMLHttpRequest().open(...) — walk back over the empty or
		// argument-bearing constructor call to its constructor identifier.
		depth := 0
		for cursor := index - 2; cursor >= 0; cursor-- {
			switch tokens[cursor].text {
			case ")":
				depth++
			case "(":
				depth--
				if depth == 0 {
					return cursor >= 1 && tokens[cursor-1].text == "XMLHttpRequest"
				}
			}
		}
	}
	return false
}

// xmlHTTPRequestInstances collects identifiers bound to an XMLHttpRequest
// construction anywhere in the file. It deliberately over-approximates by
// ignoring scope: for a guard, mistaking an unrelated identifier for an XHR
// can only cause a hardcoded maintainer URL to be reported, whereas missing a
// binding lets one through.
func xmlHTTPRequestInstances(tokens []scriptToken) map[string]bool {
	instances := map[string]bool{}
	for index := 0; index+2 < len(tokens); index++ {
		if tokens[index].text != "=" || tokens[index+1].text != "new" {
			continue
		}
		if tokens[index+2].text != "XMLHttpRequest" {
			continue
		}
		for cursor := index - 1; cursor >= 0; cursor-- {
			if tokens[cursor].kind == scriptIdentifier {
				instances[tokens[cursor].text] = true
				break
			}
			if tokens[cursor].text != ":" && tokens[cursor].text != "." {
				break
			}
		}
	}
	return instances
}

func scriptCallArgument(tokens []scriptToken, openParenthesis, destination int) int {
	if openParenthesis >= len(tokens) || tokens[openParenthesis].text != "(" {
		return -1
	}
	currentArgument := 0
	depth := 0
	for index := openParenthesis + 1; index < len(tokens); index++ {
		switch tokens[index].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			if depth == 0 {
				return -1
			}
			depth--
		case ",":
			if depth == 0 {
				currentArgument++
				continue
			}
		}
		if currentArgument == destination {
			return index
		}
	}
	return -1
}

func scriptCallArgumentEnd(tokens []scriptToken, start int) int {
	depth := 0
	for index := start; index < len(tokens); index++ {
		switch tokens[index].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			if depth == 0 {
				return index
			}
			depth--
		case ",":
			if depth == 0 {
				return index
			}
		}
	}
	return len(tokens)
}

func scriptExpressionEnd(tokens []scriptToken, start int) int {
	depth := 0
	for index := start; index < len(tokens); index++ {
		switch tokens[index].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			if depth == 0 {
				return index
			}
			depth--
		case ";", ",":
			if depth == 0 {
				return index
			}
		}
	}
	return len(tokens)
}

func scriptURLDestination(
	tokens []scriptToken,
	start, end int,
	bindings, urlBindings map[string]string,
) (string, bool) {
	for start < end && tokens[start].text == "(" {
		close := matchingScriptDelimiter(tokens, start, end)
		if close != end-1 {
			break
		}
		start++
		end--
	}
	depth := 0
	for index := start; index < end; index++ {
		switch tokens[index].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		case "+":
			if depth == 0 {
				if destination, ok := scriptURLDestination(tokens, start, index, bindings, urlBindings); ok {
					return destination, true
				}
				return scriptURLDestination(tokens, index+1, end, bindings, urlBindings)
			}
		}
	}
	if end-start != 1 {
		return "", false
	}
	current := tokens[start]
	switch current.kind {
	case scriptString:
		return scriptTemplateURLDestination(current.text, bindings)
	case scriptIdentifier:
		if destination := urlBindings[current.text]; destination != "" {
			return destination, true
		}
		if destination, ok := hardcodedURLPrefix(bindings[current.text]); ok {
			return destination, true
		}
	}
	return "", false
}

func matchingScriptDelimiter(tokens []scriptToken, open, end int) int {
	depth := 0
	for index := open; index < end; index++ {
		switch tokens[index].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func scriptTemplateURLDestination(text string, bindings map[string]string) (string, bool) {
	if destination, ok := hardcodedURLPrefix(text); ok {
		return destination, true
	}
	var resolved strings.Builder
	for {
		start := strings.Index(text, "${")
		if start < 0 {
			resolved.WriteString(text)
			break
		}
		resolved.WriteString(text[:start])
		text = text[start+2:]
		end := strings.IndexByte(text, '}')
		if end < 0 {
			return "", false
		}
		value, ok := scriptTemplateStaticValue(strings.TrimSpace(text[:end]), bindings)
		if !ok {
			return hardcodedURLPrefix(resolved.String())
		}
		resolved.WriteString(value)
		text = text[end+1:]
	}
	return hardcodedURLPrefix(resolved.String())
}

func scriptTemplateStaticValue(expression string, bindings map[string]string) (string, bool) {
	if value, ok := bindings[expression]; ok {
		return value, true
	}
	if len(expression) < 2 {
		return "", false
	}
	quote := expression[0]
	if (quote != '"' && quote != '\'' && quote != '`') || expression[len(expression)-1] != quote {
		return "", false
	}
	value := expression[1 : len(expression)-1]
	value = strings.ReplaceAll(value, `\`+string(quote), string(quote))
	value = strings.ReplaceAll(value, `\\`, `\`)
	return value, true
}

func scriptStaticString(
	tokens []scriptToken,
	index int,
	bindings map[string]string,
	seen map[string]bool,
) (string, int, bool) {
	value, next, ok := scriptStaticStringAtom(tokens, index, bindings, seen)
	if !ok {
		return "", index, false
	}
	for next < len(tokens) && tokens[next].text == "+" {
		right, after, rightOK := scriptStaticStringAtom(tokens, next+1, bindings, seen)
		if !rightOK {
			return "", index, false
		}
		value += right
		next = after
	}
	return value, next, true
}

func scriptStaticStringAtom(
	tokens []scriptToken,
	index int,
	bindings map[string]string,
	seen map[string]bool,
) (string, int, bool) {
	if index >= len(tokens) {
		return "", index, false
	}
	current := tokens[index]
	switch {
	case current.kind == scriptString:
		return current.text, index + 1, current.staticString
	case current.kind == scriptIdentifier:
		if seen == nil {
			seen = make(map[string]bool)
		}
		if seen[current.text] {
			return "", index, false
		}
		value, ok := bindings[current.text]
		if !ok {
			return "", index, false
		}
		seen[current.text] = true
		delete(seen, current.text)
		return value, index + 1, true
	case current.text == "(":
		value, next, ok := scriptStaticString(tokens, index+1, bindings, seen)
		if !ok || next >= len(tokens) || tokens[next].text != ")" {
			return "", index, false
		}
		return value, next + 1, true
	default:
		return "", index, false
	}
}

func scriptExpressionBoundary(tokens []scriptToken, index int) bool {
	if index >= len(tokens) {
		return true
	}
	switch tokens[index].text {
	case ";", ",", ")", "]", "}":
		return true
	default:
		return false
	}
}

func scriptCallArgumentBoundary(tokens []scriptToken, index int) bool {
	return index < len(tokens) && (tokens[index].text == "," || tokens[index].text == ")")
}
