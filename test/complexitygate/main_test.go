package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGoFile(t *testing.T, root, name, source string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// complexSource renders a function whose cyclomatic complexity is 1+branches.
func complexSource(pkg, name string, branches int) string {
	var builder strings.Builder
	builder.WriteString("package " + pkg + "\n\nfunc " + name + "(n int) int {\n")
	for i := 0; i < branches; i++ {
		builder.WriteString("\tif n > 0 {\n\t\tn--\n\t}\n")
	}
	builder.WriteString("\treturn n\n}\n")
	return builder.String()
}

func TestComplexityCountsBranchPoints(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoFile(t, root, "sample.go", `package sample

func branchy(items []int, flag bool) int {
	total := 0
	for _, item := range items {
		if item > 0 && flag {
			total += item
		}
	}
	switch total {
	case 1, 2:
		total++
	default:
		total--
	}
	for total > 100 {
		total /= 2
	}
	if flag || total == 0 {
		return 0
	}
	return total
}
`)
	functions, err := scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(functions) != 1 {
		t.Fatalf("functions = %d, want 1", len(functions))
	}
	// 1 base + range + if + && + case + for + if + || = 8.
	if functions[0].complexity != 8 {
		t.Fatalf("complexity = %d, want 8", functions[0].complexity)
	}
	if functions[0].key() != "sample.go:branchy" {
		t.Fatalf("key = %q", functions[0].key())
	}
}

func TestScanSkipsTestFilesTestdataAndVendor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoFile(t, root, "prod.go", "package prod\n\nfunc Kept() {}\n")
	writeGoFile(t, root, "prod_test.go", "package prod\n\nfunc TestSkipped(t *testing.T) {}\n")
	writeGoFile(t, root, "testdata/fixture.go", "package fixture\n\nfunc Skipped() {}\n")
	writeGoFile(t, root, "vendor/dep/dep.go", "package dep\n\nfunc Skipped() {}\n")
	writeGoFile(t, root, "node_modules/pkg/pkg.go", "package pkg\n\nfunc Skipped() {}\n")

	functions, err := scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(functions) != 1 || functions[0].symbol != "Kept" {
		t.Fatalf("functions = %+v, want only Kept", functions)
	}
}

func TestScanKeepsCommandFunctions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoFile(t, root, "cmd/goobers/main.go", "package main\n\nfunc run() {}\n")

	functions, err := scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(functions) != 1 || functions[0].key() != "cmd/goobers/main.go:run" {
		t.Fatalf("functions = %+v, want the cmd/ function", functions)
	}
}

func TestSymbolNameDistinguishesMethodReceivers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoFile(t, root, "sample.go", `package sample

type a struct{}

type b[T any] struct{}

func (a) Run() {}

func (r *b[T]) Run() {}
`)
	functions, err := scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var keys []string
	for _, current := range functions {
		keys = append(keys, current.key())
	}
	want := "sample.go:(a).Run sample.go:(*b).Run"
	if strings.Join(keys, " ") != want {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
}

func TestEvaluateFailsNewFunctionAboveHardCap(t *testing.T) {
	t.Parallel()
	functions := []function{{path: "internal/a/a.go", symbol: "grown", line: 12, complexity: hardCap}}
	base := baseline{softBudget: 10, entries: map[string]int{}}

	violations, _ := evaluate(functions, base)
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want exactly one", violations)
	}
	if !strings.Contains(violations[0], "internal/a/a.go:12: grown has cyclomatic complexity 40") {
		t.Fatalf("violation = %q", violations[0])
	}
}

func TestEvaluateAllowsBaselinedFunction(t *testing.T) {
	t.Parallel()
	functions := []function{{path: "internal/a/a.go", symbol: "old", complexity: 57}}
	base := baseline{softBudget: 10, entries: map[string]int{"internal/a/a.go:old": 57}}

	violations, _ := evaluate(functions, base)
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
}

// A baseline keyed by path+symbol must not hand a moved function free headroom.
func TestEvaluateFailsBaselinedFunctionMovedToAnotherFile(t *testing.T) {
	t.Parallel()
	functions := []function{{path: "internal/b/b.go", symbol: "old", complexity: 57}}
	base := baseline{softBudget: 10, entries: map[string]int{"internal/a/a.go:old": 57}}

	violations, advisories := evaluate(functions, base)
	if len(violations) != 1 || !strings.Contains(violations[0], "internal/b/b.go") {
		t.Fatalf("violations = %v, want the moved function to fail", violations)
	}
	if !containsSubstring(advisories, "stale baseline entry") {
		t.Fatalf("advisories = %v, want a stale-entry advisory", advisories)
	}
}

func TestEvaluateReportsGrowthOfBaselinedFunction(t *testing.T) {
	t.Parallel()
	functions := []function{{path: "internal/a/a.go", symbol: "old", complexity: 70}}
	base := baseline{softBudget: 10, entries: map[string]int{"internal/a/a.go:old": 57}}

	violations, advisories := evaluate(functions, base)
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
	if !containsSubstring(advisories, "grew from cc 57 to cc 70") {
		t.Fatalf("advisories = %v, want a growth advisory", advisories)
	}
}

