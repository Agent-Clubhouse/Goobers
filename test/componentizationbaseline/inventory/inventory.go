package main

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var domainFiles = map[string]map[string]bool{
	"contention": {"contestedfiles.go": true},
	"docs-churn": {"docchurn.go": true},
	"pr-status":  {"reportprstatus.go": true},
}

type packageAnalysis struct {
	record    packageRecord
	tests     []testRecord
	symbols   map[string][]string
	factories map[string]bool
}

type typedPackage struct {
	info           *types.Info
	pkg            *types.Package
	functions      map[*types.Func]*ast.FuncDecl
	domains        map[types.Object][]string
	factories      map[types.Object]string
	commandDomains map[string][]string
	dispatchers    map[*types.Func]bool
}

type packageImporter struct {
	localPath string
	local     *types.Package
	fallback  types.Importer
}

func (i packageImporter) Import(path string) (*types.Package, error) {
	if path == i.localPath {
		return i.local, nil
	}
	return i.fallback.Import(path)
}

func buildInventory(module moduleMetadata, build goEnv, commit string, tags []string, target string, prod, test []goPackage) (inventory, error) {
	prodClosure := localClosure(module.Path, prod)
	testClosure := localClosure(module.Path, test)
	packages := mergeLocalPackages(module.Path, prod, test)
	exports := packageExports(prod, test)
	root := findRootPackage(target, prod)
	if root == nil {
		return inventory{}, fmt.Errorf("production go list did not contain target %q", target)
	}

	domains := make(map[string]*domainRecord, len(domainFiles))
	for name := range domainFiles {
		domains[name] = &domainRecord{
			Name: name, SourceFiles: []string{}, TestFiles: []string{}, ProductionSymbols: []string{},
			ExistingHelpers: []string{}, ReusableLocalImports: []string{},
		}
	}

	var records []packageRecord
	var tests []testRecord
	for _, pkg := range packages {
		analysis, err := analyzePackage(module, pkg, prodClosure, testClosure, exports, domains)
		if err != nil {
			return inventory{}, err
		}
		records = append(records, analysis.record)
		tests = append(tests, analysis.tests...)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ImportPath < records[j].ImportPath })
	sort.Slice(tests, func(i, j int) bool {
		if tests[i].Package != tests[j].Package {
			return tests[i].Package < tests[j].Package
		}
		if tests[i].File != tests[j].File {
			return tests[i].File < tests[j].File
		}
		return tests[i].Name < tests[j].Name
	})

	domainRecords := make([]domainRecord, 0, len(domains))
	for _, domain := range domains {
		domain.SourceFiles = sortedUnique(domain.SourceFiles)
		domain.TestFiles = sortedUnique(domain.TestFiles)
		domain.ProductionSymbols = sortedUnique(domain.ProductionSymbols)
		domain.ExistingHelpers = sortedUnique(domain.ExistingHelpers)
		domain.ReusableLocalImports = sortedUnique(domain.ReusableLocalImports)
		domainRecords = append(domainRecords, *domain)
	}
	sort.Slice(domainRecords, func(i, j int) bool { return domainRecords[i].Name < domainRecords[j].Name })

	rootTests, rootBenchmarks, rootExamples := countTests(tests, root.ImportPath)
	return inventory{
		SchemaVersion: schemaVersion,
		Source:        source{Commit: commit, ModulePath: module.Path},
		Build: buildContext{
			GoVersion: build.GoVersion, GOOS: build.GOOS, GOARCH: build.GOARCH,
			CGOEnabled: build.CGOEnabled, BuildTags: sortedUnique(tags),
		},
		Command: commandBoundary{
			Package: root.ImportPath, ProductionClosure: prodClosure, TestClosure: testClosure,
			TestOnlyDependencies: difference(testClosure, prodClosure),
			DirectImports:        sortedUnique(root.Imports), InternalTestImports: sortedUnique(root.TestImports),
			ExternalTestImports: sortedUnique(root.XTestImports),
			ProductionFiles:     len(root.GoFiles) + len(root.CgoFiles), InternalTestFiles: len(root.TestGoFiles),
			ExternalTestFiles: len(root.XTestGoFiles), Tests: rootTests, Benchmarks: rootBenchmarks, Examples: rootExamples,
		},
		Packages: records,
		Tests:    tests,
		Domains:  domainRecords,
		Omissions: []string{
			"dependency closures and ignored files describe only the recorded GOOS, GOARCH, CGO setting, and build tags",
			"other platform and build-tag combinations are not evaluated",
		},
	}, nil
}

