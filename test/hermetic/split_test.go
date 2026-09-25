package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// linuxRaceShards mirrors the `unit` job's shard matrix in
// .github/workflows/ci.yml (pinned there by test/ci's
// TestCILinuxJournalOTLPRaceCoverage).
const linuxRaceShards = 5

// selectShardPackages is the whole-package half of one shard's selection with
// no split packages configured.
func selectShardPackages(pkgs []string, spec shardSpec, weights shardWeights) []string {
	return assignShards(shardItems(pkgs, weights, shardSplits{}), spec.total)[spec.index-1].packages
}

// selectedTests evaluates a piece's go test filter the way the testing
// package does for top-level names, returning which of names would run.
func selectedTests(t *testing.T, args, names []string) []string {
	t.Helper()
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-run" && args[index] != "-skip" {
			continue
		}
		pattern := regexp.MustCompile(args[index+1])
		var result []string
		for _, name := range names {
			if pattern.MatchString(name) == (args[index] == "-run") {
				result = append(result, name)
			}
		}
		return result
	}
	return append([]string(nil), names...)
}

func splitFixture() (shardWeights, shardSplits, []string, map[string][]string) {
	weights := shardWeights{
		DefaultSeconds: 1,
		Packages: map[string]float64{
			"pkg/big":   900,
			"pkg/rel":   500,
			"pkg/mid/1": 200,
			"pkg/mid/2": 150,
		},
	}
	enumerated := map[string][]string{"pkg/big": nil, "pkg/rel": nil}
	measured := map[string]float64{}
	for index := 0; index < 400; index++ {
		name := fmt.Sprintf("TestBig%03d", index)
		enumerated["pkg/big"] = append(enumerated["pkg/big"], name)
		if index%7 != 0 { // every seventh test is new: absent from the weights
			measured[name] = float64(index%13) + 0.25
		}
	}
	enumerated["pkg/big"] = append(enumerated["pkg/big"], "ExampleBig", "FuzzBig", "TestBig.With+Meta(chars)")
	enumerated["pkg/rel"] = []string{"TestReleaseHuge", "TestReleaseSmall", "TestReleaseNew"}
	splits := shardSplits{Packages: map[string]splitPackage{
		"pkg/big": {Pieces: 3, Tests: measured},
		"pkg/rel": {Pieces: 2, Tests: map[string]float64{"TestReleaseHuge": 160, "TestReleaseSmall": 3}},
	}}
	packages := []string{"pkg/mid/2", "pkg/big", "pkg/small", "pkg/rel", "pkg/mid/1", "pkg/other"}
	return weights, splits, packages, enumerated
}

// TestSplitShardsRunEveryTestExactlyOnce is the coverage-integrity guard for
// test-level splitting: across every shard count, each whole package lands in
// exactly one shard, and each enumerated top-level test of a split package —
// measured or new, whatever its characters — is selected by exactly one
// piece's go test filter.
func TestSplitShardsRunEveryTestExactlyOnce(t *testing.T) {
	t.Parallel()
	weights, splits, packages, enumerated := splitFixture()
	testArgs := []string{"-race", "-count=1", "./..."}
	for total := 1; total <= 8; total++ {
		wholeRuns := map[string]int{}
		testRuns := map[string]map[string]int{"pkg/big": {}, "pkg/rel": {}}
		for _, shard := range assignShards(shardItems(packages, weights, splits), total) {
			for _, pkg := range shard.packages {
				wholeRuns[pkg]++
			}
			for _, pkg := range piecePackages(shard.pieces) {
				plan, err := piecePlan(testArgs, pkg, enumerated[pkg], splits.Packages[pkg], piecesOf(shard.pieces, pkg))
				if err != nil {
					t.Fatal(err)
				}
				if plan.testArgs[len(plan.testArgs)-1] != pkg || slices.Contains(plan.testArgs, "./...") {
					t.Fatalf("piece args %q must name only %s", plan.testArgs, pkg)
				}
				for _, name := range selectedTests(t, plan.testArgs, enumerated[pkg]) {
					testRuns[pkg][name]++
				}
			}
		}
		for _, pkg := range []string{"pkg/mid/1", "pkg/mid/2", "pkg/small", "pkg/other"} {
			if wholeRuns[pkg] != 1 {
				t.Fatalf("total=%d: whole package %s ran %d times, want 1", total, pkg, wholeRuns[pkg])
			}
		}
		for _, pkg := range []string{"pkg/big", "pkg/rel"} {
			if wholeRuns[pkg] != 0 {
				t.Fatalf("total=%d: split package %s also ran whole", total, pkg)
			}
			for _, name := range enumerated[pkg] {
				if testRuns[pkg][name] != 1 {
					t.Fatalf("total=%d: %s %s ran in %d pieces, want exactly 1", total, pkg, name, testRuns[pkg][name])
				}
			}
			if len(testRuns[pkg]) != len(enumerated[pkg]) {
				t.Fatalf("total=%d: %s selected unknown tests: %v", total, pkg, testRuns[pkg])
			}
		}
	}
}

