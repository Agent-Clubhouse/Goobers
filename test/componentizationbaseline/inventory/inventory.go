package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
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

func buildInventory(module moduleMetadata, build goEnv, commit string, tags []string, target string, prod, test []goPackage) (inventory, error) {
	prodClosure := localClosure(module.Path, prod)
	testClosure := localClosure(module.Path, test)
	packages := mergeLocalPackages(module.Path, prod, test)
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
		analysis, err := analyzePackage(module, pkg, prodClosure, testClosure, domains)
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

func analyzePackage(module moduleMetadata, pkg goPackage, prodClosure, testClosure []string, domains map[string]*domainRecord) (packageAnalysis, error) {
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
	factories := make(map[string]bool)
	parsed := make(map[string]*ast.File)
	symbolFiles := make(map[string]string)
	for _, name := range append(append([]string{}, pkg.GoFiles...), pkg.CgoFiles...) {
		path := filepath.Join(pkg.Dir, name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return packageAnalysis{}, fmt.Errorf("parse production file %s: %w", filepath.ToSlash(filepath.Join(relDir, name)), err)
		}
		parsed[name] = file
		for _, decl := range file.Decls {
			for _, symbol := range declarationNames(decl) {
				symbolFiles[symbol] = name
			}
		}
		collectGlobalFactories(file, factories)
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

	var tests []testRecord
	for _, group := range []struct {
		files    []string
		external bool
	}{{pkg.TestGoFiles, false}, {pkg.XTestGoFiles, true}} {
		for _, name := range group.files {
			path := filepath.Join(pkg.Dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return packageAnalysis{}, fmt.Errorf("parse test file %s: %w", filepath.ToSlash(filepath.Join(relDir, name)), err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil {
					continue
				}
				kind, ok := testKind(fn.Name.Name)
				if !ok {
					continue
				}
				record := analyzeTestFunction(pkg.ImportPath, filepath.ToSlash(filepath.Join(relDir, name)), group.external, kind, fn, symbolDomains, factories)
				tests = append(tests, record)
				for _, domain := range record.Domains {
					domains[domain].TestFiles = append(domains[domain].TestFiles, record.File)
				}
			}
		}
	}
	return packageAnalysis{record: record, tests: tests, symbols: symbolDomains, factories: factories}, nil
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

func collectGlobalFactories(file *ast.File, factories map[string]bool) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, raw := range gen.Specs {
			spec := raw.(*ast.ValueSpec)
			isFactory := false
			if _, ok := spec.Type.(*ast.FuncType); ok {
				isFactory = true
			}
			for _, value := range spec.Values {
				if _, ok := value.(*ast.FuncLit); ok {
					isFactory = true
				}
			}
			if isFactory {
				for _, name := range spec.Names {
					factories[name.Name] = true
				}
			}
		}
	}
}

func analyzeTestFunction(pkg, file string, external bool, kind string, fn *ast.FuncDecl, symbolDomains map[string][]string, factories map[string]bool) testRecord {
	var domainNames, environment, factoryNames []string
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			domainNames = append(domainNames, symbolDomains[value.Name]...)
			if factories[value.Name] {
				factoryNames = append(factoryNames, value.Name)
			}
		case *ast.SelectorExpr:
			if value.Sel.Name == "Setenv" || value.Sel.Name == "Getenv" || value.Sel.Name == "LookupEnv" || value.Sel.Name == "Environ" {
				environment = append(environment, value.Sel.Name)
			}
		}
		return true
	})
	domainNames = sortedUnique(domainNames)
	return testRecord{
		Name: fn.Name.Name, Kind: kind, Package: pkg, File: file, External: external,
		Domains: domainNames, StraddlesDomains: len(domainNames) > 1,
		EnvironmentAccess: sortedUnique(environment), GlobalFactoryAccesses: sortedUnique(factoryNames),
	}
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