func localClosure(modulePath string, packages []goPackage) []string {
	var paths []string
	for _, pkg := range packages {
		path := canonicalImportPath(pkg.ImportPath)
		if isLocalImport(modulePath, path) && !strings.HasSuffix(path, ".test") {
			paths = append(paths, path)
		}
	}
	return sortedUnique(paths)
}

func canonicalImportPath(path string) string {
	if before, _, found := strings.Cut(path, " ["); found {
		return before
	}
	return path
}

func isLocalImport(modulePath, importPath string) bool {
	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}

func mergeLocalPackages(modulePath string, prod, test []goPackage) []goPackage {
	merged := make(map[string]goPackage)
	for _, list := range [][]goPackage{test, prod} {
		for _, pkg := range list {
			path := canonicalImportPath(pkg.ImportPath)
			if !isLocalImport(modulePath, path) || pkg.Dir == "" || strings.Contains(pkg.ImportPath, " [") || strings.HasSuffix(path, ".test") {
				continue
			}
			pkg.ImportPath = path
			if existing, ok := merged[path]; !ok || len(pkg.TestGoFiles)+len(pkg.XTestGoFiles) > len(existing.TestGoFiles)+len(existing.XTestGoFiles) {
				merged[path] = pkg
			}
		}
	}
	out := make([]goPackage, 0, len(merged))
	for _, pkg := range merged {
		out = append(out, pkg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImportPath < out[j].ImportPath })
	return out
}

func findRootPackage(target string, packages []goPackage) *goPackage {
	for i := len(packages) - 1; i >= 0; i-- {
		pkg := &packages[i]
		if pkg.Name == "main" && (strings.HasSuffix(pkg.ImportPath, "/cmd/goobers") || target == pkg.ImportPath) {
			return pkg
		}
	}
	return nil
}

func packageExports(groups ...[]goPackage) map[string]string {
	exports := make(map[string]string)
	for _, group := range groups {
		for _, pkg := range group {
			if pkg.Export != "" {
				exports[canonicalImportPath(pkg.ImportPath)] = pkg.Export
			}
		}
	}
	return exports
}

