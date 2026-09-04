package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const branchyFunction = `package sample

func Branchy(values []int) int {
	total := 0
	for _, value := range values {
		if value > 1 && value < 10 {
			total += value
		}
		switch value {
		case 1:
			total++
		case 2:
			total += 2
		}
	}
	return total
}
`

func TestComplexityMatchesGocycloScoring(t *testing.T) {
	t.Parallel()
	functions, err := scanFile("sample/sample.go", []byte(branchyFunction))
	if err != nil {
		t.Fatalf("scanFile: %v", err)
	}
	if len(functions) != 1 {
		t.Fatalf("functions = %d, want 1", len(functions))
	}
	// 1 base + range + if + && + two non-default cases.
	if got, want := functions[0].Complexity, 6; got != want {
		t.Errorf("complexity = %d, want %d", got, want)
	}
	if functions[0].Symbol != "Branchy" {
		t.Errorf("symbol = %q, want Branchy", functions[0].Symbol)
	}
}

func TestScanFileNamesMethodsWithReceiver(t *testing.T) {
	t.Parallel()
	source := `package sample

type store[T any] struct{}

func (s *store[T]) Put(v T) {}

func (s store[T]) Get() {}
`
	functions, err := scanFile("sample/sample.go", []byte(source))
	if err != nil {
		t.Fatalf("scanFile: %v", err)
	}
	var symbols []string
	for _, current := range functions {
		symbols = append(symbols, current.Symbol)
	}
	want := []string{"(*store).Put", "(store).Get"}
	if strings.Join(symbols, ",") != strings.Join(want, ",") {
		t.Errorf("symbols = %v, want %v", symbols, want)
	}
}

func TestScanTreeSkipsTestFilesAndVendoredTrees(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)
	writeFile(t, filepath.Join(root, "pkg", "sample_test.go"), strings.Replace(branchyFunction, "Branchy", "BranchyTest", 1))
	writeFile(t, filepath.Join(root, "vendor", "dep", "dep.go"), strings.Replace(branchyFunction, "Branchy", "Vendored", 1))
	writeFile(t, filepath.Join(root, "pkg", "testdata", "fixture.go"), strings.Replace(branchyFunction, "Branchy", "Fixture", 1))

	functions, err := scanTree(root)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}
	if len(functions) != 1 || functions[0].Path != "pkg/sample.go" {
		t.Fatalf("functions = %+v, want only pkg/sample.go", functions)
	}
}

func TestScanFileReadsEscapeHatch(t *testing.T) {
	t.Parallel()
	source := `package sample

// Justified is fine.
//complexitygate:allow generated dispatch table, decomposing hides the mapping
func Justified() {}

func Bare() {
	//complexitygate:allow
	_ = 1
}
`
	functions, err := scanFile("sample/sample.go", []byte(source))
	if err != nil {
		t.Fatalf("scanFile: %v", err)
	}
	byName := map[string]function{}
	for _, current := range functions {
		byName[current.Symbol] = current
	}
	if !byName["Justified"].Allowed || byName["Justified"].AllowBlank {
		t.Errorf("Justified = %+v, want allowed with a justification", byName["Justified"])
	}
	if !byName["Bare"].Allowed || !byName["Bare"].AllowBlank {
		t.Errorf("Bare = %+v, want allowed but flagged blank", byName["Bare"])
	}
}

func testBaseline(t *testing.T, budget int, entries map[string]int) baseline {
	t.Helper()
	return baseline{Entries: entries, RatchetBudget: budget}
}

func TestEvaluateFailsUnbaselinedFunctionAboveHardCap(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15}
	functions := []function{{Path: "cmd/goobers/init.go", Symbol: "runInit", Complexity: 57, Line: 66}}

	problems, _ := evaluate(functions, testBaseline(t, 5, map[string]int{}), limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "not in the baseline") {
		t.Fatalf("problems = %v, want an unbaselined hard-cap failure", problems)
	}
	if !strings.Contains(problems[0], "cmd/goobers/init.go:66") {
		t.Errorf("problem = %q, want the cmd/ path (the gate must not exclude cmd/)", problems[0])
	}
}

func TestEvaluateFailsBaselinedFunctionThatGrew(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15}
	functions := []function{{Path: "internal/readservice/runs.go", Symbol: "summarizeRunForStage", Complexity: 75}}
	base := testBaseline(t, 5, map[string]int{key("internal/readservice/runs.go", "summarizeRunForStage"): 74})

	problems, _ := evaluate(functions, base, limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "grew from the baselined 74 to 75") {
		t.Fatalf("problems = %v, want a growth failure", problems)
	}

	functions[0].Complexity = 74
	if problems, _ := evaluate(functions, base, limits); len(problems) != 0 {
		t.Errorf("problems = %v, want none at the baselined score", problems)
	}
}