func TestSplitShardPlanIsDeterministic(t *testing.T) {
	t.Parallel()
	weights, splits, packages, enumerated := splitFixture()
	reversedPackages := slices.Clone(packages)
	slices.Reverse(reversedPackages)
	reversedNames := slices.Clone(enumerated["pkg/big"])
	slices.Reverse(reversedNames)
	forward := assignShards(shardItems(packages, weights, splits), 5)
	backward := assignShards(shardItems(reversedPackages, weights, splits), 5)
	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("shard assignment depends on package order:\n%v\n%v", forward, backward)
	}
	split := splits.Packages["pkg/big"]
	want := partitionTests(enumerated["pkg/big"], split)
	if !reflect.DeepEqual(want, partitionTests(reversedNames, split)) {
		t.Fatal("test partition depends on enumeration order")
	}
	// Each runner decodes its own copy of the table, so map iteration order
	// differs between them; the partition must not.
	for attempt := 0; attempt < 20; attempt++ {
		copied := splitPackage{Pieces: split.Pieces, Tests: map[string]float64{}}
		for name, seconds := range split.Tests {
			copied.Tests[name] = seconds + 0.1 - 0.1
		}
		if !reflect.DeepEqual(want, partitionTests(enumerated["pkg/big"], copied)) {
			t.Fatal("test partition depends on map iteration order")
		}
	}
}

func TestPartitionTestsBalancesAndGivesNewTestsTheMeanWeight(t *testing.T) {
	t.Parallel()
	split := splitPackage{Pieces: 2, Tests: map[string]float64{"TestA": 10, "TestB": 6, "TestC": 4}}
	if got := split.defaultTestMillis(); got != 20000/3 {
		t.Fatalf("default test seconds = %v, want the measured mean", got)
	}
	groups := partitionTests([]string{"TestC", "TestNew", "TestA", "TestB", "TestA"}, split)
	want := [][]string{{"TestA", "TestC"}, {"TestB", "TestNew"}}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
}

func TestPieceArgsPicksTheShorterExactFilter(t *testing.T) {
	t.Parallel()
	all := []string{"TestA", "TestB", "TestC", "TestD"}
	args, err := pieceArgs([]string{"-race", "./..."}, "pkg", []string{"TestA"}, all)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-race", "-run", "^(?:TestA)$", "pkg"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
	args, err = pieceArgs([]string{"-race", "./..."}, "pkg", []string{"TestA", "TestB", "TestC"}, all)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-race", "-skip", "^(?:TestD)$", "pkg"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
	args, err = pieceArgs([]string{"./..."}, "pkg", all, all)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pkg"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q, want the whole package unfiltered", args)
	}
}

func TestPieceArgsRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"-run", "-run=X", "-skip", "-skip=X"} {
		if _, err := pieceArgs([]string{existing, "./..."}, "pkg", []string{"TestA"}, []string{"TestA", "TestB"}); err == nil {
			t.Errorf("pieceArgs accepted go test arguments carrying %s", existing)
		}
	}
	var many []string
	for index := 0; index < 8000; index++ {
		many = append(many, fmt.Sprintf("TestAVeryLongGeneratedNameForTheArgumentLimit%05d", index))
	}
	half := many[:len(many)/2]
	if _, err := pieceArgs([]string{"./..."}, "pkg", half, many); err == nil || !strings.Contains(err.Error(), "raise its piece count") {
		t.Fatalf("oversized filter error = %v, want a piece-count remedy", err)
	}
}

