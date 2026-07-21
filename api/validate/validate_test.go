package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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
// in /config-examples is valid and explains that its starter is manual-only.
func TestExampleConfigPasses(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("../../config-examples")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("expected /config-examples to be valid, got issues:\n%s", joinIssues(report))
	}
	warnings := report.Warnings()
	if len(warnings) != 1 ||
		warnings[0].Code != WarningCompatibility ||
		warnings[0].Severity != Warning ||
		!strings.Contains(warnings[0].Explanation, "goobers run default-implement") {
		t.Fatalf("expected one actionable manual-only warning, got %+v", warnings)
	}
	if report.Objects < 4 {
		t.Errorf("expected at least 4 objects, got %d", report.Objects)
	}
}

func TestGooberAssetsAreOpaqueToConfigValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../config-examples")); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "assets", "fixture.yaml")
	if err := os.MkdirAll(filepath.Dir(asset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("not: [valid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(root)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("asset fixture was parsed as config:\n%s", joinIssues(report))
	}
}

func TestGooberAssetStructureIsValidated(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"symlink root": func(t *testing.T, assets string) {
			if err := os.Symlink(t.TempDir(), assets); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
		},
		"symlink entry": func(t *testing.T, assets string) {
			if err := os.Mkdir(assets, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(assets, "reference")); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
		},
		"special file": func(t *testing.T, assets string) {
			if err := os.Mkdir(assets, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(filepath.Join(assets, "stream"), 0o600); err != nil {
				t.Skipf("FIFO unsupported: %v", err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(root, os.DirFS("../../config-examples")); err != nil {
				t.Fatal(err)
			}
			assets := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "assets")
			setup(t, assets)

			report, err := newV(t).ValidateDir(root)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			if !report.HasErrors() || !strings.Contains(joinIssues(report), "invalid goober assets") {
				t.Fatalf("unsafe assets were accepted:\n%s", joinIssues(report))
			}
		})
	}
}

func TestGooberSchemaPreservesAdapterOwnedHarnessConfig(t *testing.T) {
	v := newV(t)
	goober := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Goober",
		"metadata": {"name": "coder"},
		"spec": {
			"gaggle": "example",
			"role": "coder",
			"instructions": "instructions.md",
			"harness": "copilot",
			"model": "adapter-specific-model",
			"harnessOptions": {
				"enabled": true,
				"budget": 3,
				"nested": {"strategy": "adaptive"}
			}
		}
	}`
	if err := v.ValidateJSON("goober.schema.json", []byte(goober)); err != nil {
		t.Fatalf("adapter-owned harness config failed schema validation: %v", err)
	}
}

func TestWorkflowSchemaAcceptsExplicitManualOnlyTrigger(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "manual-flow"},
		"spec": {
			"gaggle": "example",
			"triggers": TRIGGERS,
			"start": "act",
			"tasks": [{
				"name": "act",
				"type": "deterministic",
				"goal": "Act on demand.",
				"run": {"command": ["true"]}
			}]
		}
	}`
	cases := []struct {
		name     string
		triggers string
		wantErr  bool
	}{
		{name: "manual-only", triggers: `[{"type": "manual"}]`},
		{name: "empty", triggers: `[]`, wantErr: true},
		{name: "manual mixed with schedule", triggers: `[{"type": "manual"}, {"type": "schedule", "schedule": "@daily"}]`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v.ValidateJSON("workflow.schema.json", []byte(strings.Replace(workflow, "TRIGGERS", tc.triggers, 1)))
			if tc.wantErr && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected schema validation to pass, got %v", err)
			}
		})
	}
}

