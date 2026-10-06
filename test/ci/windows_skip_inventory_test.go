package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// windowsSkipInventoryFile is the reviewed, checked-in list of packages whose
// default-tag tests the Windows gates do not run whole (#2791, the second
// branch of #2031). The Windows gate runs a curated selection rather than
// `go test ./...` because the full suite cannot fit its budget; this file
// makes the complement of that selection an enumerated artifact instead of
// whatever a regex happens not to match.
const windowsSkipInventoryFile = "windows-skip-inventory.json"

// Coverage values an inventory entry may declare. "partial" means the package
// is entered only through a step's -run selection; "none" means no Windows
// step runs its tests at all.
const (
	windowsCoverageWhole   = "whole"
	windowsCoveragePartial = "partial"
	windowsCoverageNone    = "none"
)

type windowsSkipInventory struct {
	Categories map[string]string           `json:"categories"`
	Packages   []windowsSkipInventoryEntry `json:"packages"`
}

type windowsSkipInventoryEntry struct {
	Package  string `json:"package"`
	Coverage string `json:"coverage"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// windowsTestInvocation is one `go test` command a Windows CI step runs.
type windowsTestInvocation struct {
	packages []string
	run      string
}

func TestWindowsSkipInventoryMatchesGateSelection(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	inventory := loadWindowsSkipInventory(t)
	invocations := windowsGateTestInvocations(t, loadCIWorkflow(t))
	packages := windowsTestPackages(t, root)
	coverage, err := windowsPackageCoverage(packages, invocations)
	if err != nil {
		t.Fatal(err)
	}
	problems := windowsSkipInventoryProblems(inventory, packages, coverage)
	if len(problems) > 0 {
		t.Fatalf("%s is out of date with the Windows CI gate (#2791); every package with Windows-buildable tests must either run whole in a Windows step or be listed with a reviewed reason:\n  %s",
			windowsSkipInventoryFile, strings.Join(problems, "\n  "))
	}
	counts := map[string]int{}
	for _, pkg := range packages {
		counts[coverage[pkg]]++
	}
	t.Logf("Windows test coverage across %d packages: %d whole, %d partial, %d not run",
		len(packages), counts[windowsCoverageWhole], counts[windowsCoveragePartial], counts[windowsCoverageNone])
}

func TestWindowsSkipInventoryProblemsDetectDrift(t *testing.T) {
	t.Parallel()
	inventory := windowsSkipInventory{
		Categories: map[string]string{"cost": "too slow"},
		Packages: []windowsSkipInventoryEntry{
			{Package: "a/listed", Coverage: windowsCoverageNone, Category: "cost", Reason: "slow"},
			{Package: "a/gone", Coverage: windowsCoverageNone, Category: "cost", Reason: "slow"},
			{Package: "a/nowcovered", Coverage: windowsCoverageNone, Category: "cost", Reason: "slow"},
			{Package: "a/partial", Coverage: windowsCoverageNone, Category: "cost", Reason: "slow"},
			{Package: "a/badcategory", Coverage: windowsCoverageNone, Category: "vibes", Reason: "slow"},
			{Package: "a/noreason", Coverage: windowsCoverageNone, Category: "cost"},
		},
	}
	packages := []string{"a/listed", "a/new", "a/nowcovered", "a/partial", "a/badcategory", "a/noreason"}
	coverage := map[string]string{
		"a/listed":      windowsCoverageNone,
		"a/new":         windowsCoverageNone,
		"a/nowcovered":  windowsCoverageWhole,
		"a/partial":     windowsCoveragePartial,
		"a/badcategory": windowsCoverageNone,
		"a/noreason":    windowsCoverageNone,
	}
	problems := strings.Join(windowsSkipInventoryProblems(inventory, packages, coverage), "\n")
	for _, want := range []string{
		"a/new: not run whole on Windows",
		"a/gone: listed but no such package",
		"a/nowcovered: listed but now runs whole",
		`a/partial: listed with coverage "none" but the gate runs it "partial"`,
		`a/badcategory: unknown category "vibes"`,
		"a/noreason: missing reason",
	} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems missing %q:\n%s", want, problems)
		}
	}
	if strings.Contains(problems, "a/listed") {
		t.Errorf("a correctly listed package was reported:\n%s", problems)
	}
}

func TestWindowsGoTestInvocationsParseStepCommands(t *testing.T) {
	t.Parallel()
	got := parseGoTestInvocations("echo hi\n" +
		"go test -v ./a ./b/... -count=1\n" +
		"go test -tags=integration ./c -run ^TestX -timeout=3m\n" +
		"go test ./d -run 'TestY|TestZ'\n" +
		"go test ./e \\\n  -run=TestW\n" +
		"go test ./f -skip TestSlow\n" +
		"go build ./...\n")
	want := []windowsTestInvocation{
		{packages: []string{"./a", "./b/..."}},
		{packages: []string{"./c"}, run: "^TestX"},
		{packages: []string{"./d"}, run: "TestY|TestZ"},
		{packages: []string{"./e"}, run: "TestW"},
		{packages: []string{"./f"}, run: "-skip TestSlow"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parseGoTestInvocations = %+v, want %+v", got, want)
	}
	coverage, err := windowsPackageCoverage([]string{"a", "b", "b/x", "c", "d", "e", "f", "g"}, got)
	if err != nil {
		t.Fatal(err)
	}
	wantCoverage := map[string]string{
		"a": windowsCoverageWhole, "b": windowsCoverageWhole, "b/x": windowsCoverageWhole,
		"c": windowsCoveragePartial, "d": windowsCoveragePartial, "e": windowsCoveragePartial,
		"f": windowsCoveragePartial, "g": windowsCoverageNone,
	}
	if fmt.Sprint(coverage) != fmt.Sprint(wantCoverage) {
		t.Fatalf("coverage = %v, want %v", coverage, wantCoverage)
	}
	if _, err := windowsPackageCoverage([]string{"a"}, []windowsTestInvocation{{packages: []string{"./missing"}}}); err == nil {
		t.Fatal("a step naming a package that does not exist must be reported, not silently ignored")
	}
}

func loadWindowsSkipInventory(t *testing.T) windowsSkipInventory {
	t.Helper()
	data, err := os.ReadFile(windowsSkipInventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	var inventory windowsSkipInventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatalf("parse %s: %v", windowsSkipInventoryFile, err)
	}
	return inventory
}

// windowsGateTestInvocations collects every `go test` the Windows CI legs run:
// the windows-smoke job's steps, plus the shipped-workflow suite that the
// shipped job's windows-latest leg runs through `go run ./test/ci group shipped`.
func windowsGateTestInvocations(t *testing.T, workflow ciWorkflow) []windowsTestInvocation {
	t.Helper()
	job, ok := workflow.Jobs["windows-smoke"]
	if !ok || job.RunsOn != "windows-latest" {
		t.Fatal("ci.yml must keep a windows-smoke job on windows-latest; update the Windows skip inventory test if the gate moved")
	}
	var invocations []windowsTestInvocation
	if job.If != ciFullGate || !slices.Contains(job.Needs, "scope") || job.ContinueOnError {
		t.Fatal("windows-smoke must run for the full profile for its selection to count as Windows coverage")
	}
	for _, step := range job.Steps {
		stepInvocations := parseGoTestInvocations(step.Run)
		if len(stepInvocations) > 0 && (step.If != "" || step.ContinueOnError) {
			t.Fatalf("windows-smoke step %q runs go test conditionally or non-fatally; it cannot count as Windows coverage", step.Name)
		}
		invocations = append(invocations, stepInvocations...)
	}
	shipped := workflow.Jobs["shipped"]
	if shipped.step(t, "Shipped-workflow contracts").Run != "go run ./test/ci group shipped" {
		t.Fatal("shipped job no longer runs `go run ./test/ci group shipped`; update the Windows skip inventory test")
	}
	if !shippedHasWindowsLeg(t) {
		t.Fatal("shipped job no longer has a windows-latest matrix leg; update the Windows skip inventory test")
	}
	for _, c := range shippedWorkflowChecks(toolchain{goCommand: "go"}, nil) {
		if len(c.args) > 0 && c.args[0] == "test" {
			invocations = append(invocations, goTestInvocation(c.args[1:]))
		}
	}
	return invocations
}

// shippedHasWindowsLeg reports whether the shipped job's matrix includes a
// windows-latest leg (ciJob models only the unit-shard matrix).
func shippedHasWindowsLeg(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						OS string `yaml:"os"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, leg := range workflow.Jobs["shipped"].Strategy.Matrix.Include {
		if leg.OS == "windows-latest" {
			return true
		}
	}
	return false
}

