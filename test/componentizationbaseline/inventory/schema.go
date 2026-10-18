package main

const schemaVersion = "goobers.dev/componentization-baseline/inventory/v1"

type inventory struct {
	SchemaVersion string          `json:"schemaVersion"`
	Source        source          `json:"source"`
	Build         buildContext    `json:"buildContext"`
	Command       commandBoundary `json:"command"`
	Packages      []packageRecord `json:"packages"`
	Tests         []testRecord    `json:"tests"`
	Domains       []domainRecord  `json:"domains"`
	Omissions     []string        `json:"omissions"`
}

type source struct {
	Commit     string `json:"commit"`
	ModulePath string `json:"modulePath"`
}

type buildContext struct {
	GoVersion  string   `json:"goVersion"`
	GOOS       string   `json:"goos"`
	GOARCH     string   `json:"goarch"`
	CGOEnabled string   `json:"cgoEnabled"`
	BuildTags  []string `json:"buildTags"`
}

type commandBoundary struct {
	Package              string   `json:"package"`
	ProductionClosure    []string `json:"productionClosure"`
	TestClosure          []string `json:"testClosure"`
	TestOnlyDependencies []string `json:"testOnlyDependencies"`
	DirectImports        []string `json:"directImports"`
	InternalTestImports  []string `json:"internalTestImports"`
	ExternalTestImports  []string `json:"externalTestImports"`
	ProductionFiles      int      `json:"productionFiles"`
	InternalTestFiles    int      `json:"internalTestFiles"`
	ExternalTestFiles    int      `json:"externalTestFiles"`
	Tests                int      `json:"tests"`
	Benchmarks           int      `json:"benchmarks"`
	Examples             int      `json:"examples"`
}

type packageRecord struct {
	ImportPath          string   `json:"importPath"`
	Path                string   `json:"path"`
	ProductionFiles     []string `json:"productionFiles"`
	InternalTestFiles   []string `json:"internalTestFiles"`
	ExternalTestFiles   []string `json:"externalTestFiles"`
	IgnoredFiles        []string `json:"ignoredFiles"`
	DirectImports       []string `json:"directImports"`
	InternalTestImports []string `json:"internalTestImports"`
	ExternalTestImports []string `json:"externalTestImports"`
	InProductionClosure bool     `json:"inProductionClosure"`
	InTestClosure       bool     `json:"inTestClosure"`
	TestOnly            bool     `json:"testOnly"`
}

type testRecord struct {
	Name                  string   `json:"name"`
	Kind                  string   `json:"kind"`
	Package               string   `json:"package"`
	File                  string   `json:"file"`
	External              bool     `json:"external"`
	Domains               []string `json:"domains"`
	StraddlesDomains      bool     `json:"straddlesDomains"`
	EnvironmentAccess     []string `json:"environmentAccess"`
	GlobalFactoryAccesses []string `json:"globalFactoryAccesses"`
}

type domainRecord struct {
	Name                 string   `json:"name"`
	SourceFiles          []string `json:"sourceFiles"`
	TestFiles            []string `json:"testFiles"`
	ProductionSymbols    []string `json:"productionSymbols"`
	ExistingHelpers      []string `json:"existingHelpers"`
	ReusableLocalImports []string `json:"reusableLocalImports"`
}