func TestParseTestListKeepsOnlyRunnableTopLevelNames(t *testing.T) {
	t.Parallel()
	output := "TestAlpha\nExampleBeta\nFuzzGamma\nBenchmarkDelta\nTestWith_Underscore\nok  \tpkg\t0.412s\n\n"
	want := []string{"TestAlpha", "ExampleBeta", "FuzzGamma", "TestWith_Underscore"}
	if got := parseTestList(output); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseTestList = %q, want %q", got, want)
	}
}

func TestLoadShardSplitsValidates(t *testing.T) {
	t.Parallel()
	valid := `{"schemaVersion":1,"source":{"run":1,"commit":"` + strings.Repeat("a", 40) +
		`","timingJobs":["unit"]},"packages":{"pkg":{"pieces":2,"tests":{"TestA":1}}}}`
	for name, document := range map[string]string{
		"schema":      strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":2`, 1),
		"run":         strings.Replace(valid, `"run":1`, `"run":0`, 1),
		"commit":      strings.Replace(valid, strings.Repeat("a", 40), "abc", 1),
		"one piece":   strings.Replace(valid, `"pieces":2`, `"pieces":1`, 1),
		"subtest":     strings.Replace(valid, `"TestA"`, `"TestA/sub"`, 1),
		"negative":    strings.Replace(valid, `"TestA":1`, `"TestA":-1`, 1),
		"not a test":  strings.Replace(valid, `"TestA"`, `"helper"`, 1),
		"no job list": strings.Replace(valid, `"timingJobs":["unit"]`, `"timingJobs":[]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSplitsFixture(t, root, document)
			if _, err := loadShardSplits(root); err == nil {
				t.Fatalf("loadShardSplits accepted %s", document)
			}
		})
	}
	root := t.TempDir()
	writeSplitsFixture(t, root, valid)
	if _, err := loadShardSplits(root); err != nil {
		t.Fatal(err)
	}
}

func writeSplitsFixture(t *testing.T, root, document string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, shardSplitsPath), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCheckedInSplitsNameRealPackages keeps the split table from silently
// outliving a rename: a split entry for a package go list no longer reports
// would quietly stop splitting anything.
func TestCheckedInSplitsNameRealPackages(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	splits, err := loadShardSplits(root)
	if err != nil {
		t.Fatal(err)
	}
	for pkg, split := range splits.Packages {
		list := exec.Command("go", "list", pkg)
		list.Dir = root
		if output, err := list.CombinedOutput(); err != nil {
			t.Errorf("split package %s is not a module package: %v\n%s", pkg, err, output)
		}
		if len(split.Tests) == 0 {
			t.Errorf("split package %s has no measured tests", pkg)
		}
	}
}

// TestCheckedInSplitFiltersKeepHeadroom fails well before a piece's filter
// reaches maxRunPatternBytes in CI, while raising the piece count is still a
// routine change rather than a red merge gate.
func TestCheckedInSplitFiltersKeepHeadroom(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	splits, err := loadShardSplits(root)
	if err != nil {
		t.Fatal(err)
	}
	const headroomBytes = maxRunPatternBytes * 3 / 4
	for pkg, split := range splits.Packages {
		names := make([]string, 0, len(split.Tests))
		for name := range split.Tests {
			names = append(names, name)
		}
		groups := partitionTests(names, split)
		for index, group := range groups {
			args, err := pieceArgs([]string{"./..."}, pkg, group, groupsUnion(groups))
			if err != nil {
				t.Fatal(err)
			}
			if size := len(args[1]); size > headroomBytes {
				t.Errorf("%s piece %d/%d filter is %d bytes (headroom limit %d, hard limit %d): raise its pieces in %s",
					pkg, index+1, split.Pieces, size, headroomBytes, maxRunPatternBytes, shardSplitsPath)
			}
		}
	}
}

func TestAssignTimingOutputsNamesOnePartPerPlan(t *testing.T) {
	t.Parallel()
	plans := []testPlan{{}, {}}
	assignTimingOutputs(plans, "test-timings/unit-race.json")
	if plans[0].timingOutput != "test-timings/unit-race.part1.json" || plans[1].timingOutput != "test-timings/unit-race.part2.json" {
		t.Fatalf("timing outputs = %q, %q", plans[0].timingOutput, plans[1].timingOutput)
	}
	untimed := []testPlan{{}}
	assignTimingOutputs(untimed, "")
	if untimed[0].timingOutput != "" {
		t.Fatalf("untimed plan got timing output %q", untimed[0].timingOutput)
	}
}

func TestLineWriterForwardsOnlyWholeLines(t *testing.T) {
	t.Parallel()
	var destination bytes.Buffer
	writer := &lineWriter{destination: &destination}
	for _, chunk := range []string{"ok pk", "g 1s\nFAIL", " other\npartial"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := destination.String(); got != "ok pkg 1s\nFAIL other\n" {
		t.Fatalf("forwarded %q before flush", got)
	}
	writer.flush()
	if got := destination.String(); got != "ok pkg 1s\nFAIL other\npartial" {
		t.Fatalf("forwarded %q after flush", got)
	}
}

// TestSplitPiecesRunEveryFixtureTestExactlyOnce drives the real go toolchain:
// it enumerates a fixture package with listPackageTests, runs each piece's
// arguments with `go test -json`, and proves the testing package's own -run
// and -skip handling executes every top-level test (with its subtests) in
// exactly one piece.
func TestSplitPiecesRunEveryFixtureTestExactlyOnce(t *testing.T) {
	root := writeSplitModuleFixture(t)
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOWORK=off")
	testArgs := []string{"-count=1", "./..."}
	names, err := listPackageTests("go", root, env, testArgs, "./splitfixture")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if want := []string{"ExampleShout", "FuzzEcho", "TestAlpha", "TestBeta", "TestDelta", "TestGamma", "TestNested"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("enumerated %q, want %q", names, want)
	}
	split := splitPackage{Pieces: 3, Tests: map[string]float64{"TestAlpha": 5, "TestBeta": 1}}
	ran := map[string]int{}
	for piece := 1; piece <= split.Pieces; piece++ {
		plan, err := piecePlan(testArgs, "./splitfixture", names, split, []int{piece})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range runFixturePiece(t, root, env, plan.testArgs) {
			ran[name]++
		}
	}
	for _, name := range append(names, "TestNested/inner") {
		if ran[name] != 1 {
			t.Errorf("%s ran %d times across pieces, want exactly 1 (%v)", name, ran[name], ran)
		}
	}
}

func writeSplitModuleFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                  "module example.com/splitfixture\n\ngo 1.21\n",
		"splitfixture/fixture.go": "package splitfixture\n\nfunc Shout(s string) string { return s + \"!\" }\n",
		"splitfixture/fixture_test.go": `package splitfixture

import (
	"fmt"
	"testing"
)

func TestAlpha(t *testing.T) {}
func TestBeta(t *testing.T)  {}
func TestGamma(t *testing.T) {}
func TestDelta(t *testing.T) {}
func TestNested(t *testing.T) {
	t.Run("inner", func(t *testing.T) {})
}
func FuzzEcho(f *testing.F) {
	f.Add("seed")
	f.Fuzz(func(t *testing.T, s string) {})
}
func ExampleShout() {
	fmt.Println(Shout("hi"))
	// Output: hi!
}
func BenchmarkShout(b *testing.B) {}
`,
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runFixturePiece runs one piece and returns every test (and subtest) that
// ran to a terminal pass.
func runFixturePiece(t *testing.T, root string, env, testArgs []string) []string {
	t.Helper()
	command := exec.Command("go", append([]string{"test", "-json"}, testArgs...)...)
	command.Dir = root
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go test %q: %v\n%s", testArgs, err, output)
	}
	var ran []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var event struct {
			Action string
			Test   string
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Action == "pass" && event.Test != "" {
			ran = append(ran, event.Test)
		}
	}
	return ran
}