// parseGoTestInvocations extracts each `go test` command line from a step's
// run script.
func parseGoTestInvocations(script string) []windowsTestInvocation {
	var invocations []windowsTestInvocation
	// Join shell line continuations first, so a `-run` on a continuation
	// line still narrows the command it belongs to.
	script = strings.ReplaceAll(strings.ReplaceAll(script, "\r\n", "\n"), "\\\n", " ")
	for _, line := range strings.Split(script, "\n") {
		words := shellWords(line)
		if len(words) >= 2 && words[0] == "go" && words[1] == "test" {
			invocations = append(invocations, goTestInvocation(words[2:]))
		}
	}
	return invocations
}

func goTestInvocation(args []string) windowsTestInvocation {
	var invocation windowsTestInvocation
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-run" && i+1 < len(args):
			i++
			invocation.run = args[i]
		case strings.HasPrefix(arg, "-run="):
			invocation.run = strings.TrimPrefix(arg, "-run=")
		case arg == "-skip" && i+1 < len(args):
			// A -skip narrows the selection exactly like -run does.
			i++
			invocation.run = "-skip " + args[i]
		case strings.HasPrefix(arg, "-skip="):
			invocation.run = arg
		case arg == "-tags" || arg == "-timeout" || arg == "-count":
			i++ // the flag's value is the next word
		case strings.HasPrefix(arg, "./"):
			invocation.packages = append(invocation.packages, arg)
		}
	}
	return invocation
}

