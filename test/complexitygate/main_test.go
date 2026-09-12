package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
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

func gitCommand(t *testing.T, root string, args ...string) {
	t.Helper()
	command := testgit.Command(append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func initGitRepository(t *testing.T, root string) {
	t.Helper()
	gitCommand(t, root, "init", "--quiet")
}

func commitAll(t *testing.T, root string) {
	t.Helper()
	gitCommand(t, root, "add", ".")
	gitCommand(t, root, "-c", "user.name=Complexity Gate Test", "-c", "user.email=complexity@example.invalid", "commit", "--quiet", "-m", "fixture")
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

const longSequentialFunction = `package sample

func NewOversized() int {
	value := 1
	value++
	value++
	value++
	value++
	value++
	value++
	value++
	return value
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
	if got, want := functions[0].BodyLines, 15; got != want {
		t.Errorf("body lines = %d, want %d", got, want)
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
	return baseline{
		Entries: entries, EntryJustifications: make(map[string]justification), RatchetBudget: budget,
		BodyLengths: make(map[string]int), BodyJustifications: make(map[string]justification), BodyLengthCap: -1,
	}
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

func TestEvaluateEnforcesAllowedBaselinedFunctionAndStaleSemantics(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15}
	entryKey := key("internal/executor/shell.go", "(*ShellExecutor).Run")
	base := testBaseline(t, 1, map[string]int{entryKey: 87})
	functions := []function{{
		Path: "internal/executor/shell.go", Symbol: "(*ShellExecutor).Run",
		Complexity: 90, Allowed: true,
	}}

	problems, notes := evaluate(functions, base, limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "grew from the baselined 87 to 90") {
		t.Fatalf("problems = %v, want allowed+baselined growth failure", problems)
	}
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want no stale note while the allowed function remains above the cap", notes)
	}

	functions[0].Complexity = 39
	problems, notes = evaluate(functions, base, limits)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none after tightening below the cap", problems)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "stale baseline entry internal/executor/shell.go (*ShellExecutor).Run") {
		t.Fatalf("notes = %v, want the ordinary stale-entry note", notes)
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

func TestEvaluateBodyLengthHasNoAllowExemption(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15, body: 200}
	entryKey := key("cmd/goobers/up.go", "runUpContextWithForce")
	base := testBaseline(t, 0, map[string]int{})
	base.BodyLengthCap = 200
	functions := []function{{
		Path: "cmd/goobers/up.go", Symbol: "runUpContextWithForce", Line: 320,
		BodyLines: 200, Allowed: true,
	}}

	problems, _ := evaluate(functions, base, limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "body length 200") || !strings.Contains(problems[0], "not in the baseline") {
		t.Fatalf("problems = %v, want unbaselined body-length failure despite allow directive", problems)
	}

	base.BodyLengths[entryKey] = 1560
	functions[0].BodyLines = 1561
	problems, _ = evaluate(functions, base, limits)
	if len(problems) != 1 || !strings.Contains(problems[0], "grew from the baselined 1560 to 1561") {
		t.Fatalf("problems = %v, want body-length growth failure", problems)
	}
}

func TestParseBaselineRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"missing budget":                "a.go\tfn\t41\n",
		"bad budget":                    "!ratchet-budget many\n",
		"short row":                     "!ratchet-budget 1\na.go\tfn\n",
		"bad score":                     "!ratchet-budget 1\na.go\tfn\tzero\n",
		"duplicate":                     "!ratchet-budget 1\na.go\tfn\t41\na.go\tfn\t42\n",
		"duplicate ratchet budget":      "!ratchet-budget 1\n!ratchet-budget 2\n",
		"blank entry justification":     "!ratchet-budget 1\n!entry-justification\ta.go\tfn\t42\t\n",
		"blank budget justification":    "!ratchet-budget-justification\t2\t\n!ratchet-budget 1\n",
		"duplicate entry justification": "!ratchet-budget 1\n!entry-justification\ta.go\tfn\t42\tone\n!entry-justification\ta.go\tfn\t42\ttwo\n",
		"duplicate body-length cap":     "!ratchet-budget 1\n!body-length-cap 200\n!body-length-cap 201\n",
		"duplicate body-length entry":   "!ratchet-budget 1\n!body-length\ta.go\tfn\t200\n!body-length\ta.go\tfn\t201\n",
		"blank body justification":      "!ratchet-budget 1\n!body-length-justification\ta.go\tfn\t201\t\n",
	} {
		if _, err := parseBaseline(strings.NewReader(content)); err == nil {
			t.Errorf("%s: parseBaseline succeeded, want an error", name)
		}
	}
}

func TestParseBaselineReadsEntriesAndBudget(t *testing.T) {
	t.Parallel()
	parsed, err := parseBaseline(strings.NewReader("# comment\n\n!ratchet-budget-justification\t177\tnew command family\n!ratchet-budget 177\n!entry-justification\tcmd/goobers/init.go\trunInit\t57\tlegacy generated form\ncmd/goobers/init.go\trunInit\t57\n"))
	if err != nil {
		t.Fatalf("parseBaseline: %v", err)
	}
	if parsed.RatchetBudget != 177 {
		t.Errorf("budget = %d, want 177", parsed.RatchetBudget)
	}
	if got := parsed.Entries[key("cmd/goobers/init.go", "runInit")]; got != 57 {
		t.Errorf("entry = %d, want 57", got)
	}
	if parsed.RatchetJustification == nil || parsed.RatchetJustification.Reason != "new command family" {
		t.Errorf("ratchet justification = %+v, want parsed reason", parsed.RatchetJustification)
	}
	if got := parsed.EntryJustifications[key("cmd/goobers/init.go", "runInit")]; got.Target != 57 || got.Reason != "legacy generated form" {
		t.Errorf("entry justification = %+v, want target and reason", got)
	}
}

func TestValidateBaselineUpdateRequiresExactTargetJustifications(t *testing.T) {
	t.Parallel()
	entryKey := key("pkg/sample.go", "Branchy")
	current := testBaseline(t, 1, map[string]int{entryKey: 5})
	next := testBaseline(t, 2, map[string]int{entryKey: 6})
	current.BodyLengthCap, next.BodyLengthCap = 200, 200
	current.BodyLengths[entryKey] = 200
	next.BodyLengths[entryKey] = 201

	err := validateBaselineUpdate(&current, next)
	if err == nil || !strings.Contains(err.Error(), "would grow from 5 to 6") || !strings.Contains(err.Error(), "budget would grow from 1 to 2") || !strings.Contains(err.Error(), "body length would grow from 200 to 201") {
		t.Fatalf("validateBaselineUpdate error = %v, want score, budget, and body-length justification failures", err)
	}

	next.EntryJustifications[entryKey] = justification{Target: 7, Reason: "wrong target"}
	next.RatchetJustification = &justification{Target: 3, Reason: "wrong target"}
	if err := validateBaselineUpdate(&current, next); err == nil {
		t.Fatal("validateBaselineUpdate accepted justifications for different targets")
	}

	next.EntryJustifications[entryKey] = justification{Target: 6, Reason: "generated switch gained a required case"}
	next.RatchetJustification = &justification{Target: 2, Reason: "new command remains above the ratchet"}
	next.BodyJustifications[entryKey] = justification{Target: 201, Reason: "startup sequencing requires one more cleanup block"}
	if err := validateBaselineUpdate(&current, next); err != nil {
		t.Fatalf("validateBaselineUpdate rejected exact-target justifications: %v", err)
	}
}

func TestBaselineForFunctionsDoesNotAdmitNewOversizedFunction(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15, body: 200}
	existingKey := key("existing.go", "Existing")
	previous := testBaseline(t, 0, map[string]int{})
	previous.BodyLengthCap = 200
	previous.BodyLengths[existingKey] = 220
	functions := []function{
		{Path: "existing.go", Symbol: "Existing", BodyLines: 210},
		{Path: "new.go", Symbol: "New", BodyLines: 250, Allowed: true},
	}

	next := baselineForFunctions(functions, limits, &previous, &previous)
	if got := next.BodyLengths[existingKey]; got != 210 {
		t.Fatalf("existing body-length baseline = %d, want tightened 210", got)
	}
	if _, exists := next.BodyLengths[key("new.go", "New")]; exists {
		t.Fatal("updater admitted a new oversized function into the body-length baseline")
	}
}

func TestBaselineForFunctionsKeepsRecordedAllowedFunction(t *testing.T) {
	t.Parallel()
	limits := thresholds{hardCap: 40, ratchet: 25, report: 15}
	recordedKey := key("internal/executor/shell.go", "(*ShellExecutor).Run")
	current := testBaseline(t, 1, map[string]int{recordedKey: 90})
	current.EntryJustifications[recordedKey] = justification{Target: 90, Reason: "explicit legacy exception"}
	functions := []function{
		{Path: "internal/executor/shell.go", Symbol: "(*ShellExecutor).Run", Complexity: 90, Allowed: true},
		{Path: "generated.go", Symbol: "Generated", Complexity: 50, Allowed: true},
	}

	next := baselineForFunctions(functions, limits, &current, &current)
	if got := next.Entries[recordedKey]; got != 90 {
		t.Fatalf("recorded allowed score = %d, want 90", got)
	}
	if _, exists := next.Entries[key("generated.go", "Generated")]; exists {
		t.Fatal("unbaselined allowed function was added to the baseline")
	}
	if got := next.EntryJustifications[recordedKey]; got.Reason != "explicit legacy exception" {
		t.Fatalf("recorded justification = %+v, want it preserved", got)
	}
}

func TestReviewWindowBaselineTransitions(t *testing.T) {
	t.Parallel()
	type fixtureBaseline struct {
		RatchetBudget int            `json:"ratchetBudget"`
		Entries       map[string]int `json:"entries"`
	}
	type transition struct {
		Commit    string           `json:"commit"`
		Pattern   string           `json:"pattern"`
		WantError bool             `json:"wantError"`
		Before    *fixtureBaseline `json:"before"`
		After     fixtureBaseline  `json:"after"`
	}
	data, err := os.ReadFile(filepath.Join("testdata", "review-window-transitions.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures []transition
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	if len(fixtures) != 8 {
		t.Fatalf("fixtures = %d, want all 8 review-window baseline commits", len(fixtures))
	}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Commit+"/"+fixture.Pattern, func(t *testing.T) {
			var before *baseline
			if fixture.Before != nil {
				parsed := testBaseline(t, fixture.Before.RatchetBudget, fixture.Before.Entries)
				before = &parsed
			}
			after := testBaseline(t, fixture.After.RatchetBudget, fixture.After.Entries)
			err := validateBaselineUpdate(before, after)
			if (err != nil) != fixture.WantError {
				t.Fatalf("validateBaselineUpdate error = %v, wantError=%t", err, fixture.WantError)
			}
		})
	}
}

func TestRunUpdateThenEnforceRoundTrips(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initGitRepository(t, root)
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)
	baselinePath := filepath.Join("test", "complexitygate", "baseline.txt")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, baselinePath)), 0o755); err != nil {
		t.Fatalf("mkdir baseline directory: %v", err)
	}

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
	if !strings.Contains(stdout.String(), "body lines") {
		t.Errorf("stdout = %q, want body-length tier summary", stdout.String())
	}
}

func TestRunUpdateRefusesGrowthUntilBaselineCarriesJustification(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initGitRepository(t, root)
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)
	baselinePath := filepath.Join(root, defaultBaselinePath)
	writeFile(t, baselinePath, "!ratchet-budget 1\npkg/sample.go\tBranchy\t5\n")
	commitAll(t, root)
	args := []string{"-root", root, "-hard", "5", "-ratchet", "4", "-report", "3", "-update"}

	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("unjustified update exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "would grow from 5 to 6") {
		t.Fatalf("stderr = %q, want score-growth refusal", stderr.String())
	}

	writeFile(t, baselinePath, "!ratchet-budget 1\n!entry-justification\tpkg/sample.go\tBranchy\t6\tbranch table gained a required case\npkg/sample.go\tBranchy\t5\n")
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("justified update exit = %d, stderr=%q", code, stderr.String())
	}
	written, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read updated baseline: %v", err)
	}
	for _, want := range []string{
		"!entry-justification\tpkg/sample.go\tBranchy\t6\tbranch table gained a required case",
		"pkg/sample.go\tBranchy\t6",
	} {
		if !strings.Contains(string(written), want) {
			t.Fatalf("updated baseline = %q, want %q", written, want)
		}
	}
}

func TestRunUpdateRejectsPreEditedScoreWithoutJustification(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initGitRepository(t, root)
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)
	baselinePath := filepath.Join(root, defaultBaselinePath)
	writeFile(t, baselinePath, "!ratchet-budget 1\npkg/sample.go\tBranchy\t5\n")
	commitAll(t, root)

	writeFile(t, baselinePath, "!ratchet-budget 1\npkg/sample.go\tBranchy\t6\n")
	var stdout, stderr bytes.Buffer
	args := []string{"-root", root, "-hard", "5", "-ratchet", "4", "-report", "3", "-update"}
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("pre-edited score update exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "would grow from 5 to 6") {
		t.Fatalf("stderr = %q, want score-growth refusal against HEAD", stderr.String())
	}
}

func TestRunUpdateRejectsPreEditedBudgetWithoutJustification(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initGitRepository(t, root)
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)
	baselinePath := filepath.Join(root, defaultBaselinePath)
	writeFile(t, baselinePath, "!ratchet-budget 0\n")
	commitAll(t, root)

	writeFile(t, baselinePath, "!ratchet-budget 1\n")
	var stdout, stderr bytes.Buffer
	args := []string{"-root", root, "-hard", "7", "-ratchet", "4", "-report", "3", "-update"}
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("pre-edited budget update exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "budget would grow from 0 to 1") {
		t.Fatalf("stderr = %q, want budget-growth refusal against HEAD", stderr.String())
	}
}

func TestRunUpdateCannotBaselineNewOversizedFunction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initGitRepository(t, root)
	writeFile(t, filepath.Join(root, "pkg", "sample.go"), branchyFunction)
	baselinePath := filepath.Join(root, defaultBaselinePath)
	writeFile(t, baselinePath, "!ratchet-budget 0\n!body-length-cap 10\n!body-length\tpkg/sample.go\tBranchy\t15\n")
	commitAll(t, root)
	writeFile(t, filepath.Join(root, "pkg", "new.go"), longSequentialFunction)

	args := []string{"-root", root, "-hard", "100", "-ratchet", "99", "-report", "98", "-body-length", "10"}
	var stdout, stderr bytes.Buffer
	if code := run(append(args, "-update"), &stdout, &stderr); code != 0 {
		t.Fatalf("update exit = %d, stderr=%q", code, stderr.String())
	}
	written, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if strings.Contains(string(written), "NewOversized") {
		t.Fatalf("updater admitted new oversized function: %s", written)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("enforce exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "NewOversized") || !strings.Contains(stderr.String(), "not in the baseline") {
		t.Fatalf("stderr=%q, want new oversized-function refusal", stderr.String())
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
	if base.BodyLengthCap != defaultBodyLength {
		t.Fatalf("body-length cap = %d, want fixed default %d", base.BodyLengthCap, defaultBodyLength)
	}
	functions, err := scanTree(root)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}
	present := make(map[string]bool, len(functions))
	measuredBodyLengths := make(map[string]int, len(functions))
	commands := 0
	for _, current := range functions {
		present[key(current.Path, current.Symbol)] = true
		measuredBodyLengths[key(current.Path, current.Symbol)] = current.BodyLines
		if current.Complexity >= defaultHardCap && strings.HasPrefix(current.Path, "cmd/") {
			commands++
		}
	}
	for entryKey, lines := range base.BodyLengths {
		measured, exists := measuredBodyLengths[entryKey]
		if !exists || measured > lines || lines < base.BodyLengthCap {
			t.Errorf("body-length baseline %q = %d, measured %d, exists=%t", entryKey, lines, measured, exists)
		}
	}
	for entryKey, measured := range measuredBodyLengths {
		if measured < base.BodyLengthCap {
			continue
		}
		if _, exists := base.BodyLengths[entryKey]; !exists {
			t.Errorf("oversized function %q at %d lines is missing from body-length baseline", entryKey, measured)
		}
	}
	upKey := key("cmd/goobers/up.go", "runUpContextWithForce")
	if base.BodyLengths[upKey] == 0 {
		t.Error("runUpContextWithForce must remain visible in the body-length baseline")
	}
	for entryKey := range base.Entries {
		if !present[entryKey] {
			path, symbol, _ := strings.Cut(entryKey, "\t")
			t.Errorf("baseline entry %s %s no longer exists; run `make complexity-update`", path, symbol)
		}
	}
	for entryKey, reason := range base.EntryJustifications {
		score, exists := base.Entries[entryKey]
		if !exists || reason.Target != score {
			t.Errorf("entry justification %q targets %d, want an existing baseline entry at that exact score", entryKey, reason.Target)
		}
	}
	if reason := base.RatchetJustification; reason != nil && reason.Target != base.RatchetBudget {
		t.Errorf("ratchet justification targets %d, want current budget %d", reason.Target, base.RatchetBudget)
	}
	if commands == 0 {
		t.Error("no cmd/ function is over the hard cap; the gate must not be excluding cmd/")
	}
}