func analyzePackage(module moduleMetadata, pkg goPackage, prodClosure, testClosure []string, exports map[string]string, domains map[string]*domainRecord) (packageAnalysis, error) {
	relDir, err := repositoryPath(module.Dir, pkg.Dir)
	if err != nil {
		return packageAnalysis{}, fmt.Errorf("normalize package %s: %w", pkg.ImportPath, err)
	}
	productionFiles := sortedFilePaths(relDir, append(append([]string{}, pkg.GoFiles...), pkg.CgoFiles...))
	internalTests := sortedFilePaths(relDir, pkg.TestGoFiles)
	externalTests := sortedFilePaths(relDir, pkg.XTestGoFiles)
	ignored := sortedFilePaths(relDir, pkg.IgnoredGoFiles)
	prodSet := sliceSet(prodClosure)
	testSet := sliceSet(testClosure)
	record := packageRecord{
		ImportPath: pkg.ImportPath, Path: relDir, ProductionFiles: productionFiles,
		InternalTestFiles: internalTests, ExternalTestFiles: externalTests, IgnoredFiles: ignored,
		DirectImports: sortedUnique(pkg.Imports), InternalTestImports: sortedUnique(pkg.TestImports),
		ExternalTestImports: sortedUnique(pkg.XTestImports), InProductionClosure: prodSet[pkg.ImportPath],
		InTestClosure: testSet[pkg.ImportPath], TestOnly: testSet[pkg.ImportPath] && !prodSet[pkg.ImportPath],
	}

	symbolDomains := make(map[string][]string)
	parsed := make(map[string]*ast.File)
	symbolFiles := make(map[string]string)
	fset := token.NewFileSet()
	for _, name := range append(append([]string{}, pkg.GoFiles...), pkg.CgoFiles...) {
		path := filepath.Join(pkg.Dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return packageAnalysis{}, fmt.Errorf("parse production file %s: %w", filepath.ToSlash(filepath.Join(relDir, name)), err)
		}
		parsed[name] = file
		for _, decl := range file.Decls {
			for _, symbol := range declarationNames(decl) {
				symbolFiles[symbol] = name
			}
		}
	}
	for name, file := range parsed {
		for domain, files := range domainFiles {
			if !files[name] {
				continue
			}
			repoPath := filepath.ToSlash(filepath.Join(relDir, name))
			domains[domain].SourceFiles = append(domains[domain].SourceFiles, repoPath)
			domains[domain].ReusableLocalImports = append(domains[domain].ReusableLocalImports, localImports(module.Path, file)...)
			for _, decl := range file.Decls {
				for _, symbol := range declarationNames(decl) {
					symbolDomains[symbol] = append(symbolDomains[symbol], domain)
					domains[domain].ProductionSymbols = append(domains[domain].ProductionSymbols, symbol)
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if !ok {
					return true
				}
				if owner, exists := symbolFiles[identifier.Name]; exists && owner != name {
					domains[domain].ExistingHelpers = append(
						domains[domain].ExistingHelpers,
						pkg.ImportPath+"."+identifier.Name,
					)
				}
				return true
			})
		}
	}

	internalFiles := make(map[string]*ast.File)
	for _, name := range pkg.TestGoFiles {
		path := filepath.Join(pkg.Dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return packageAnalysis{}, fmt.Errorf("parse test file %s: %w", filepath.ToSlash(filepath.Join(relDir, name)), err)
		}
		internalFiles[name] = file
	}
	typed := typePackage(fset, pkg.ImportPath, pkg.Name, parsed, internalFiles, nil, exports)
	factories := collectGlobalFactories(typed.pkg)
	for name, file := range parsed {
		for domain, files := range domainFiles {
			if !files[name] {
				continue
			}
			for _, decl := range file.Decls {
				for _, object := range declarationObjects(decl, typed.info) {
					typed.domains[object] = append(typed.domains[object], domain)
				}
			}
		}
	}
	for name := range factories {
		if object := typed.pkg.Scope().Lookup(name); object != nil {
			typed.factories[object] = name
		}
	}
	typed.collectCommandDispatch(parsed, internalFiles)

	var tests []testRecord
	for _, group := range []struct {
		files    []string
		external bool
	}{{pkg.TestGoFiles, false}, {pkg.XTestGoFiles, true}} {
		groupParsed := internalFiles
		groupTyped := typed
		if group.external {
			groupParsed = make(map[string]*ast.File)
			externalFSet := token.NewFileSet()
			for _, name := range group.files {
				path := filepath.Join(pkg.Dir, name)
				file, err := parser.ParseFile(externalFSet, path, nil, 0)
				if err != nil {
					return packageAnalysis{}, fmt.Errorf("parse test file %s: %w", filepath.ToSlash(filepath.Join(relDir, name)), err)
				}
				groupParsed[name] = file
			}
			groupTyped = typePackage(externalFSet, pkg.ImportPath+"_test", pkg.Name+"_test", nil, groupParsed, typed.pkg, exports)
			groupTyped.domains = typed.domains
			groupTyped.factories = typed.factories
		}
		for _, name := range group.files {
			file := groupParsed[name]
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil {
					continue
				}
				kind, ok := testKind(fn.Name.Name)
				if !ok {
					continue
				}
				record := analyzeTestFunction(pkg.ImportPath, filepath.ToSlash(filepath.Join(relDir, name)), group.external, kind, fn, groupTyped)
				tests = append(tests, record)
				for _, domain := range record.Domains {
					domains[domain].TestFiles = append(domains[domain].TestFiles, record.File)
				}
			}
		}
	}
	return packageAnalysis{record: record, tests: tests, symbols: symbolDomains, factories: factories}, nil
}

func typePackage(fset *token.FileSet, path, name string, production, test map[string]*ast.File, local *types.Package, exports map[string]string) typedPackage {
	info := &types.Info{
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	files := sortedASTFiles(production, test)
	fallback := types.Importer(importer.Default())
	if len(exports) > 0 {
		fallback = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			export, ok := exports[path]
			if !ok {
				return nil, fmt.Errorf("no export data for %s", path)
			}
			return os.Open(export)
		})
	}
	config := types.Config{Importer: fallback, Error: func(error) {}}
	if local != nil {
		config.Importer = packageImporter{localPath: local.Path(), local: local, fallback: config.Importer}
	}
	checked, _ := config.Check(path, fset, files, info)
	if checked == nil {
		checked = types.NewPackage(path, name)
	}
	functions := make(map[*types.Func]*ast.FuncDecl)
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if object, ok := info.Defs[fn.Name].(*types.Func); ok {
				functions[object] = fn
			}
		}
	}
	return typedPackage{
		info: info, pkg: checked, functions: functions,
		domains: make(map[types.Object][]string), factories: make(map[types.Object]string),
		commandDomains: make(map[string][]string), dispatchers: make(map[*types.Func]bool),
	}
}

