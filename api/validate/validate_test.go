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

func joinIssues(r *Report) string {
	var b strings.Builder
	for _, i := range r.Issues {
		b.WriteString(i.String())
		b.WriteByte('\n')
	}
	return b.String()
}
