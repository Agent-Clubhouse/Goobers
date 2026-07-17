package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newV(t *testing.T) *Validator {
	t.Helper()
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// envelopeName derives the envelope kind from a fixture filename, e.g.
// "result-bad-status.json" -> "result".
func envelopeName(file string) string {
	base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if i := strings.IndexByte(base, '-'); i >= 0 {
		return base[:i]
	}
	return base
}

func TestValidEnvelopesPass(t *testing.T) {
	v := newV(t)
	files, _ := filepath.Glob("testdata/envelopes/valid/*.json")
	if len(files) == 0 {
		t.Fatal("no valid envelope fixtures found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.ValidateEnvelope(envelopeName(f), data); err != nil {
				t.Errorf("expected %s to pass, got: %v", f, err)
			}
		})
	}
}

func TestInvalidEnvelopesFail(t *testing.T) {
	v := newV(t)
	files, _ := filepath.Glob("testdata/envelopes/invalid/*.json")
	if len(files) == 0 {
		t.Fatal("no invalid envelope fixtures found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.ValidateEnvelope(envelopeName(f), data); err == nil {
				t.Errorf("expected %s to fail validation, but it passed", f)
			}
		})
	}
}

// TestExampleConfigPasses is the headline acceptance check: the reference config
// in /config-examples validates clean (exit 0 equivalent).
func TestExampleConfigPasses(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("../../config-examples")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("expected /config-examples to be valid, got issues:\n%s", joinIssues(report))
	}
	if report.Objects < 4 {
		t.Errorf("expected at least 4 objects, got %d", report.Objects)
	}
}

func TestConfigBadReportsCrossRefErrors(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("testdata/config-bad")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("expected config-bad to have errors, got none")
	}
	all := joinIssues(report)
	for _, want := range []string{
		"ghost-gaggle",   // manifest -> undefined gaggle
		"ghost-coder",    // task -> undefined goober
		"ghost-reviewer", // gate -> undefined reviewer goober
		"ghost-state",    // start -> undefined state
		"missing.md",     // goober instructions file not found
	} {
		if !strings.Contains(all, want) {
			t.Errorf("expected an error mentioning %q; full report:\n%s", want, all)
		}
	}
}

// TestCompilerChecksSurfaceInValidate proves `goobers validate` inherits the
// workflow compiler's deeper analysis (issue #9): a bad schedule expression, an
// unreachable state, and a stage using a capability its goober does not grant
// are all reported, with actionable messages.
func TestCompilerChecksSurfaceInValidate(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("testdata/config-bad-compile")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("expected config-bad-compile to have errors, got none")
	}
	all := joinIssues(report)
	for _, want := range []string{
		"invalid schedule",                   // bad cron expression
		`state "orphan" is unreachable`,      // reachability
		`capability "repo:push" not granted`, // capability admission
	} {
		if !strings.Contains(all, want) {
			t.Errorf("expected an error mentioning %q; full report:\n%s", want, all)
		}
	}
}

func TestBrokenManifestFailsClearly(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("testdata/broken-manifest")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("expected broken manifest to fail, got no errors")
	}
	all := joinIssues(report)
	// The error should clearly point at the offending field(s).
	if !strings.Contains(all, "environment") && !strings.Contains(all, "name") {
		t.Errorf("expected a clear field-level error (environment/name); got:\n%s", all)
	}
}

// TestGateExclusivityGivesClearMessageNoCascade reproduces QA-1's finding: a
// gate that violates GT-016 (two evaluator blocks) is schema-invalid, but it
// must still produce the clear "exactly one evaluator block" message AND must not
// trigger a misleading cascade (the goober's workflow reference must still
// resolve because the schema-invalid workflow stays in the cross-ref index).
func TestGateExclusivityGivesClearMessageNoCascade(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("testdata/config-bad-gate")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("expected the GT-016 violation to be rejected, got no errors")
	}
	all := joinIssues(report)
	if !strings.Contains(all, "exactly one evaluator block") {
		t.Errorf("expected the clear GT-016 message; got:\n%s", all)
	}
	// The cascade bug blamed the goober: "associated workflow \"flow\" is not defined".
	if strings.Contains(all, `workflow "flow" is not defined`) {
		t.Errorf("misleading cascade present: workflow reference dangled even though flow is defined; got:\n%s", all)
	}
	// And the cryptic raw schema message should be humanized.
	if strings.Contains(all, ": not failed") {
		t.Errorf("expected the cryptic \"not failed\" schema message to be humanized; got:\n%s", all)
	}
}

func TestWarningCodesAreStable(t *testing.T) {
	got := []WarningCode{
		WarningDeprecatedFeature,
		WarningPreviewFeature,
		WarningCompatibility,
		WarningModelFallback,
	}
	want := []WarningCode{"VER001", "VER002", "VER003", "MODEL002"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("warning code %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReportWarningsPreserveShapeAndSortDeterministically(t *testing.T) {
	report := &Report{Issues: []Issue{
		{Code: WarningPreviewFeature, Severity: Warning, File: "z.yaml", Kind: "Workflow", Name: "preview", Message: "preview feature is unstable"},
		{Severity: Error, File: "a.yaml", Message: "remains an error"},
		{Code: WarningModelFallback, Severity: Warning, Kind: "Goober", Name: "coder", Message: "configured model is unavailable"},
		{Code: WarningDeprecatedFeature, Severity: Warning, File: "a.yaml", Kind: "Workflow", Name: "legacy", Message: "deprecated feature remains supported"},
	}}

	got := report.Warnings()
	if len(got) != 3 {
		t.Fatalf("Warnings() returned %d warnings, want 3: %+v", len(got), got)
	}
	want := []CodedWarning{
		{Code: WarningModelFallback, Severity: Warning, Scope: "Goober/coder", Explanation: "configured model is unavailable"},
		{Code: WarningDeprecatedFeature, Severity: Warning, Scope: "a.yaml Workflow/legacy", Explanation: "deprecated feature remains supported"},
		{Code: WarningPreviewFeature, Severity: Warning, Scope: "z.yaml Workflow/preview", Explanation: "preview feature is unstable"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("warning %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func joinIssues(r *Report) string {
	var b strings.Builder
	for _, i := range r.Issues {
		b.WriteString(i.String())
		b.WriteByte('\n')
	}
	return b.String()
}
