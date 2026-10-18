package main

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStressWeightsRejectMissingOrInvalidData(t *testing.T) {
	for _, data := range []string{
		`{`,
		`{"schemaVersion":2,"defaultMillis":2000,"packages":{"./p":{"TestA":1}}}`,
		`{"schemaVersion":1,"defaultMillis":0,"packages":{"./p":{"TestA":1}}}`,
		`{"schemaVersion":1,"defaultMillis":60001,"packages":{"./p":{"TestA":1}}}`,
		`{"schemaVersion":1,"defaultMillis":2000,"packages":{}}`,
		`{"schemaVersion":1,"defaultMillis":2000,"packages":{"./p":{}}}`,
		`{"schemaVersion":1,"defaultMillis":2000,"packages":{"./p":{"TestA":-1}}}`,
		`{"schemaVersion":1,"defaultMillis":2000,"packages":{"./p":{"TestA":1800001}}}`,
		`{"schemaVersion":1,"defaultMillis":2000,"packages":{"./p":{"not-a-test":1}}}`,
	} {
		if _, err := loadStressWeights([]byte(data)); err == nil {
			t.Errorf("invalid weights accepted: %s", data)
		}
	}
	if _, err := weightedStressTests([]string{"TestNew"}, shardSpec{Package: "./unmeasured", Shards: 2}); err == nil || !strings.Contains(err.Error(), "no measured stress weights") {
		t.Fatalf("missing package weights error = %v", err)
	}
}

func currentLinuxStressTests(t *testing.T) []string {
	t.Helper()
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "linux", "amd64", true
	ctx.BuildTags = append(slices.Clone(ctx.BuildTags), "race")
	dir := filepath.Join("..", "..", "cmd", "goobers")
	pkg, err := ctx.ImportDir(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range append(slices.Clone(pkg.TestGoFiles), pkg.XTestGoFiles...) {
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, file), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name != "TestMain" && testNamePattern.MatchString(fn.Name.Name) {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names
}

func TestWeightedStressPartitionPreservesCurrentLinuxTestCoverage(t *testing.T) {
	names := currentLinuxStressTests(t)
	file, err := os.Open("packages.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	packages, err := loadPackages(file)
	if err != nil {
		t.Fatal(err)
	}
	var spec packageSpec
	for _, candidate := range packages {
		if candidate.Package == "./cmd/goobers" {
			spec = candidate
		}
	}
	if spec.Shards != 32 || spec.Count != 20 || len(names) == 0 {
		t.Fatalf("unexpected stress enrollment or empty test inventory: %+v, %d tests", spec, len(names))
	}
	excluded := parseExcludedTests(spec.Exclude)
	names = slices.DeleteFunc(names, func(name string) bool { _, skip := excluded[name]; return skip })
	// New or renamed tests remain enrolled at the documented fallback weight.
	names = append(names, "TestNewUnmeasuredStressCoverage")
	seen := map[string]int{}
	for index := range spec.Shards {
		shard := shardSpec{Package: spec.Package, Shards: spec.Shards, Index: index}
		selected, err := weightedStressTests(names, shard)
		if err != nil || len(selected) == 0 {
			t.Fatalf("shard %d: %v, %v", index, selected, err)
		}
		permuted := slices.Clone(names)
		slices.Reverse(permuted)
		again, err := weightedStressTests(permuted, shard)
		if err != nil || !slices.Equal(selected, again) {
			t.Fatalf("shard %d changed after input permutation: %v", index, err)
		}
		for _, name := range selected {
			if _, skip := excluded[name]; skip {
				t.Errorf("excluded test scheduled: %s", name)
			}
			seen[name]++
		}
	}
	for _, name := range names {
		if seen[name] != 1 {
			t.Errorf("%s appears %d times, want exactly once", name, seen[name])
		}
	}
}