func TestWorkflowSchemaValidatesContinueOnError(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "best-effort"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "manual"}],
			"start": "notify",
			"tasks": [{
				"name": "notify",
				"type": "deterministic",
				"goal": "Notify without failing the workflow.",
				"run": {"command": ["false"]},
				"continueOnError": VALUE
			}]
		}
	}`
	for _, tc := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "boolean", value: "true"},
		{name: "non-boolean", value: `"true"`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v.ValidateJSON("workflow.schema.json", []byte(strings.Replace(workflow, "VALUE", tc.value, 1)))
			if tc.wantErr && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected schema validation to pass, got %v", err)
			}
		})
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

func TestAcceptedButInertWorkflowFieldsEmitCodedWarnings(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("testdata/config-warnings")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("warnings must not fail validation:\n%s", joinIssues(report))
	}

	warnings := report.Warnings()
	if len(warnings) != 2 {
		t.Fatalf("Warnings() = %+v, want expectedOutputs and run.image warnings", warnings)
	}
	all := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if warning.Code != WarningCompatibility {
			t.Errorf("warning code = %q, want %q", warning.Code, WarningCompatibility)
		}
		all = append(all, warning.Explanation)
	}
	explanations := strings.Join(all, "\n")
	for _, want := range []string{
		"expectedOutputs is declared but the stage has no inputs.resultFile to emit it through",
		"run.image is not honored by the local runner",
	} {
		if !strings.Contains(explanations, want) {
			t.Errorf("warnings = %+v, want explanation containing %q", warnings, want)
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

func TestWorkflowOwnerMustBelongToWorkflowGaggle(t *testing.T) {
	report, err := newV(t).ValidateDir("testdata/config-cross-gaggle-owner")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("expected cross-gaggle workflow owner to fail validation")
	}
	got := joinIssues(report)
	if !strings.Contains(got, `targets goober "reviewer" in gaggle "beta", not workflow gaggle "alpha"`) ||
		!strings.Contains(got, `reviewer goober "reviewer" is in gaggle "beta", not workflow gaggle "alpha"`) {
		t.Fatalf("cross-gaggle owner errors missing:\n%s", got)
	}
}

func TestForeignLayoutDiagnosticsAreActionable(t *testing.T) {
	tests := []struct {
		name              string
		manifestGaggles   string
		workflowGaggle    string
		capability        string
		writeInstructions bool
		want              []string
	}{
		{
			name:              "valid",
			manifestGaggles:   "    - acme",
			workflowGaggle:    "acme",
			capability:        "repo:read",
			writeInstructions: true,
		},
		{
			name:              "unbound workflow",
			manifestGaggles:   "    - acme",
			workflowGaggle:    "ghost",
			capability:        "repo:read",
			writeInstructions: true,
			want: []string{
				`foreign.yaml Workflow/build: spec.gaggle names "ghost", but no Gaggle/ghost definition was found`,
			},
		},
		{
			name:              "manifest names undefined gaggle",
			manifestGaggles:   "    - ghost",
			workflowGaggle:    "acme",
			capability:        "repo:read",
			writeInstructions: true,
			want: []string{
				`foreign.yaml Manifest/foreign: spec.gaggles references "ghost", but no Gaggle/ghost definition was found`,
			},
		},
		{
			name:              "capability typo",
			manifestGaggles:   "    - acme",
			workflowGaggle:    "acme",
			capability:        "github:prs:write",
			writeInstructions: true,
			want: []string{
				`foreign.yaml Goober/coder: spec.capabilities contains unknown capability "github:prs:write"; did you mean "github:pr:write"?`,
			},
		},
		{
			name:            "missing instructions",
			manifestGaggles: "    - acme",
			workflowGaggle:  "acme",
			capability:      "repo:read",
			want: []string{
				`foreign.yaml Goober/coder: spec.instructions file "instructions.md" was not found; expected it at "instructions.md"`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: foreign
spec:
  instance:
    name: foreign
    environment: dev
  gaggles:
%s
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: acme
spec:
  project:
    provider: github
    owner: acme
    name: app
  backlog:
    provider: github
    project: acme/app
  isolation:
    namespace: gaggle-acme
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: coder
spec:
  gaggle: acme
  role: coder
  instructions: instructions.md
  capabilities:
    - %s
  workflows:
    - build
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: build
spec:
  gaggle: %s
  triggers:
    - type: manual
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: Build the project.
      run:
        command: ["true"]
        workspace: scratch
`, tc.manifestGaggles, tc.capability, tc.workflowGaggle)
			if err := os.WriteFile(filepath.Join(dir, "foreign.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.writeInstructions {
				if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("# Coder\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			got := joinIssues(report)
			if len(tc.want) == 0 {
				if report.HasErrors() {
					t.Fatalf("valid foreign layout reported errors:\n%s", got)
				}
				return
			}
			if !report.HasErrors() {
				t.Fatalf("malformed foreign layout reported no errors")
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostics missing %q:\n%s", want, got)
				}
			}
			if tc.name == "capability typo" && strings.Count(got, `unknown capability "github:prs:write"`) != 1 {
				t.Errorf("capability typo should be reported once at its Goober source:\n%s", got)
			}
		})
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