func (typed *typedPackage) collectCommandDispatch(groups ...map[string]*ast.File) {
	files := sortedASTFiles(groups...)
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var names []string
			var domains []string
			for _, arg := range call.Args {
				switch value := arg.(type) {
				case *ast.BasicLit:
					if value.Kind == token.STRING {
						if name, err := strconv.Unquote(value.Value); err == nil {
							names = append(names, name)
						}
					}
				case *ast.Ident:
					domains = append(domains, typed.domains[typed.info.Uses[value]]...)
				}
			}
			for _, name := range names {
				typed.commandDomains[name] = append(typed.commandDomains[name], domains...)
			}
			return true
		})
	}

	registryObjects := make(map[types.Object]bool)
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok || !expressionsContainCommand(assign.Rhs, typed.commandDomains) {
				return true
			}
			for _, expression := range assign.Lhs {
				if ident, ok := expression.(*ast.Ident); ok {
					if object := typed.info.Uses[ident]; object != nil && object.Parent() == typed.pkg.Scope() {
						registryObjects[object] = true
					}
				}
			}
			return true
		})
	}

	calls := make(map[*types.Func][]*types.Func)
	for function, declaration := range typed.functions {
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			object := typed.info.Uses[ident]
			if registryObjects[object] {
				typed.dispatchers[function] = true
			}
			if called, ok := object.(*types.Func); ok && typed.functions[called] != nil {
				calls[function] = append(calls[function], called)
			}
			return true
		})
	}
	for changed := true; changed; {
		changed = false
		for function, called := range calls {
			if typed.dispatchers[function] {
				continue
			}
			for _, target := range called {
				if typed.dispatchers[target] {
					typed.dispatchers[function] = true
					changed = true
					break
				}
			}
		}
	}
}

func expressionsContainCommand(expressions []ast.Expr, commands map[string][]string) bool {
	found := false
	for _, expression := range expressions {
		ast.Inspect(expression, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err == nil && len(commands[value]) > 0 {
				found = true
				return false
			}
			return true
		})
	}
	return found
}

func sortedASTFiles(groups ...map[string]*ast.File) []*ast.File {
	byName := make(map[string]*ast.File)
	for _, group := range groups {
		for name, file := range group {
			byName[name] = file
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	files := make([]*ast.File, 0, len(names))
	for _, name := range names {
		files = append(files, byName[name])
	}
	return files
}

func declarationObjects(decl ast.Decl, info *types.Info) []types.Object {
	var names []*ast.Ident
	switch value := decl.(type) {
	case *ast.FuncDecl:
		names = append(names, value.Name)
	case *ast.GenDecl:
		for _, raw := range value.Specs {
			switch spec := raw.(type) {
			case *ast.TypeSpec:
				names = append(names, spec.Name)
			case *ast.ValueSpec:
				names = append(names, spec.Names...)
			}
		}
	}
	objects := make([]types.Object, 0, len(names))
	for _, name := range names {
		if object := info.Defs[name]; object != nil {
			objects = append(objects, object)
		}
	}
	return objects
}

func sortedFilePaths(dir string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, filepath.ToSlash(filepath.Join(dir, name)))
	}
	sort.Strings(out)
	return out
}

func declarationNames(decl ast.Decl) []string {
	switch value := decl.(type) {
	case *ast.FuncDecl:
		return []string{value.Name.Name}
	case *ast.GenDecl:
		var names []string
		for _, spec := range value.Specs {
			switch spec := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, spec.Name.Name)
			case *ast.ValueSpec:
				for _, name := range spec.Names {
					names = append(names, name.Name)
				}
			}
		}
		return names
	default:
		return nil
	}
}

func collectGlobalFactories(pkg *types.Package) map[string]bool {
	factories := make(map[string]bool)
	for _, name := range pkg.Scope().Names() {
		object, ok := pkg.Scope().Lookup(name).(*types.Var)
		if !ok {
			continue
		}
		if _, ok := object.Type().Underlying().(*types.Signature); ok {
			factories[name] = true
		}
	}
	return factories
}