func TestEvaluateKeysBaselineByPathSoMovingCreatesNoHeadroom(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15}
	base := testBaseline(t, 5, map[string]int{key("internal/old/file.go", "big"): 50})
	functions := []function{{Path: "internal/new/file.go", Symbol: "big", Complexity: 50}}

	problems, notes := evaluate(functions, base, limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "internal/new/file.go") {
		t.Fatalf("problems = %v, want the moved copy to fail", problems)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, "\n"), "stale baseline entry internal/old/file.go") {
		t.Errorf("notes = %v, want a stale note for the old key", notes)
	}
}

func TestEvaluateHonoursJustifiedEscapeHatch(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15}
	functions := []function{{Path: "cmd/goobers/init.go", Symbol: "runInit", Complexity: 57, Allowed: true}}

	if problems, _ := evaluate(functions, testBaseline(t, 5, map[string]int{}), limits); len(problems) != 0 {
		t.Fatalf("problems = %v, want none for a justified function", problems)
	}

	functions[0].AllowBlank = true
	problems, _ := evaluate(functions, testBaseline(t, 5, map[string]int{}), limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "needs a justification") {
		t.Fatalf("problems = %v, want a blank-justification failure", problems)
	}
}

func TestEvaluateRatchetBudget(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15}
	functions := []function{
		{Path: "a.go", Symbol: "a", Complexity: 30},
		{Path: "b.go", Symbol: "b", Complexity: 26},
	}

	problems, _ := evaluate(functions, testBaseline(t, 1, map[string]int{}), limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "over the budget of 1") {
		t.Fatalf("problems = %v, want a ratchet failure", problems)
	}

	problems, notes := evaluate(functions, testBaseline(t, 3, map[string]int{}), limits)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none under budget", problems)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "under the budget of 3") {
		t.Errorf("notes = %v, want a tightening note", notes)
	}
}

func TestParseBaselineRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"missing budget": "a.go\tfn\t41\n",
		"bad budget":     "!ratchet-budget many\n",
		"short row":      "!ratchet-budget 1\na.go\tfn\n",
		"bad score":      "!ratchet-budget 1\na.go\tfn\tzero\n",
		"duplicate":      "!ratchet-budget 1\na.go\tfn\t41\na.go\tfn\t42\n",
	} {
		if _, err := parseBaseline(strings.NewReader(content)); err == nil {
			t.Errorf("%s: parseBaseline succeeded, want an error", name)
		}
	}
}

func TestParseBaselineReadsEntriesAndBudget(t *testing.T) {
	t.Parallel()
	parsed, err := parseBaseline(strings.NewReader("# comment\n\n!ratchet-budget 177\ncmd/goobers/init.go\trunInit\t57\n"))
	if err != nil {
		t.Fatalf("parseBaseline: %v", err)
	}
	if parsed.RatchetBudget != 177 {
		t.Errorf("budget = %d, want 177", parsed.RatchetBudget)
	}
	if got := parsed.Entries[key("cmd/goobers/init.go", "runInit")]; got != 57 {
		t.Errorf("entry = %d, want 57", got)
	}
}

func TestRunUpdateThenEnforceRoundTrips(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)
	baselinePath := filepath.Join("test", "complexitygate", "baseline.txt")
	writeFile(t, filepath.Join(root, baselinePath), "")

	var stdout, stderr bytes.Buffer
	args := []string{"-root", root, "-hard", "5", "-ratchet", "4", "-report", "3"}
	if code := run(append(args, "-update"), &stdout, &stderr); code != 0 {
		t.Fatalf("update exit = %d, stderr = %s", code, stderr.String())
	}
	written, err := os.ReadFile(filepath.Join(root, baselinePath))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if !strings.Contains(string(written), "pkg/sample.go\tBranchy\t6") {
		t.Fatalf("baseline = %q, want the scanned entry", written)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("enforce exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "baselined") {
		t.Errorf("stdout = %q, want the tier summary", stdout.String())
	}
}

func TestRunFailsWhenBaselineIsMissing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-root", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "read baseline") {
		t.Errorf("stderr = %q, want a baseline read failure", stderr.String())
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

// TestRepositoryBaselineIsCurrent keeps the committed baseline honest: it must
// parse, and every entry must still name a function in the tree.
func TestRepositoryBaselineIsCurrent(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	base, err := readBaseline(filepath.Join(root, defaultBaselinePath))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if base.RatchetBudget <= 0 {
		t.Fatalf("ratchet budget = %d, want a positive pinned value", base.RatchetBudget)
	}
	functions, err := scanTree(root)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}
	present := make(map[string]bool, len(functions))
	commands := 0
	for _, current := range functions {
		present[key(current.Path, current.Symbol)] = true
		if current.Complexity >= defaultHardCap && strings.HasPrefix(current.Path, "cmd/") {
			commands++
		}
	}
	for entryKey := range base.Entries {
		if !present[entryKey] {
			path, symbol, _ := strings.Cut(entryKey, "\t")
			t.Errorf("baseline entry %s %s no longer exists; run `make complexity-update`", path, symbol)
		}
	}
	if commands == 0 {
		t.Error("no cmd/ function is over the hard cap; the gate must not be excluding cmd/")
	}
}
