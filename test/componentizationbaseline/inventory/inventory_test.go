package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildInventoryNormalizesModulePathsAndClassifiesTests(t *testing.T) {
	module, prod, test := fixturePackages(t)
	got, err := buildInventory(module, goEnv{
		GOOS: "linux", GOARCH: "amd64", CGOEnabled: "0", GoVersion: "go1.test",
	}, "abc123", []string{"integration", "custom"}, "./cmd/goobers", prod, test)
	if err != nil {
		t.Fatal(err)
	}

	if got.Source.ModulePath != "example.test/renamed/module" {
		t.Fatalf("module path = %q", got.Source.ModulePath)
	}
	if !reflect.DeepEqual(got.Build.BuildTags, []string{"custom", "integration"}) {
		t.Fatalf("build tags = %#v", got.Build.BuildTags)
	}
	wantProd := []string{
		"example.test/renamed/module/cmd/goobers",
		"example.test/renamed/module/internal/shared",
	}
	if !reflect.DeepEqual(got.Command.ProductionClosure, wantProd) {
		t.Fatalf("production closure = %#v", got.Command.ProductionClosure)
	}
	if !reflect.DeepEqual(got.Command.TestOnlyDependencies, []string{"example.test/renamed/module/internal/testkit"}) {
		t.Fatalf("test-only dependencies = %#v", got.Command.TestOnlyDependencies)
	}
	if got.Command.Tests != 2 || got.Command.Benchmarks != 1 || got.Command.Examples != 1 {
		t.Fatalf("function counts = tests %d, benchmarks %d, examples %d", got.Command.Tests, got.Command.Benchmarks, got.Command.Examples)
	}

	var straddling testRecord
	for _, record := range got.Tests {
		if record.Name == "TestAcrossDomains" {
			straddling = record
			break
		}
	}
	if !straddling.StraddlesDomains {
		t.Fatalf("TestAcrossDomains = %#v", straddling)
	}
	if !reflect.DeepEqual(straddling.Domains, []string{"contention", "docs-churn"}) {
		t.Fatalf("domains = %#v", straddling.Domains)
	}
	if !reflect.DeepEqual(straddling.EnvironmentAccess, []string{"Getenv", "Setenv"}) {
		t.Fatalf("environment access = %#v", straddling.EnvironmentAccess)
	}
	if !reflect.DeepEqual(straddling.GlobalFactoryAccesses, []string{"newDependency"}) {
		t.Fatalf("global factories = %#v", straddling.GlobalFactoryAccesses)
	}

	for _, pkg := range got.Packages {
		if filepath.IsAbs(pkg.Path) {
			t.Fatalf("absolute package path leaked: %q", pkg.Path)
		}
		for _, path := range append(append(pkg.ProductionFiles, pkg.InternalTestFiles...), pkg.ExternalTestFiles...) {
			if filepath.IsAbs(path) || strings.Contains(path, `\`) {
				t.Fatalf("non-normalized repository path: %q", path)
			}
		}
	}
	if !reflect.DeepEqual(got.Packages[0].IgnoredFiles, []string{"cmd/goobers/platform_windows.go"}) {
		t.Fatalf("ignored files = %#v", got.Packages[0].IgnoredFiles)
	}
	for _, domain := range got.Domains {
		if domain.Name == "docs-churn" && !reflect.DeepEqual(domain.ExistingHelpers, []string{"example.test/renamed/module/cmd/goobers.commonHelper"}) {
			t.Fatalf("docs-churn helpers = %#v", domain.ExistingHelpers)
		}
	}
}

func TestInventoryOrderingIsDeterministic(t *testing.T) {
	module, prod, test := fixturePackages(t)
	first, err := buildInventory(module, goEnv{GOOS: "linux", GOARCH: "amd64"}, "abc", []string{"z", "a"}, "./cmd/goobers", prod, test)
	if err != nil {
		t.Fatal(err)
	}
	reversePackages(prod)
	reversePackages(test)
	second, err := buildInventory(module, goEnv{GOOS: "linux", GOARCH: "amd64"}, "abc", []string{"a", "z"}, "./cmd/goobers", prod, test)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("inventory changed with discovery order:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}

func TestBuildContextDifferencesAreRecorded(t *testing.T) {
	module, prod, test := fixturePackages(t)
	linux, err := buildInventory(module, goEnv{GOOS: "linux", GOARCH: "amd64"}, "abc", nil, "./cmd/goobers", prod, test)
	if err != nil {
		t.Fatal(err)
	}
	windows, err := buildInventory(module, goEnv{GOOS: "windows", GOARCH: "arm64"}, "abc", nil, "./cmd/goobers", prod, test)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(linux.Build, windows.Build) {
		t.Fatal("different build contexts produced equal metadata")
	}
	if windows.Build.GOOS != "windows" || windows.Build.GOARCH != "arm64" {
		t.Fatalf("windows context = %#v", windows.Build)
	}
	if len(windows.Omissions) == 0 || !strings.Contains(strings.Join(windows.Omissions, " "), "other platform") {
		t.Fatalf("omissions = %#v", windows.Omissions)
	}
}

func TestDiscoveryReportsMalformedAndFailingCommands(t *testing.T) {
	t.Run("malformed module metadata", func(t *testing.T) {
		runner := scriptedRunner{responses: []scriptedResponse{{
			name: "go", args: "list -m -json", result: commandResult{stdout: []byte(`{"Path":`)},
		}}}
		_, err := (discovery{runner: &runner}).collect(context.Background(), "./cmd/goobers")
		if err == nil || !strings.Contains(err.Error(), "decode `go list -m -json`") || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("go list failure includes stderr", func(t *testing.T) {
		runner := scriptedRunner{responses: []scriptedResponse{{
			name: "go", args: "list -m -json",
			result: commandResult{stderr: []byte("go: module graph is broken\n")},
			err:    errors.New("exit status 1"),
		}}}
		_, err := (discovery{runner: &runner}).collect(context.Background(), "./cmd/goobers")
		if err == nil || !strings.Contains(err.Error(), "go list -m -json failed: go: module graph is broken") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("package error is not a partial inventory", func(t *testing.T) {
		_, err := decodePackages([]byte(`{"ImportPath":"example.test/bad","Error":{"Err":"missing generated package"}}`), "production go list")
		if err == nil || !strings.Contains(err.Error(), `package "example.test/bad": missing generated package`) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunDoesNotPublishPartialOutput(t *testing.T) {
	runner := scriptedRunner{responses: []scriptedResponse{{
		name: "go", args: "list -m -json", result: commandResult{stdout: []byte(`not-json`)},
	}}}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, &stdout, &stderr, &runner)
	if code != 1 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("partial output = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "inventory failed") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func fixturePackages(t *testing.T) (moduleMetadata, []goPackage, []goPackage) {
	t.Helper()
	root := t.TempDir()
	commandDir := filepath.Join(root, "cmd", "goobers")
	sharedDir := filepath.Join(root, "internal", "shared")
	testkitDir := filepath.Join(root, "internal", "testkit")
	for _, dir := range []string{commandDir, sharedDir, testkitDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(commandDir, "docchurn.go"): `package main
import "example.test/renamed/module/internal/shared"
var newDependency = func() int { return shared.Value }
func runDocsChurn() { commonHelper() }`,
		filepath.Join(commandDir, "contestedfiles.go"): `package main
func partitionByContention() {}`,
		filepath.Join(commandDir, "reportprstatus.go"): `package main
func runReportPRStatus() {}`,
		filepath.Join(commandDir, "helpers.go"): `package main
func commonHelper() {}
`,
		filepath.Join(commandDir, "inventory_test.go"): `package main
import ("os"; "testing")
func TestAcrossDomains(t *testing.T) {
	t.Setenv("KEY", "value")
	_ = os.Getenv("KEY")
	runDocsChurn()
	partitionByContention()
	_ = newDependency()
}
func BenchmarkDocs(b *testing.B) { runDocsChurn() }
func Example_docs() { runDocsChurn() }`,
		filepath.Join(commandDir, "external_test.go"): `package main_test
import ("testing"; goobers "example.test/renamed/module/cmd/goobers")
func TestExternal(t *testing.T) { goobers.RunReportPRStatus() }`,
		filepath.Join(sharedDir, "shared.go"):   "package shared\nconst Value = 1\n",
		filepath.Join(testkitDir, "testkit.go"): "package testkit\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const modulePath = "example.test/renamed/module"
	command := goPackage{
		ImportPath: modulePath + "/cmd/goobers", Name: "main", Dir: commandDir,
		GoFiles:     []string{"reportprstatus.go", "docchurn.go", "contestedfiles.go", "helpers.go"},
		TestGoFiles: []string{"inventory_test.go"}, XTestGoFiles: []string{"external_test.go"},
		IgnoredGoFiles: []string{"platform_windows.go"},
		Imports:        []string{modulePath + "/internal/shared", "fmt"},
		TestImports:    []string{"testing", modulePath + "/internal/testkit"},
		XTestImports:   []string{"testing", modulePath + "/cmd/goobers"},
	}
	shared := goPackage{ImportPath: modulePath + "/internal/shared", Name: "shared", Dir: sharedDir, GoFiles: []string{"shared.go"}}
	testkit := goPackage{ImportPath: modulePath + "/internal/testkit", Name: "testkit", Dir: testkitDir, GoFiles: []string{"testkit.go"}}
	prod := []goPackage{{ImportPath: "fmt", Name: "fmt"}, shared, command}
	test := []goPackage{testkit, command, shared, {ImportPath: "testing", Name: "testing"}}
	return moduleMetadata{Path: modulePath, Dir: root}, prod, test
}

func reversePackages(packages []goPackage) {
	for left, right := 0, len(packages)-1; left < right; left, right = left+1, right-1 {
		packages[left], packages[right] = packages[right], packages[left]
	}
}

type scriptedResponse struct {
	name   string
	args   string
	result commandResult
	err    error
}

type scriptedRunner struct {
	responses []scriptedResponse
}

func (r *scriptedRunner) run(_ context.Context, name string, args, _ []string) (commandResult, error) {
	if len(r.responses) == 0 {
		return commandResult{}, errors.New("unexpected command")
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	if response.name != name || response.args != strings.Join(args, " ") {
		return commandResult{}, errors.New("unexpected command: " + name + " " + strings.Join(args, " "))
	}
	return response.result, response.err
}
