package nomination

import (
	"sort"
	"strings"
	"testing"
)

// sortedFindings returns every parsed finding in a stable order.
func sortedFindings(f *Findings) []Finding {
	out := make([]Finding, 0, len(f.byKey))
	for _, finding := range f.byKey {
		out = append(out, finding)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// signalsStdout is the shape collect-repo-signals prints: the JSON index,
// then each tool's raw output under its fixed header (go vet text,
// golangci-lint JSON followed by the linter's stderr, test2json fail lines,
// and the trailing sections the parser ignores).
const signalsStdout = `=== repo-signals ===
{"schema":"goobers.dev/repo-signals/v1","head":"abc","signalCount":3,"signals":[]}
=== go vet (exit 1) ===
# github.com/goobers/goobers/internal/worktree
internal/worktree/manager.go:88:2: result of (*os.File).Close call not used
./internal/worktree/manager.go:91:15: fmt.Sprintf format %d has arg "s" of wrong type string
vet: some unrelated line without a position

=== golangci-lint (exit 1) ===
{"Issues":[{"FromLinter":"errcheck","Text":"Error return value of ` + "`f.Close`" + ` is not checked","Severity":"","SourceLines":["\tf.Close()"],"Pos":{"Filename":"internal/worktree/manager.go","Offset":65,"Line":88,"Column":9},"ExpectNoLint":false,"ExpectedNoLintLinter":""},{"FromLinter":"govet","Text":"printf: bad","Pos":{"Filename":"./internal/runner/run.go","Line":12,"Column":1}}],"Report":{"Linters":[{"Name":"errcheck","Enabled":true}]}}
level=warning msg="the linter's own stderr follows the JSON"
=== go test failures (exit 1) ===
{"Time":"2026-08-29T07:08:45Z","Action":"fail","Package":"github.com/goobers/goobers/internal/runner","Test":"TestRunnerRace","Elapsed":0.2}
{"Time":"2026-08-29T07:08:45Z","Action":"fail","Package":"github.com/goobers/goobers/internal/runner","Elapsed":0.3}
{"Time":"2026-08-29T07:08:46Z","Action":"fail","Package":"github.com/goobers/goobers/internal/x","Elapsed":0,"FailedBuild":"github.com/goobers/goobers/internal/x [github.com/goobers/goobers/internal/x.test]"}
{"Time":"2026-08-29T07:08:46Z","Action":"pass","Package":"github.com/goobers/goobers/internal/y","Test":"TestPasses","Elapsed":0}
=== go test stderr ===
{"Action":"fail","Package":"github.com/goobers/goobers/internal/ignored","Test":"TestInAnIgnoredSection"}
=== go test output of failing tests ===
--- github.com/goobers/goobers/internal/runner TestRunnerRace ---
    run_test.go:41: expected 3 got 2
`

func TestParseSignalsReadsEveryToolSection(t *testing.T) {
	f := ParseSignals([]byte(signalsStdout))
	if len(f.Problems) != 0 {
		t.Fatalf("problems = %v", f.Problems)
	}
	want := []Finding{
		{Tool: ToolLint, Rule: "errcheck", Path: "internal/worktree/manager.go", Line: 88},
		{Tool: ToolLint, Rule: "govet", Path: "internal/runner/run.go", Line: 12},
		{Tool: ToolTest, Package: "github.com/goobers/goobers/internal/runner", Test: "TestRunnerRace"},
		{Tool: ToolVet, Rule: "fmt.Sprintf format %d has arg \"s\" of wrong type string", Path: "internal/worktree/manager.go", Line: 91},
		{Tool: ToolVet, Rule: "result of (*os.File).Close call not used", Path: "internal/worktree/manager.go", Line: 88},
	}
	got := sortedFindings(f)
	if len(got) != len(want) {
		t.Fatalf("findings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if f.Counts[ToolVet] != 2 || f.Counts[ToolLint] != 2 || f.Counts[ToolTest] != 1 {
		t.Fatalf("counts = %v", f.Counts)
	}
}

// TestMatchIsByteForByte pins that a finding pointer matches only the exact
// tool record: every field participates, an off-by-one line, a different
// rule text, a sibling test, or a kind other than finding is no match.
func TestMatchIsByteForByte(t *testing.T) {
	f := ParseSignals([]byte(signalsStdout))
	vet := Evidence{Kind: EvidenceFinding, Tool: ToolVet, Path: "internal/worktree/manager.go", Line: 88, Rule: "result of (*os.File).Close call not used"}
	lint := Evidence{Kind: EvidenceFinding, Tool: ToolLint, Path: "internal/worktree/manager.go", Line: 88, Rule: "errcheck"}
	test := Evidence{Kind: EvidenceFinding, Tool: ToolTest, Package: "github.com/goobers/goobers/internal/runner", Test: "TestRunnerRace"}
	for name, e := range map[string]Evidence{"vet": vet, "lint": lint, "test": test} {
		if _, ok := f.Match(e); !ok {
			t.Errorf("%s pointer copied from the tool output did not match", name)
		}
	}
	mutate := func(e Evidence, fn func(*Evidence)) Evidence { fn(&e); return e }
	for name, e := range map[string]Evidence{
		"vet line off by one":             mutate(vet, func(e *Evidence) { e.Line = 89 }),
		"vet rule paraphrased":            mutate(vet, func(e *Evidence) { e.Rule = "result of Close is not used" }),
		"vet rule with trailing space":    mutate(vet, func(e *Evidence) { e.Rule += " " }),
		"vet path with ./":                mutate(vet, func(e *Evidence) { e.Path = "./" + e.Path }),
		"vet claimed as lint":             mutate(vet, func(e *Evidence) { e.Tool = ToolLint }),
		"lint rule is the text":           mutate(lint, func(e *Evidence) { e.Rule = "Error return value of `f.Close` is not checked" }),
		"lint other file":                 mutate(lint, func(e *Evidence) { e.Path = "internal/worktree/manager_test.go" }),
		"test sibling":                    mutate(test, func(e *Evidence) { e.Test = "TestRunnerRaces" }),
		"test other package":              mutate(test, func(e *Evidence) { e.Package = "github.com/goobers/goobers/internal/x" }),
		"package-level fail":              {Kind: EvidenceFinding, Tool: ToolTest, Package: "github.com/goobers/goobers/internal/runner"},
		"failed build":                    {Kind: EvidenceFinding, Tool: ToolTest, Package: "github.com/goobers/goobers/internal/x"},
		"fail line in an ignored section": {Kind: EvidenceFinding, Tool: ToolTest, Package: "github.com/goobers/goobers/internal/ignored", Test: "TestInAnIgnoredSection"},
		"source pointer at the same spot": {Kind: EvidenceSource, Path: "internal/worktree/manager.go", Line: 88},
		"artifact pointer":                {Kind: EvidenceArtifact, Path: "internal/worktree/manager.go", Digest: "sha256:" + strings.Repeat("0", 64)},
	} {
		if got, ok := f.Match(e); ok {
			t.Errorf("%s matched %+v", name, got)
		}
	}
	var none *Findings
	if _, ok := none.Match(vet); ok {
		t.Fatal("a nil finding set matched")
	}
}

func TestParseSignalsReportsUnreadableSections(t *testing.T) {
	truncated := strings.Replace(signalsStdout, `"Report":{"Linters":[{"Name":"errcheck","Enabled":true}]}}`, `"Report":{"Lin`, 1)
	f := ParseSignals([]byte(truncated))
	if f.Counts[ToolLint] != 0 {
		t.Fatalf("a truncated lint document yielded %d lint findings; want none (a problem, not a partial set)", f.Counts[ToolLint])
	}
	if f.Counts[ToolVet] != 2 || f.Counts[ToolTest] != 1 {
		t.Fatalf("the other sections were lost: %v", f.Counts)
	}
	if len(f.Problems) != 1 || !strings.Contains(f.Problems[0], "golangci-lint: JSON document is not parseable") {
		t.Fatalf("problems = %v", f.Problems)
	}

	empty := ParseSignals([]byte("=== go vet (exit 0) ===\n\n=== golangci-lint (exit 0) ===\n{\"Issues\":[]}\n\n=== go test failures (exit 0) ===\n\n"))
	if len(empty.byKey) != 0 || len(empty.Problems) != 0 {
		t.Fatalf("clean tree: findings = %d, problems = %v", len(empty.byKey), empty.Problems)
	}
	if len(ParseSignals(nil).byKey) != 0 {
		t.Fatal("empty stdout yielded findings")
	}
}

// TestApprovalBoundsOnPaths pins the fix-surface and load-bearing helpers
// the approval decision is built from.
func TestApprovalBoundsOnPaths(t *testing.T) {
	for p, want := range map[string]bool{
		"api/v1alpha1/workflow_types.go": true,
		"api/schemas/workflow.json":      true,
		"docs/design/foo.md":             true,
		".github/workflows/ci.yml":       true,
		"deploy/base/x.yaml":             true,
		"providers/model.go":             true,
		"internal/journal/reader.go":     true,
		"internal/journal":               true,
		"providers/model_test.go":        false,
		"providers/github.go":            false,
		"internal/journalx/a.go":         false,
		"apis/x.go":                      false,
		"internal/worktree/manager.go":   false,
	} {
		if got := touchesLoadBearing(p); got != want {
			t.Errorf("touchesLoadBearing(%q) = %v, want %v", p, got, want)
		}
	}
	for pkg, want := range map[string]bool{
		"github.com/goobers/goobers/internal/journal":      true,
		"github.com/goobers/goobers/internal/journal/sub":  true,
		"github.com/goobers/goobers/api/v1alpha1":          true,
		"github.com/goobers/goobers/providers":             true,
		"github.com/goobers/goobers/deploy":                true,
		"github.com/goobers/goobers/internal/journalx":     false,
		"github.com/goobers/goobers/internal/worktree":     false,
		"github.com/goobers/goobers/internal/apix/journal": false,
	} {
		if got := findingTouchesLoadBearing(Finding{Tool: ToolTest, Package: pkg}); got != want {
			t.Errorf("findingTouchesLoadBearing(test %q) = %v, want %v", pkg, got, want)
		}
	}
	if !findingTouchesLoadBearing(Finding{Tool: ToolVet, Path: "providers/model.go", Line: 1}) || findingTouchesLoadBearing(Finding{Tool: ToolLint, Path: "providers/github.go", Line: 1}) {
		t.Fatal("vet/lint findings must use the path rule")
	}
	for name, tc := range map[string]struct {
		source, surface string
		want            bool
	}{
		"same directory":            {"internal/worktree/manager.go", "internal/worktree", true},
		"sibling directory":         {"internal/runner/run.go", "internal/worktree", false},
		"child directory":           {"internal/worktree/sub/x.go", "internal/worktree", false},
		"import path suffix":        {"internal/worktree/manager.go", "github.com/goobers/goobers/internal/worktree", true},
		"import path other package": {"internal/runner/run.go", "github.com/goobers/goobers/internal/worktree", false},
		"root file against import":  {"main.go", "github.com/goobers/goobers/internal/worktree", false},
	} {
		if got := inPackage(tc.source, tc.surface); got != tc.want {
			t.Errorf("%s: inPackage(%q, %q) = %v, want %v", name, tc.source, tc.surface, got, tc.want)
		}
	}
}