func TestEvaluateHonorsJustifiedEscapeHatch(t *testing.T) {
	t.Parallel()
	functions := []function{{
		path:          "internal/a/a.go",
		symbol:        "generated",
		complexity:    45,
		allowed:       true,
		justification: "one switch arm per protocol opcode",
	}}
	base := baseline{softBudget: 10, entries: map[string]int{}}

	violations, advisories := evaluate(functions, base)
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
	if !containsSubstring(advisories, "one switch arm per protocol opcode") {
		t.Fatalf("advisories = %v, want the justification echoed", advisories)
	}
}

func TestEvaluateRejectsEscapeHatchWithoutJustification(t *testing.T) {
	t.Parallel()
	functions := []function{{path: "internal/a/a.go", symbol: "bare", complexity: 45, allowed: true}}
	base := baseline{softBudget: 10, entries: map[string]int{}}

	violations, _ := evaluate(functions, base)
	if len(violations) != 1 || !strings.Contains(violations[0], "requires a justification") {
		t.Fatalf("violations = %v, want a missing-justification failure", violations)
	}
}

func TestEvaluateFailsWhenSoftBudgetIsExceeded(t *testing.T) {
	t.Parallel()
	functions := []function{
		{path: "a.go", symbol: "one", complexity: softCap},
		{path: "b.go", symbol: "two", complexity: softCap},
	}
	base := baseline{softBudget: 1, entries: map[string]int{}}

	violations, _ := evaluate(functions, base)
	if len(violations) != 1 || !strings.Contains(violations[0], "over the soft budget of 1") {
		t.Fatalf("violations = %v, want a soft-budget failure", violations)
	}
}

func TestEvaluateReportsSlackAndReportOnlyTier(t *testing.T) {
	t.Parallel()
	functions := []function{
		{path: "a.go", symbol: "one", complexity: softCap},
		{path: "b.go", symbol: "two", complexity: reportCap},
		{path: "c.go", symbol: "three", complexity: reportCap - 1},
	}
	base := baseline{softBudget: 3, entries: map[string]int{}}

	violations, advisories := evaluate(functions, base)
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
	if !containsSubstring(advisories, "lower soft-budget to 1") {
		t.Fatalf("advisories = %v, want a ratchet hint", advisories)
	}
	if !containsSubstring(advisories, "2 functions at or above cc 15 (report-only)") {
		t.Fatalf("advisories = %v, want the report-only count", advisories)
	}
}

func TestEscapeHatchIsReadFromDocAndInlineComments(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoFile(t, root, "sample.go", `package sample

// documented does a lot.
//complexitygate:allow generated dispatch table
func documented() {}

func inline() {} //complexitygate:allow one arm per opcode

//complexitygate:allow
func bare() {}

func plain() {}
`)
	functions, err := scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := map[string]function{}
	for _, current := range functions {
		got[current.symbol] = current
	}
	if want := "generated dispatch table"; got["documented"].justification != want {
		t.Fatalf("documented justification = %q, want %q", got["documented"].justification, want)
	}
	if want := "one arm per opcode"; got["inline"].justification != want {
		t.Fatalf("inline justification = %q, want %q", got["inline"].justification, want)
	}
	if !got["bare"].allowed || got["bare"].justification != "" {
		t.Fatalf("bare = %+v, want an allowed directive with no justification", got["bare"])
	}
	if got["plain"].allowed {
		t.Fatalf("plain = %+v, want no directive", got["plain"])
	}
}

func TestParseBaselineRoundTripsRenderedOutput(t *testing.T) {
	t.Parallel()
	functions := []function{
		{path: "a.go", symbol: "big", complexity: hardCap + 2},
		{path: "b.go", symbol: "medium", complexity: softCap},
		{path: "c.go", symbol: "hatched", complexity: hardCap + 5, allowed: true, justification: "why"},
	}
	parsed, err := parseBaseline(strings.NewReader(renderBaseline(functions)))
	if err != nil {
		t.Fatalf("parseBaseline: %v", err)
	}
	if parsed.softBudget != 3 {
		t.Fatalf("softBudget = %d, want 3", parsed.softBudget)
	}
	if len(parsed.entries) != 1 || parsed.entries["a.go:big"] != hardCap+2 {
		t.Fatalf("entries = %v, want only the un-hatched capped function", parsed.entries)
	}
}

func TestParseBaselineRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing budget":   "a.go:one 41\n",
		"duplicate budget": "soft-budget 1\nsoft-budget 2\n",
		"duplicate entry":  "soft-budget 1\na.go:one 41\na.go:one 42\n",
		"unkeyed entry":    "soft-budget 1\nplainname 41\n",
		"not a number":     "soft-budget 1\na.go:one many\n",
		"wrong arity":      "soft-budget 1\na.go:one 41 extra\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseBaseline(strings.NewReader(content)); err == nil {
				t.Fatal("parseBaseline: want an error")
			}
		})
	}
}

func TestRunWritesAndThenPassesAgainstItsOwnBaseline(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoFile(t, root, "test/complexitygate/keep.go", "package main\n")
	writeGoFile(t, root, "internal/a/a.go", complexSource("a", "big", hardCap))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-write", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("run -write = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, stderr.String())
	}

	writeGoFile(t, root, "internal/b/b.go", complexSource("b", "fresh", hardCap))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{root}, &stdout, &stderr); code != 1 {
		t.Fatalf("run after adding a complex function = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "internal/b/b.go") {
		t.Fatalf("stderr = %q, want the new function named", stderr.String())
	}
}

func TestRunReportsMissingBaseline(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "read baseline") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRejectsExtraArguments(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"one", "two"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