func TestWorkflowWarningPreservesLegacyCLIRepresentation(t *testing.T) {
	issue := Issue{
		Code:     WarningCompatibility,
		Severity: Warning,
		File:     "gaggles/alpha/workflows/deploy.yaml",
		Gaggle:   "alpha",
		Kind:     "Workflow",
		Name:     "deploy",
		Message:  "configuration uses a compatibility path",
	}
	report := &Report{Issues: []Issue{issue}}

	apiWarning := report.Warnings()[0]
	if apiWarning.Code != WarningCompatibility ||
		apiWarning.Scope != "gaggles/alpha/workflows/deploy.yaml Gaggle/alpha Workflow/deploy" {
		t.Fatalf("API warning = %+v, want coded source and gaggle scope", apiWarning)
	}
	cliWarning := report.CLIWarnings()[0]
	if cliWarning.Code != "" || cliWarning.Scope != "Workflow/deploy" {
		t.Fatalf("CLI warning = %+v, want legacy uncoded workflow scope", cliWarning)
	}
	if got := issue.CLIString(); got != "WARNING Workflow/deploy: configuration uses a compatibility path" {
		t.Fatalf("CLIString() = %q", got)
	}
	cliIssue := report.CLIReport().Issues[0]
	if cliIssue.Code != "" || cliIssue.File != "" || cliIssue.Gaggle != "" {
		t.Fatalf("CLI report issue = %+v, want legacy JSON provenance", cliIssue)
	}
	if report.Issues[0] != issue {
		t.Fatalf("CLIReport mutated source issue: %+v", report.Issues[0])
	}
}

func TestCLIReportPreservesIssuesSliceShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		issues []Issue
	}{
		{name: "nil"},
		{name: "empty", issues: []Issue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := (&Report{Issues: tc.issues}).CLIReport()
			if (report.Issues == nil) != (tc.issues == nil) {
				t.Fatalf("CLIReport issues = %#v, want source slice shape", report.Issues)
			}
		})
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

func TestGaggleSchemaAcceptsCICommandAndRequiredCapabilities(t *testing.T) {
	v := newV(t)
	gaggle := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Gaggle",
		"metadata": {"name": "web"},
		"spec": {
			"project": {"provider": "github", "owner": "acme", "name": "web"},
			"backlog": {"provider": "github", "project": "acme/web"},
			"isolation": {"namespace": "gaggle-web"},
			CIFIELD
			REQFIELD
			"displayName": "Web"
		}
	}`
	for _, tc := range []struct {
		name    string
		ci      string
		req     string
		wantErr bool
	}{
		{name: "both fields valid", ci: `"ciCommand": ["npm", "run", "ci"],`, req: `"requiredCapabilities": ["dotnet@8", "os=windows"],`},
		{name: "omitted fields (regression)", ci: "", req: ""},
		{name: "empty ciCommand rejected", ci: `"ciCommand": [],`, req: "", wantErr: true},
		{name: "malformed capability rejected", ci: "", req: `"requiredCapabilities": ["dot net"],`, wantErr: true},
		{name: "empty capability string rejected", ci: "", req: `"requiredCapabilities": [""],`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(gaggle, "CIFIELD", tc.ci, 1)
			doc = strings.Replace(doc, "REQFIELD", tc.req, 1)
			err := v.ValidateJSON("gaggle.schema.json", []byte(doc))
			if tc.wantErr && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected schema validation to pass, got %v", err)
			}
		})
	}
}

func TestWorkflowSchemaValidatesTaskRequiredCapabilities(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "build"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "manual"}],
			"start": "act",
			"tasks": [{
				"name": "act",
				"type": "deterministic",
				"goal": "Build.",
				"run": {"command": ["dotnet", "build"]},
				"requiredCapabilities": CAPS
			}]
		}
	}`
	for _, tc := range []struct {
		name    string
		caps    string
		wantErr bool
	}{
		{name: "valid tokens", caps: `["dotnet@8", "xcode"]`},
		{name: "empty array", caps: `[]`},
		{name: "malformed token", caps: `["os windows"]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v.ValidateJSON("workflow.schema.json", []byte(strings.Replace(workflow, "CAPS", tc.caps, 1)))
			if tc.wantErr && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected schema validation to pass, got %v", err)
			}
		})
	}
}