func analyzeTestFunction(pkg, file string, external bool, kind string, fn *ast.FuncDecl, typed typedPackage) testRecord {
	var domainNames, environment, factoryNames []string
	seen := make(map[*types.Func]bool)
	var inspectFunction func(*ast.FuncDecl)
	inspectFunction = func(current *ast.FuncDecl) {
		object, _ := typed.info.Defs[current.Name].(*types.Func)
		if object != nil {
			if seen[object] {
				return
			}
			seen[object] = true
		}
		localValues := functionLocalValues(current, typed.info)
		ast.Inspect(current.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.CallExpr:
				ident, ok := value.Fun.(*ast.Ident)
				if !ok {
					break
				}
				called, _ := typed.info.Uses[ident].(*types.Func)
				if !typed.dispatchers[called] {
					break
				}
				for _, argument := range value.Args {
					for _, name := range expressionStrings(argument, typed.info, localValues, make(map[types.Object]bool)) {
						domainNames = append(domainNames, typed.commandDomains[name]...)
					}
				}
			case *ast.Ident:
				referenced := typed.info.Uses[value]
				domainNames = append(domainNames, typed.domains[referenced]...)
				if name := typed.factories[referenced]; name != "" {
					factoryNames = append(factoryNames, name)
				}
				if called, ok := referenced.(*types.Func); ok {
					if helper := typed.functions[called]; helper != nil {
						inspectFunction(helper)
					}
				}
			case *ast.SelectorExpr:
				if value.Sel.Name == "Setenv" || value.Sel.Name == "Getenv" || value.Sel.Name == "LookupEnv" || value.Sel.Name == "Environ" {
					environment = append(environment, value.Sel.Name)
				}
			}
			return true
		})
	}
	inspectFunction(fn)
	domainNames = sortedUnique(domainNames)
	return testRecord{
		Name: fn.Name.Name, Kind: kind, Package: pkg, File: file, External: external,
		Domains: domainNames, StraddlesDomains: len(domainNames) > 1,
		EnvironmentAccess: sortedUnique(environment), GlobalFactoryAccesses: sortedUnique(factoryNames),
	}
}

func functionLocalValues(fn *ast.FuncDecl, info *types.Info) map[types.Object][]ast.Expr {
	values := make(map[types.Object][]ast.Expr)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			if len(value.Lhs) != len(value.Rhs) {
				return true
			}
			for i, lhs := range value.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok {
					if object := info.Defs[ident]; object != nil {
						values[object] = append(values[object], value.Rhs[i])
					}
				}
			}
		case *ast.ValueSpec:
			if len(value.Names) != len(value.Values) {
				return true
			}
			for i, name := range value.Names {
				if object := info.Defs[name]; object != nil {
					values[object] = append(values[object], value.Values[i])
				}
			}
		case *ast.RangeStmt:
			if ident, ok := value.Value.(*ast.Ident); ok {
				if object := info.Defs[ident]; object != nil {
					values[object] = append(values[object], value.X)
				}
			}
		}
		return true
	})
	return values
}

func expressionStrings(expression ast.Expr, info *types.Info, localValues map[types.Object][]ast.Expr, seen map[types.Object]bool) []string {
	var values []string
	ast.Inspect(expression, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.BasicLit:
			if value.Kind == token.STRING {
				if text, err := strconv.Unquote(value.Value); err == nil {
					values = append(values, text)
				}
			}
		case *ast.Ident:
			object := info.Uses[value]
			if object == nil || seen[object] {
				return true
			}
			seen[object] = true
			for _, assigned := range localValues[object] {
				values = append(values, expressionStrings(assigned, info, localValues, seen)...)
			}
		}
		return true
	})
	return values
}

func testKind(name string) (string, bool) {
	for _, prefix := range []struct{ prefix, kind string }{{"Test", "test"}, {"Benchmark", "benchmark"}, {"Example", "example"}} {
		if strings.HasPrefix(name, prefix.prefix) {
			return prefix.kind, true
		}
	}
	return "", false
}

func localImports(modulePath string, file *ast.File) []string {
	var imports []string
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if isLocalImport(modulePath, path) {
			imports = append(imports, path)
		}
	}
	return sortedUnique(imports)
}

func sliceSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func countTests(tests []testRecord, pkg string) (int, int, int) {
	var testCount, benchmarkCount, exampleCount int
	for _, record := range tests {
		if record.Package != pkg {
			continue
		}
		switch record.Kind {
		case "test":
			testCount++
		case "benchmark":
			benchmarkCount++
		case "example":
			exampleCount++
		}
	}
	return testCount, benchmarkCount, exampleCount
}