// shellWords splits a single command line into words, honouring single and
// double quotes. It is enough for the plain commands ci.yml's Windows steps
// use; anything fancier would not be a `go test` line this test cares about.
func shellWords(line string) []string {
	var words []string
	var current strings.Builder
	inWord := false
	var quote rune
	for _, r := range strings.TrimSpace(line) {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, current.String())
	}
	return words
}

// windowsTestPackages lists, module-relative, every package that has
// default-tag tests when built for windows. A package whose tests are all
// //go:build unix-tagged has nothing for Windows to run and is not a gap.
func windowsTestPackages(t *testing.T, root string) []string {
	t.Helper()
	command := exec.Command("go", "list", "-e", "-find", "-tags=",
		"-f", "{{.Dir}}\t{{len .TestGoFiles}}\t{{len .XTestGoFiles}}\t{{if .Error}}{{.Error.Err}}{{end}}", "./...")
	command.Dir = root
	command.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list (GOOS=windows): %v", err)
	}
	var packages []string
	for _, line := range strings.Split(strings.TrimRight(string(output), "\r\n"), "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 4)
		if len(fields) != 4 {
			t.Fatalf("unexpected go list line %q", line)
		}
		if fields[3] != "" && !strings.Contains(fields[3], "build constraints exclude all Go files") {
			t.Fatalf("go list %s: %s", fields[0], fields[3])
		}
		if fields[1] == "0" && fields[2] == "0" {
			continue
		}
		rel, err := filepath.Rel(root, fields[0])
		if err != nil {
			t.Fatal(err)
		}
		packages = append(packages, filepath.ToSlash(rel))
	}
	slices.Sort(packages)
	return packages
}

// windowsPackageCoverage classifies each package as run whole, run only
// through a named -run selection, or not run on Windows at all.
func windowsPackageCoverage(packages []string, invocations []windowsTestInvocation) (map[string]string, error) {
	coverage := map[string]string{}
	for _, pkg := range packages {
		coverage[pkg] = windowsCoverageNone
	}
	for _, invocation := range invocations {
		for _, pattern := range invocation.packages {
			pattern = strings.TrimPrefix(pattern, "./")
			recursive := strings.HasSuffix(pattern, "/...")
			base := strings.TrimSuffix(pattern, "/...")
			matched := false
			for _, pkg := range packages {
				if pkg != base && (!recursive || !strings.HasPrefix(pkg, base+"/")) {
					continue
				}
				matched = true
				if invocation.run == "" {
					coverage[pkg] = windowsCoverageWhole
				} else if coverage[pkg] == windowsCoverageNone {
					coverage[pkg] = windowsCoveragePartial
				}
			}
			if !matched {
				return nil, fmt.Errorf("a Windows CI step tests ./%s, which matches no package with Windows-buildable tests", pattern)
			}
		}
	}
	return coverage, nil
}

func windowsSkipInventoryProblems(inventory windowsSkipInventory, packages []string, coverage map[string]string) []string {
	var problems []string
	listed := map[string]bool{}
	for _, entry := range inventory.Packages {
		if listed[entry.Package] {
			problems = append(problems, fmt.Sprintf("%s: listed more than once", entry.Package))
		}
		listed[entry.Package] = true
		if _, ok := inventory.Categories[entry.Category]; !ok {
			problems = append(problems, fmt.Sprintf("%s: unknown category %q", entry.Package, entry.Category))
		}
		if strings.TrimSpace(entry.Reason) == "" {
			problems = append(problems, fmt.Sprintf("%s: missing reason", entry.Package))
		}
		actual, exists := coverage[entry.Package]
		switch {
		case !exists:
			problems = append(problems, fmt.Sprintf("%s: listed but no such package has Windows-buildable tests; remove the entry", entry.Package))
		case actual == windowsCoverageWhole:
			problems = append(problems, fmt.Sprintf("%s: listed but now runs whole on Windows; remove the entry", entry.Package))
		case actual != entry.Coverage:
			problems = append(problems, fmt.Sprintf("%s: listed with coverage %q but the gate runs it %q", entry.Package, entry.Coverage, actual))
		}
	}
	for _, pkg := range packages {
		if coverage[pkg] != windowsCoverageWhole && !listed[pkg] {
			problems = append(problems, fmt.Sprintf("%s: not run whole on Windows (coverage %q) and not listed; add it to the Windows CI gate or to %s with a reason",
				pkg, coverage[pkg], windowsSkipInventoryFile))
		}
	}
	slices.Sort(problems)
	return problems
}
