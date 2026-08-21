package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/supportmatrix"
	wf "github.com/goobers/goobers/internal/workflow"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
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
	// The compatibility warnings are the manual-only advisories on the
	// example workflows that carry no schedule trigger: default-implement and
	// docs-updater in both acme-web and its #2777 acme-web-claude parallel
	// (#2777's fleet posture duplicates the same workflow shape under a
	// different harness), plus one implementation workflow per polyglot
	// service. Preview warnings are asserted separately by
	// TestPreviewFeaturesRequireInstanceOptIn.
	var warnings []CodedWarning
	for _, warning := range report.Warnings() {
		if warning.Code == WarningCompatibility {
			warnings = append(warnings, warning)
		}
	}
	if len(warnings) != 7 {
		t.Fatalf("expected seven actionable manual-only compatibility warnings, got %+v", warnings)
	}
	var sawDefaultImplement, sawDocsUpdater, sawDotnetImplementation, sawJavaImplementation, sawPythonImplementation bool
	for _, w := range warnings {
		if w.Code != WarningCompatibility || w.Severity != Warning {
			t.Fatalf("unexpected warning (want only manual-only compatibility advisories): %+v", w)
		}
		if strings.Contains(w.Explanation, "goobers run default-implement") {
			sawDefaultImplement = true
		}
		if strings.Contains(w.Explanation, "goobers run docs-updater") {
			sawDocsUpdater = true
		}
		if strings.Contains(w.Explanation, "goobers run dotnet-implementation") {
			sawDotnetImplementation = true
		}
		if strings.Contains(w.Explanation, "goobers run java-implementation") {
			sawJavaImplementation = true
		}
		if strings.Contains(w.Explanation, "goobers run python-implementation") {
			sawPythonImplementation = true
		}
	}
	if !sawDefaultImplement || !sawDocsUpdater || !sawDotnetImplementation || !sawJavaImplementation || !sawPythonImplementation {
		t.Fatalf("expected manual-only warnings for default-implement, docs-updater, and the dotnet-service, java-service, and python-service implementations, got %+v", warnings)
	}
	if report.Objects < 4 {
		t.Errorf("expected at least 4 objects, got %d", report.Objects)
	}
}

func TestLabelPredicatesValidatedAtConfigLoad(t *testing.T) {
	valid := `("size:s" in labels || "size:m" in labels) && !("platform:windows" in labels)`
	tests := []struct {
		name              string
		gaggleExpression  string
		triggerExpression string
		taskExpression    string
		taskExpressionSet bool
		want              string
	}{
		{
			name:              "valid grouped expressions",
			gaggleExpression:  valid,
			triggerExpression: valid,
			taskExpression:    valid,
		},
		{
			name:             "invalid gaggle expression",
			gaggleExpression: `labels.size() > 0`,
			want:             "spec.backlog.labelPredicate is invalid",
		},
		{
			name:              "invalid trigger expression",
			triggerExpression: `labels.exists(label, label == "size:s")`,
			want:              "spec.triggers[0].labelPredicate is invalid",
		},
		{
			name:           "invalid backlog-query input",
			taskExpression: `"size:s" in`,
			want:           "spec.tasks[0].inputs.labelPredicate is invalid",
		},
		{
			name:             "blank gaggle expression",
			gaggleExpression: " \t",
			want:             "spec.backlog.labelPredicate is invalid",
		},
		{
			name:              "blank trigger expression",
			triggerExpression: " \t",
			want:              "spec.triggers[0].labelPredicate is invalid",
		},
		{
			name:           "blank backlog-query input",
			taskExpression: " \t",
			want:           "spec.tasks[0].inputs.labelPredicate is invalid",
		},
		{
			name:              "empty backlog-query input",
			taskExpressionSet: true,
			want:              "spec.tasks[0].inputs.labelPredicate is invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := func(indent, name, value string, includeEmpty bool) string {
				if value == "" && !includeEmpty {
					return ""
				}
				return fmt.Sprintf("%s%s: %q\n", indent, name, value)
			}
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: local
spec:
  instance:
    name: local
    environment: dev
  gaggles: [acme]
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
    labels: [trusted]
%s  isolation:
    namespace: gaggle-acme
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: select
spec:
  gaggle: acme
  triggers:
    - type: backlog-item
      selector:
        area:runner: "true"
%s  start: query
  tasks:
    - name: query
      type: deterministic
      goal: Query matching backlog items.
      run:
        command: ["goobers", "backlog-query"]
      capabilities:
        - github:issues:write
      inputs:
        requireLabels: area:runner
        excludeLabels: goobers:claimed
%s`, field("    ", "labelPredicate", tt.gaggleExpression, false),
				field("      ", "labelPredicate", tt.triggerExpression, false),
				field("        ", "labelPredicate", tt.taskExpression, tt.taskExpressionSet))

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			issues := joinIssues(report)
			if tt.want == "" {
				if report.HasErrors() {
					t.Fatalf("valid predicates reported errors:\n%s", issues)
				}
				return
			}
			if !report.HasErrors() || !strings.Contains(issues, tt.want) {
				t.Fatalf("issues = %q, want error containing %q", issues, tt.want)
			}
		})
	}
}

func TestFieldSelectionsValidatedAtConfigLoad(t *testing.T) {
	valid := `fields["number"] >= 10 && fields["state"] == "open"`
	tests := []struct {
		name             string
		gagglePredicate  string
		triggerPredicate string
		taskPredicate    string
		fieldOrder       string
		want             string
	}{
		{
			name:             "valid field selection",
			gagglePredicate:  valid,
			triggerPredicate: valid,
			taskPredicate:    valid,
			fieldOrder:       "milestone.number:desc,number:asc",
		},
		{
			name:            "invalid gaggle predicate",
			gagglePredicate: `fields.number == 10`,
			want:            "spec.backlog.fieldPredicate is invalid",
		},
		{
			name:             "invalid trigger predicate",
			triggerPredicate: `fields["number"] + 1 == 10`,
			want:             "spec.triggers[0].fieldPredicate is invalid",
		},
		{
			name:          "invalid task predicate",
			taskPredicate: `fields["number"]`,
			want:          "spec.tasks[0].inputs.fieldPredicate is invalid",
		},
		{
			name:       "invalid field order",
			fieldOrder: "priority:sideways",
			want:       "spec.tasks[0].inputs.fieldOrder is invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := func(indent, name, value string) string {
				if value == "" {
					return ""
				}
				return fmt.Sprintf("%s%s: %q\n", indent, name, value)
			}
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: local
spec:
  instance:
    name: local
    environment: dev
  gaggles: [acme]
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
%s  isolation:
    namespace: gaggle-acme
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: select
spec:
  gaggle: acme
  triggers:
    - type: backlog-item
%s  start: query
  tasks:
    - name: query
      type: deterministic
      goal: Query matching backlog items.
      run:
        command: ["goobers", "backlog-query"]
      capabilities:
        - github:issues:write
      inputs:
%s%s`, field("    ", "fieldPredicate", tt.gagglePredicate),
				field("      ", "fieldPredicate", tt.triggerPredicate),
				field("        ", "fieldPredicate", tt.taskPredicate),
				field("        ", "fieldOrder", tt.fieldOrder))

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			issues := joinIssues(report)
			if tt.want == "" {
				if report.HasErrors() {
					t.Fatalf("valid field selection reported errors:\n%s", issues)
				}
				return
			}
			if !report.HasErrors() || !strings.Contains(issues, tt.want) {
				t.Fatalf("issues = %q, want error containing %q", issues, tt.want)
			}
		})
	}
}

// TestCanonicalConfigIsGAWithoutPreviewOptIn is the #1196 regression: the
// canonical DSL surface that guided-init scaffolds and /config-examples model
// must validate with NO VER002 preview findings even without the preview
// opt-in, because every standard field is GA. An earlier placeholder marked
// every field preview, so guided-init tripped a blocking VER002 on every field
// ("config directory failed validation"). The shipped config omits the opt-in,
// proving the surface is genuinely GA rather than merely opt-in-tolerated.
func TestCanonicalConfigIsGAWithoutPreviewOptIn(t *testing.T) {
	for _, dir := range []string{"../../config-examples", "../../examples/ios-simulator"} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			manifest, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(manifest), wf.PreviewFeaturesAnnotation) {
				t.Fatalf("GA-only shipped config must not opt in to preview features:\n%s", manifest)
			}

			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			for _, issue := range report.Issues {
				if issue.Code == WarningPreviewFeature {
					t.Errorf("standard field wrongly flagged preview without opt-in (#1196): %s/%s: %s",
						issue.Kind, issue.Name, issue.Message)
				}
			}
			if report.HasErrors() {
				t.Fatalf("canonical config without preview opt-in must validate clean (all standard fields GA), got:\n%s", joinIssues(report))
			}
		})
	}
}

func TestGagglePreviewFeatureRequiresExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name         string
		annotation   string
		wantBlocking bool
	}{
		{name: "default off", wantBlocking: true},
		{name: "explicit opt-in", annotation: "\n  annotations:\n    goobers.dev/allow-preview-features: \"true\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: preview-test%s
spec:
  instance:
    name: preview-test
    environment: dev
  gaggles:
    - preview-test
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: preview-test
spec:
  project:
    provider: github
    owner: acme
    name: app
  backlog:
    provider: github
    project: acme/app
  isolation:
    namespace: gaggle-preview-test
  sandbox:
    agentic: enforced
`, tc.annotation)
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}

			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			var preview *Issue
			for i := range report.Issues {
				issue := &report.Issues[i]
				if issue.Code == WarningPreviewFeature && issue.Kind == "Gaggle" {
					preview = issue
					break
				}
			}
			if preview == nil {
				t.Fatalf("missing Gaggle preview diagnostic:\n%s", joinIssues(report))
			}
			if gotBlocking := preview.Severity == Error; gotBlocking != tc.wantBlocking {
				t.Fatalf("preview diagnostic severity = %s, want blocking %v: %s", preview.Severity, tc.wantBlocking, preview.Message)
			}
		})
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
			if err := mkfifoAsset(filepath.Join(assets, "stream")); err != nil {
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
	for _, harness := range []string{"copilot", "claude-code"} {
		goober := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Goober",
		"metadata": {"name": "coder"},
		"spec": {
			"gaggle": "example",
			"role": "coder",
			"instructions": "instructions.md",
			"harness": "` + harness + `",
			"model": "adapter-specific-model",
			"harnessOptions": {
				"enabled": true,
				"budget": 3,
				"nested": {"strategy": "adaptive"}
			},
			"policyActions": ["modify-repository"],
			"conditionalPolicyActions": ["open-or-update-pr"]
		}
		}`
		if err := v.ValidateJSON("goober.schema.json", []byte(goober)); err != nil {
			t.Fatalf("%s adapter-owned harness config failed schema validation: %v", harness, err)
		}
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

func TestWorkflowSchemaAcceptsPollingPriority(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "priority-flow"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "backlog-item", "priority": 25}],
			"start": "act",
			"tasks": [{
				"name": "act",
				"type": "deterministic",
				"goal": "Act on ready work.",
				"run": {"command": ["true"]}
			}]
		}
	}`
	if err := v.ValidateJSON("workflow.schema.json", []byte(workflow)); err != nil {
		t.Fatalf("polling priority failed schema validation: %v", err)
	}
}

func TestWorkflowSchemaAcceptsScheduleIdleBackoff(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "adaptive-poll"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "schedule", "schedule": "* * * * *",
				"idleBackoff": {"enabled": true, "floor": "1m", "ceiling": "15m"}}],
			"start": "act",
			"tasks": [{
				"name": "act",
				"type": "deterministic",
				"goal": "Poll for work.",
				"run": {"command": ["true"]}
			}]
		}
	}`
	if err := v.ValidateJSON("workflow.schema.json", []byte(workflow)); err != nil {
		t.Fatalf("schedule idle backoff failed schema validation: %v", err)
	}
}

func TestWorkflowSchemaValidatesBacklogTrustLabel(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "trusted-flow"},
		"spec": {
			"gaggle": "example",
			"triggers": TRIGGERS,
			"start": "act",
			"tasks": [{
				"name": "act",
				"type": "deterministic",
				"goal": "Act on approved work.",
				"run": {"command": ["true"]}
			}]
		}
	}`
	cases := []struct {
		name     string
		triggers string
		wantErr  bool
	}{
		{
			name:     "explicit trust and routing labels",
			triggers: `[{"type":"backlog-item","trustLabel":"team-approved","selector":{"team-approved":"true","goobers:ready":"true"}}]`,
		},
		{
			name:     "empty trust label",
			triggers: `[{"type":"backlog-item","trustLabel":""}]`,
			wantErr:  true,
		},
		{
			name:     "trust label on schedule",
			triggers: `[{"type":"schedule","schedule":"@daily","trustLabel":"team-approved"}]`,
			wantErr:  true,
		},
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

func TestWorkflowSchemaValidatesDSLVersion(t *testing.T) {
	v := newV(t)
	workflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: DSL_VERSION
metadata: {name: versioned-flow}
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: act
  tasks:
    - name: act
      type: deterministic
      goal: Act.
      run: {command: ["true"]}
`
	for _, tc := range []struct {
		version string
		wantErr bool
	}{
		{version: `"1.4"`},
		{version: `"1"`, wantErr: true},
		{version: `"v1"`, wantErr: true},
		{version: `"1.4.0"`, wantErr: true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			jsonBytes, err := yaml.YAMLToJSON([]byte(strings.Replace(workflow, "DSL_VERSION", tc.version, 1)))
			if err != nil {
				t.Fatalf("convert YAML: %v", err)
			}
			err = v.ValidateJSON("workflow.schema.json", jsonBytes)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected schema validation to fail")
				}
				if !strings.Contains(err.Error(), "/dslVersion") {
					t.Fatalf("schema error %q does not name dslVersion", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected schema validation to pass, got %v", err)
			}
		})
	}

	withoutVersion := strings.Replace(workflow, "dslVersion: DSL_VERSION\n", "", 1)
	jsonBytes, err := yaml.YAMLToJSON([]byte(withoutVersion))
	if err != nil {
		t.Fatalf("convert YAML without dslVersion: %v", err)
	}
	if err := v.ValidateJSON("workflow.schema.json", jsonBytes); err != nil {
		t.Fatalf("missing dslVersion changed validation behavior: %v", err)
	}
}

func TestWorkflowSchemaRequiresExactlyOneRunForm(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"dslVersion": "2.0",
		"metadata": {"name": "inline-check"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "manual"}],
			"start": "check",
			"tasks": [{
				"name": "check",
				"type": "deterministic",
				"goal": "Check policy.",
				"run": RUN
			}]
		}
	}`
	for _, tc := range []struct {
		name    string
		run     string
		wantErr bool
	}{
		{name: "command", run: `{"command":["true"]}`},
		{name: "script", run: `{"script":"printf 'ok\\n'"}`},
		{name: "both", run: `{"command":["true"],"script":"printf 'ok\\n'"}`, wantErr: true},
		{name: "neither", run: `{}`, wantErr: true},
		{name: "empty script", run: `{"script":""}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v.ValidateJSON("workflow.schema.json", []byte(strings.Replace(workflow, "RUN", tc.run, 1)))
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

func TestWorkflowSchemaAcceptsDocsRoots(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "docs-flow"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "manual"}],
			"start": "act",
			"docsRoots": ROOTS,
			"tasks": [{
				"name": "act",
				"type": "deterministic",
				"goal": "Act.",
				"run": {"command": ["true"]}
			}]
		}
	}`
	for _, tc := range []struct {
		name    string
		roots   string
		wantErr bool
	}{
		{name: "valid roots", roots: `["docs", "docs/design", "README.md"]`},
		{name: "empty-string item rejected", roots: `["docs", ""]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v.ValidateJSON("workflow.schema.json", []byte(strings.Replace(workflow, "ROOTS", tc.roots, 1)))
			if tc.wantErr && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected schema validation to pass, got %v", err)
			}
		})
	}
}

// TestWorkflowDocsRootsSemanticValidation covers the config-load lexical check
// (#1016): roots that pass the schema (non-empty strings) but are not usable
// containment roots — absolute, escaping, whole-repo — are rejected with a
// docsRoots error, while genuine repo-relative roots pass.
func TestWorkflowDocsRootsSemanticValidation(t *testing.T) {
	gaggleYAML := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project:
    provider: github
    owner: acme
    name: web
    branch: main
    connectionRef: c
  backlog:
    provider: github
    project: acme/web
    labels: [goobers]
    connectionRef: c
  isolation:
    namespace: gaggle-example
`
	workflowTmpl := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: docs-updater
spec:
  gaggle: example
  triggers:
    - type: manual
  start: act
  docsRoots: ROOTS
  tasks:
    - name: act
      type: deterministic
      goal: Act.
      run:
        command: ["true"]
`
	for _, tc := range []struct {
		name        string
		roots       string
		wantDocsErr bool
	}{
		{name: "valid", roots: `["docs", "docs/design", "README.md"]`},
		{name: "absolute", roots: `["/etc/docs"]`, wantDocsErr: true},
		{name: "escaping", roots: `[".."]`, wantDocsErr: true},
		{name: "whole-repo", roots: `["."]`, wantDocsErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gaggle.yaml"), []byte(gaggleYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			wf := strings.Replace(workflowTmpl, "ROOTS", tc.roots, 1)
			if err := os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(wf), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			var docsErr bool
			for _, issue := range report.Issues {
				if issue.Severity == Error && strings.Contains(issue.Message, "docsRoots") {
					docsErr = true
				}
			}
			if docsErr != tc.wantDocsErr {
				t.Fatalf("docsRoots error = %v, want %v; issues = %v", docsErr, tc.wantDocsErr, report.Issues)
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

func TestGooberSkillPackageWarnings(t *testing.T) {
	base := t.TempDir()
	configDir := filepath.Join(base, "config")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: skill-packages
spec:
  instance:
    name: skill-packages
    environment: dev
  gaggles:
    - example
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project:
    provider: github
    owner: example
    name: app
  backlog:
    provider: github
    project: example/app
  isolation:
    namespace: gaggle-example
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: coder
spec:
  gaggle: example
  role: coder
  instructions: instructions.md
  skills:
    - present-shared
    - present-scoped
    - missing
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "instructions.md"), []byte("# Coder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "skills", "present-shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "gaggles", "example", "skills", "present-scoped"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := newV(t).ValidateDir(configDir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	var warnings []CodedWarning
	for _, warning := range report.Warnings() {
		if warning.Code == WarningMissingSkillPackage {
			warnings = append(warnings, warning)
		}
	}
	want := CodedWarning{
		Code:        WarningMissingSkillPackage,
		Severity:    Warning,
		Scope:       "config.yaml Goober/coder",
		Explanation: `spec.skills declares "missing", but no skill package directory was found at "gaggles/example/skills/missing" or "skills/missing"`,
	}
	if len(warnings) != 1 || warnings[0] != want {
		t.Fatalf("missing skill warnings = %+v, want %+v", warnings, want)
	}
	if report.HasErrors() {
		t.Fatalf("missing skill package must remain non-fatal: %+v", report.Issues)
	}
}

func TestGooberSkillPackageWarningsIncludeInvalidConfigsAndNames(t *testing.T) {
	base := t.TempDir()
	configDir := filepath.Join(base, "config")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: skill-packages
spec:
  instance:
    name: skill-packages
    environment: dev
  gaggles:
    - example
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project:
    provider: github
    owner: example
    name: app
  backlog:
    provider: github
    project: example/app
  isolation:
    namespace: gaggle-example
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: coder
spec:
  gaggle: example
  role: coder
  instructions: missing.md
  skills:
    - missing
    - nested/name
    - ..
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := newV(t).ValidateDir(configDir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("expected unrelated missing-instructions error")
	}
	var explanations []string
	for _, warning := range report.Warnings() {
		if warning.Code == WarningMissingSkillPackage {
			explanations = append(explanations, warning.Explanation)
		}
	}
	for _, want := range []string{
		`spec.skills declares "missing", but no skill package directory was found at "gaggles/example/skills/missing" or "skills/missing"`,
		`spec.skills declares "nested/name", but the skill name cannot resolve to a package directory under "skills"`,
		`spec.skills declares "..", but the skill name cannot resolve to a package directory under "skills"`,
	} {
		if !slices.Contains(explanations, want) {
			t.Errorf("missing skill warnings = %q, want explanation %q", explanations, want)
		}
	}
	if len(explanations) != 3 {
		t.Errorf("missing skill warning count = %d, want 3: %q", len(explanations), explanations)
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

// TestReadOnlyReferenceReposValidateCleanly proves a gaggle that reads distinct
// reference repos (MGV-10, #1285) — a read-write Project plus read-only
// AdditionalRepos under the same owner — passes validation end-to-end.
func TestReadOnlyReferenceReposValidateCleanly(t *testing.T) {
	report, err := newV(t).ValidateDir("testdata/config-additional-repos")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("expected config-additional-repos to validate cleanly, got:\n%s", joinIssues(report))
	}
}

func TestAdditionalReposCapabilityRuntimeSupport(t *testing.T) {
	tests := []struct {
		name            string
		additionalRepos []apiv1.RepoRef
		task            apiv1.Task
		wantError       bool
	}{
		{
			name:            "deterministic scratch rejected",
			additionalRepos: []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "reference"}},
			task: apiv1.Task{
				Name: "read-reference", Type: apiv1.TaskDeterministic, Goal: "Read reference.",
				Run:          &apiv1.DeterministicRun{Command: []string{"read-reference"}, Workspace: apiv1.WorkspaceScratch},
				Capabilities: []string{string(capability.ContentsRead)},
			},
			wantError: true,
		},
		{
			name:            "agentic scratch rejected",
			additionalRepos: []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "reference"}},
			task: apiv1.Task{
				Name: "read-reference", Type: apiv1.TaskAgentic, Goal: "Read reference.", Goober: "reader",
				Workspace: apiv1.WorkspaceScratch, Capabilities: []string{string(capability.ContentsRead)},
			},
			wantError: true,
		},
		{
			name:            "deterministic repo supported",
			additionalRepos: []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "reference"}},
			task: apiv1.Task{
				Name: "read-reference", Type: apiv1.TaskDeterministic, Goal: "Read reference.",
				Run:          &apiv1.DeterministicRun{Command: []string{"read-reference"}},
				Capabilities: []string{string(capability.ContentsRead)},
			},
		},
		{
			name: "scratch without additional repos supported",
			task: apiv1.Task{
				Name: "read-provider", Type: apiv1.TaskDeterministic, Goal: "Read provider.",
				Run:          &apiv1.DeterministicRun{Command: []string{"read-provider"}, Workspace: apiv1.WorkspaceScratch},
				Capabilities: []string{string(capability.ContentsRead)},
			},
		},
		{
			name:            "scratch without contents read supported",
			additionalRepos: []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "reference"}},
			task: apiv1.Task{
				Name: "compute", Type: apiv1.TaskDeterministic, Goal: "Compute.",
				Run: &apiv1.DeterministicRun{Command: []string{"compute"}, Workspace: apiv1.WorkspaceScratch},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ix := newIndex()
			ix.gaggles["example"] = apiv1.Gaggle{Spec: apiv1.GaggleSpec{AdditionalRepos: tc.additionalRepos}}
			ix.goobers["reader"] = apiv1.Goober{Spec: apiv1.GooberSpec{
				Gaggle: "example", Capabilities: []string{string(capability.ContentsRead)},
			}}
			workflow := apiv1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "reference-reader"},
				DSLVersion: supportmatrix.NextDSLVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle: "example", Start: tc.task.Name, Tasks: []apiv1.Task{tc.task},
				},
			}
			report := &Report{}
			ix.checkWorkflow(report, workflow, "workflow.yaml", false)

			var got []Issue
			for _, issue := range report.Issues {
				if issue.Code == errorCapabilityRuntimeSupport {
					got = append(got, issue)
				}
			}
			if tc.wantError {
				if len(got) != 1 {
					t.Fatalf("runtime-support errors = %v, want one; report: %s", got, joinIssues(report))
				}
				want := `task "read-reference" declares capability "contents:read" in a scratch workspace`
				if !strings.Contains(got[0].Message, want) {
					t.Errorf("runtime-support error = %q, want %q", got[0].Message, want)
				}
			} else if len(got) != 0 {
				t.Errorf("runtime-support errors = %v, want none", got)
			}
		})
	}
}

func TestCapabilityRuntimeSupportCodeStable(t *testing.T) {
	if got, want := errorCapabilityRuntimeSupport, WarningCode("WF019"); got != want {
		t.Fatalf("errorCapabilityRuntimeSupport = %q, want stable code %q", got, want)
	}
	if errorCapabilityRuntimeSupport == WarningGateCompletionHidesFailure {
		t.Fatalf("errorCapabilityRuntimeSupport duplicates %q", WarningGateCompletionHidesFailure)
	}
}

func TestSubprocessTimeoutCodeStable(t *testing.T) {
	if got, want := WarningSubprocessTimeout, WarningCode("WF021"); got != want {
		t.Fatalf("WarningSubprocessTimeout = %q, want stable code %q", got, want)
	}
	for _, other := range []WarningCode{errorCapabilityRuntimeSupport, WarningGateCompletionHidesFailure} {
		if WarningSubprocessTimeout == other {
			t.Fatalf("WarningSubprocessTimeout duplicates %q", other)
		}
	}
}

func TestSubprocessTimeoutWarningWiredIntoValidate(t *testing.T) {
	tests := []struct {
		name        string
		task        apiv1.Task
		wantWarning bool
	}{
		{
			name: "make target with GO_TEST_TIMEOUT override at or above stage timeout warns (#3377)",
			task: apiv1.Task{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "Run CI.",
				Run:            &apiv1.DeterministicRun{Command: []string{"make", "ci"}, Env: map[string]string{"GO_TEST_TIMEOUT": "30m"}},
				TimeoutSeconds: 1500,
			},
			wantWarning: true,
		},
		{
			name: "stage budget clearing the declared subprocess ceiling is clean",
			task: apiv1.Task{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "Run CI.",
				Run:            &apiv1.DeterministicRun{Command: []string{"make", "ci"}, Env: map[string]string{"GO_TEST_TIMEOUT": "30m"}},
				TimeoutSeconds: 2400,
			},
		},
		{
			name: "bare make target with no declared override is clean (deliberately narrow detection)",
			task: apiv1.Task{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "Run CI.",
				Run:            &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
				TimeoutSeconds: 1500,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ix := newIndex()
			ix.gaggles["example"] = apiv1.Gaggle{Spec: apiv1.GaggleSpec{}}
			workflow := apiv1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "example-workflow"},
				DSLVersion: supportmatrix.NextDSLVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle: "example", Start: tc.task.Name, Tasks: []apiv1.Task{tc.task},
				},
			}
			report := &Report{}
			ix.checkWorkflow(report, workflow, "workflow.yaml", false)

			var got []Issue
			for _, issue := range report.Issues {
				if issue.Code == WarningSubprocessTimeout {
					got = append(got, issue)
				}
			}
			if tc.wantWarning {
				if len(got) != 1 {
					t.Fatalf("subprocess-timeout warnings = %v, want one; report: %s", got, joinIssues(report))
				}
				if got[0].Severity != Warning {
					t.Fatalf("severity = %q, want warning", got[0].Severity)
				}
			} else if len(got) != 0 {
				t.Fatalf("subprocess-timeout warnings = %v, want none", got)
			}
		})
	}
}

func TestAdditionalReposCapabilityRuntimeSupportForAgenticGate(t *testing.T) {
	tests := []struct {
		name      string
		workspace apiv1.WorkspaceMode
		wantError bool
	}{
		{name: "scratch rejected", workspace: apiv1.WorkspaceScratch, wantError: true},
		{name: "repo supported", workspace: apiv1.WorkspaceRepo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ix := newIndex()
			ix.gaggles["example"] = apiv1.Gaggle{Spec: apiv1.GaggleSpec{
				AdditionalRepos: []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "reference"}},
			}}
			ix.goobers["reviewer"] = apiv1.Goober{Spec: apiv1.GooberSpec{
				Gaggle: "example", Capabilities: []string{string(capability.ContentsRead)},
			}}
			workflow := apiv1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "reference-review"},
				DSLVersion: supportmatrix.NextDSLVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle: "example",
					Start:  "prepare",
					Tasks: []apiv1.Task{{
						Name: "prepare", Type: apiv1.TaskDeterministic, Goal: "Prepare.",
						Run: &apiv1.DeterministicRun{Command: []string{"prepare"}}, Next: "review",
					}},
					Gates: []apiv1.Gate{{
						Name: "review", Evaluator: apiv1.EvaluatorAgentic,
						Agentic:  &apiv1.AgenticGate{Goober: "reviewer", Workspace: tc.workspace},
						Branches: map[string]string{"pass": "", "fail": wf.TargetAbort},
					}},
				},
			}
			report := &Report{}
			ix.checkWorkflow(report, workflow, "workflow.yaml", false)

			var got []Issue
			for _, issue := range report.Issues {
				if issue.Code == errorCapabilityRuntimeSupport {
					got = append(got, issue)
				}
			}
			if tc.wantError {
				if len(got) != 1 {
					t.Fatalf("runtime-support errors = %v, want one; report: %s", got, joinIssues(report))
				}
				want := `gate "review" reviewer goober "reviewer" declares capability "contents:read" in a scratch workspace`
				if !strings.Contains(got[0].Message, want) {
					t.Errorf("runtime-support error = %q, want %q", got[0].Message, want)
				}
			} else if len(got) != 0 {
				t.Errorf("runtime-support errors = %v, want none", got)
			}
		})
	}
}

// TestReadOnlyReferenceRepoMustNotBeProject asserts the MGV-10 (#1285) coherence
// check: a repo cannot be both the gaggle's read-write Project and a read-only
// AdditionalRepos reference, and a reference repo must not be listed twice.
func TestReadOnlyReferenceRepoMustNotBeProject(t *testing.T) {
	ix := newIndex()
	ix.gaggles["site"] = apiv1.Gaggle{
		Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "site"},
			AdditionalRepos: []apiv1.RepoRef{
				{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "site"},    // == Project
				{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "goobers"}, // fine
				{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "goobers"}, // duplicate
			},
		},
	}
	report := &Report{}
	ix.checkGaggleAdditionalRepos(report)

	all := joinIssues(report)
	for _, want := range []string{
		"same repository as spec.project",
		"repeats repository",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("expected an error mentioning %q; full report:\n%s", want, all)
		}
	}
}

func TestStageTimeoutCoherenceSurfacesInValidate(t *testing.T) {
	ix := newIndex()
	ix.gaggles["example"] = apiv1.Gaggle{}
	report := &Report{}
	workflow := apiv1.Workflow{
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example",
			Start:  "queue-watch",
			Tasks: []apiv1.Task{{
				Name: "queue-watch",
				Type: apiv1.TaskDeterministic,
				Goal: "Wait for the queue.",
				Run:  &apiv1.DeterministicRun{Command: []string{"watch-queue"}},
				Inputs: map[string]string{
					"pollTimeoutSeconds": "30m",
				},
			}},
		},
	}
	workflow.Name = "queue-review"
	ix.checkWorkflow(report, workflow, "workflow.yaml", false)

	var found bool
	for _, issue := range report.Issues {
		if issue.Severity == Error && strings.Contains(issue.Message, "effective stage timeout 10m0s") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("timeout-coherence diagnostic not surfaced: %v", report.Issues)
	}
}

func TestGooberFeatureDefinitionsUseReferencedWorkflowVersions(t *testing.T) {
	ix := newIndex()
	for _, definition := range []apiv1.Workflow{
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Workflow"},
			ObjectMeta: metav1.ObjectMeta{Name: "legacy"},
			DSLVersion: supportmatrix.CurrentDSLVersion,
			Spec:       apiv1.WorkflowSpec{Gaggle: "example"},
		},
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Workflow"},
			ObjectMeta: metav1.ObjectMeta{Name: "next"},
			DSLVersion: supportmatrix.NextDSLVersion,
			Spec:       apiv1.WorkflowSpec{Gaggle: "example"},
		},
	} {
		identity := workflowIdentity{gaggle: definition.Spec.Gaggle, name: definition.Name}
		ix.workflows[identity] = indexedWorkflow{definition: definition}
	}

	definitions := ix.featureDefinitionsForGoober(apiv1.GooberSpec{
		Gaggle:    "example",
		Workflows: []string{"next"},
	})
	if len(definitions) != 1 || definitions[0].DSLVersion != supportmatrix.NextDSLVersion {
		t.Fatalf("feature definitions = %+v, want only DSL %q", definitions, supportmatrix.NextDSLVersion)
	}
}

// TestWorkflowLessObjectsResolveAtNewestSupportedDSLVersion pins the #3297
// fallback: a gaggle (or goober) with zero workflows must have its features
// checked at the newest supported DSL version. The pre-#3297 fallback was an
// unpinned wf.Definition{}, which the version router rewrote to
// supportmatrix.CurrentDSLVersion ("1.4", deprecated) — so every workflow-less
// gaggle would fail validation the moment 1.4 turns unsupported (declared for
// v0.5.0), with an error its author cannot act on because GaggleSpec has no
// dslVersion field.
func TestWorkflowLessObjectsResolveAtNewestSupportedDSLVersion(t *testing.T) {
	ix := newIndex()

	assertNewestSupported := func(kind string, definitions []wf.Definition) {
		t.Helper()
		if len(definitions) != 1 {
			t.Fatalf("%s definitions = %+v, want exactly one fallback probe", kind, definitions)
		}
		got := definitions[0].DSLVersion
		if got == "" || got == supportmatrix.CurrentDSLVersion {
			t.Fatalf("%s fallback DSL version = %q; must not be unpinned or the deprecated %q",
				kind, got, supportmatrix.CurrentDSLVersion)
		}
		if got != supportmatrix.NextDSLVersion {
			t.Fatalf("%s fallback DSL version = %q, want newest supported %q",
				kind, got, supportmatrix.NextDSLVersion)
		}
	}

	assertNewestSupported("gaggle", ix.featureDefinitionsForGaggle("workflow-less"))
	assertNewestSupported("goober", ix.featureDefinitionsForGoober(apiv1.GooberSpec{Gaggle: "workflow-less"}))
}

func TestAcceptedButInertWorkflowFieldEmitsCodedWarning(t *testing.T) {
	v := newV(t)
	report, err := v.ValidateDir("testdata/config-warnings")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("warnings must not fail validation:\n%s", joinIssues(report))
	}

	var warnings []CodedWarning
	for _, warning := range report.Warnings() {
		if warning.Code == WarningCompatibility {
			warnings = append(warnings, warning)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("compatibility warnings = %+v, want expectedOutputs warning", warnings)
	}
	want := "expectedOutputs is declared but the stage has no inputs.resultFile to emit it through"
	if !strings.Contains(warnings[0].Explanation, want) {
		t.Errorf("warnings = %+v, want explanation containing %q", warnings, want)
	}
}

// TestExplicitZeroMaxRunsPerHourWarns reproduces #3360: a workflow that
// writes readiness.maxRunsPerHour: 0 expecting "unlimited" (by analogy to
// instance.yaml's runConditions.maxParallelRuns, where 0 does mean
// unlimited) instead gets silently throttled to the scheduler's default of
// 10/hour. `goobers validate` must surface this as a non-fatal warning
// naming both the actual behavior and the asymmetric field, and must NOT
// warn when the field is simply omitted or set to a positive value — the
// overwhelming majority of real workflows never set it at all.
func TestExplicitZeroMaxRunsPerHourWarns(t *testing.T) {
	const configTemplate = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: example
spec:
  instance:
    name: example
    environment: dev
  gaggles:
    - acme
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
kind: Workflow
metadata:
  name: build
spec:
  gaggle: acme
  triggers:
    - type: manual
  start: build
%s  tasks:
    - name: build
      type: deterministic
      goal: Build the project.
      run:
        command: ["true"]
        workspace: scratch
`

	tests := []struct {
		name      string
		readiness string
		wantWarn  bool
	}{
		{name: "explicit zero warns", readiness: "  readiness:\n    maxRunsPerHour: 0\n", wantWarn: true},
		{name: "omitted does not warn", readiness: "", wantWarn: false},
		{name: "explicit positive does not warn", readiness: "  readiness:\n    maxRunsPerHour: 5\n", wantWarn: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := fmt.Sprintf(configTemplate, tc.readiness)
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}

			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			if report.HasErrors() {
				t.Fatalf("warning must not fail validation:\n%s", joinIssues(report))
			}

			var got []CodedWarning
			for _, warning := range report.Warnings() {
				if warning.Code == WarningZeroMaxRunsPerHour {
					got = append(got, warning)
				}
			}
			if !tc.wantWarn {
				if len(got) != 0 {
					t.Fatalf("WF020 warnings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("WF020 warnings = %+v, want exactly one", got)
			}
			for _, want := range []string{"maxRunsPerHour", "unlimited", "10", "maxParallelRuns"} {
				if !strings.Contains(got[0].Explanation, want) {
					t.Errorf("warning explanation = %q, want it to contain %q", got[0].Explanation, want)
				}
			}
		})
	}
}

func TestWorkflowSchemaRejectsRunImageFixture(t *testing.T) {
	const fixture = "testdata/config-run-image"
	report, err := newV(t).ValidateDir(fixture)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("workflow with run.image passed validation")
	}
	all := joinIssues(report)
	for _, want := range []string{"/spec/tasks/0/run", "image", "additionalProperties"} {
		if !strings.Contains(all, want) {
			t.Errorf("validation report = %q, want clear run.image rejection containing %q", all, want)
		}
	}

	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(fixture)); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(root, "gaggles", "example", "workflows", "container-build.yaml")
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	withoutImage := strings.Replace(string(raw), "        image: alpine:3.20\n", "", 1)
	if withoutImage == string(raw) {
		t.Fatal("test setup did not remove run.image")
	}
	if err := os.WriteFile(workflowPath, []byte(withoutImage), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = newV(t).ValidateDir(root)
	if err != nil {
		t.Fatalf("ValidateDir without run.image: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("same workflow without run.image must remain valid:\n%s", joinIssues(report))
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
			name:              "runner-only capability",
			manifestGaggles:   "    - acme",
			workflowGaggle:    "acme",
			capability:        "configrepo:read",
			writeInstructions: true,
			want: []string{
				`foreign.yaml Goober/coder: spec.capabilities contains runner-only capability "configrepo:read"`,
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
  annotations:
    goobers.dev/allow-preview-features: "true"
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
		ErrorRemovedFeature,
		WarningModelFallback,
		WarningSkillPackageCollision,
	}
	want := []WarningCode{"VER001", "VER002", "VER003", "VER004", "MODEL002", "SKILL001"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("warning code %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFeatureSupportLevelsUseIssueChannel(t *testing.T) {
	tests := []struct {
		name         string
		feature      wf.Feature
		allowPreview bool
		wantCode     WarningCode
		wantSeverity Severity
		wantIssue    bool
	}{
		{
			name:      "ga",
			feature:   wf.Feature{ID: "stable", Level: wf.SupportGA, SinceVersion: "v1.0.0"},
			wantIssue: false,
		},
		{
			name:         "preview",
			feature:      wf.Feature{ID: "new-field", Level: wf.SupportPreview, SinceVersion: "v1.2.0"},
			allowPreview: true,
			wantCode:     WarningPreviewFeature,
			wantSeverity: Warning,
			wantIssue:    true,
		},
		{
			name: "deprecated",
			feature: wf.Feature{
				ID:                   "old-field",
				Level:                wf.SupportDeprecated,
				SinceVersion:         "v1.3.0",
				Replacement:          "new-field",
				RemovalTargetVersion: "v2.0.0",
			},
			wantCode:     WarningDeprecatedFeature,
			wantSeverity: Warning,
			wantIssue:    true,
		},
		{
			name: "removed",
			feature: wf.Feature{
				ID:                    "removed-field",
				Level:                 wf.SupportRemoved,
				SinceVersion:          "v2.0.0",
				LastSupportingVersion: "v1.9.0",
			},
			wantCode:     ErrorRemovedFeature,
			wantSeverity: Error,
			wantIssue:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := &Report{}
			report.addFeatureDiagnostics(
				"workflow.yaml",
				"example",
				"Workflow",
				"feature-level",
				wf.CheckFeatureSupport(wf.Definition{}, []wf.Feature{tc.feature}, tc.allowPreview),
			)
			if !tc.wantIssue {
				if len(report.Issues) != 0 {
					t.Fatalf("issues = %+v, want none", report.Issues)
				}
				return
			}
			if len(report.Issues) != 1 {
				t.Fatalf("issues = %+v, want one", report.Issues)
			}
			issue := report.Issues[0]
			if issue.Code != tc.wantCode || issue.Severity != tc.wantSeverity {
				t.Fatalf("issue = %+v, want code %q severity %q", issue, tc.wantCode, tc.wantSeverity)
			}
			if !strings.Contains(issue.Message, string(tc.feature.ID)) {
				t.Errorf("message = %q, want feature name %q", issue.Message, tc.feature.ID)
			}
		})
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
	if got := cliWarning.String(); got != "WARNING Workflow/deploy: configuration uses a compatibility path" {
		t.Fatalf("CLI warning String() = %q", got)
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

func TestGaggleSchemaAcceptsSelfIdentity(t *testing.T) {
	v := newV(t)
	gaggle := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Gaggle",
		"metadata": {"name": "web"},
		"spec": {
			"selfIdentity": "web-bot",
			"project": {"provider": "github", "owner": "acme", "name": "web"},
			"backlog": {"provider": "github", "project": "acme/web"},
			"isolation": {"namespace": "gaggle-web"}
		}
	}`
	if err := v.ValidateJSON("gaggle.schema.json", []byte(gaggle)); err != nil {
		t.Fatalf("gaggle selfIdentity rejected: %v", err)
	}
}

func TestGaggleSchemaAcceptsWorkcopiesRoot(t *testing.T) {
	v := newV(t)
	gaggle := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Gaggle",
		"metadata": {"name": "web"},
		"spec": {
			"project": {"provider": "github", "owner": "acme", "name": "web"},
			"backlog": {"provider": "github", "project": "acme/web"},
			"isolation": {"namespace": "gaggle-web"},
			"workcopies": {"root": "/g"}
		}
	}`
	if err := v.ValidateJSON("gaggle.schema.json", []byte(gaggle)); err != nil {
		t.Fatalf("gaggle workcopies root rejected: %v", err)
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

func TestRunControlSchemas(t *testing.T) {
	v := newV(t)
	gaggle := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Gaggle",
		"metadata": {"name": "web"},
		"spec": {
			"project": {"provider": "github", "owner": "acme", "name": "web"},
			"backlog": {"provider": "github", "project": "acme/web"},
			"isolation": {"namespace": "gaggle-web"},
			"runControls": {"maxRepasses": 4, "stalledRunTimeout": "2h", "maxRunDuration": "8h"}
		}
	}`
	if err := v.ValidateJSON("gaggle.schema.json", []byte(gaggle)); err != nil {
		t.Fatalf("gaggle runControls rejected: %v", err)
	}

	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "build"},
		"spec": {
			"gaggle": "web",
			"triggers": [{"type": "manual"}],
			"runControls": {"maxRepasses": 3, "stalledRunTimeout": "90m", "maxRunDuration": "6h"},
			"start": "review",
			"gates": [{
				"name": "review",
				"evaluator": "automated",
				"automated": {"check": "status-equals"},
				"maxRepasses": 1,
				"branches": {"pass": "", "fail": "review"}
			}]
		}
	}`
	if err := v.ValidateJSON("workflow.schema.json", []byte(workflow)); err != nil {
		t.Fatalf("workflow runControls rejected: %v", err)
	}

	human := strings.Replace(workflow, `"evaluator": "automated",`, `"evaluator": "human",`, 1)
	human = strings.Replace(human, `"automated": {"check": "status-equals"},`, `"human": {},`, 1)
	if err := v.ValidateJSON("workflow.schema.json", []byte(human)); err == nil {
		t.Fatal("human gate maxRepasses was accepted")
	}
	if err := v.ValidateJSON("workflow.schema.json", []byte(strings.Replace(workflow, `"maxRepasses": 3`, `"maxRepasses": 0`, 1))); err == nil {
		t.Fatal("zero workflow maxRepasses was accepted")
	}
}

// TestGaggleSchemaSandboxAndCheckout covers the two v2 cloud-ladder additions
// to the Gaggle schema: the per-gaggle sandbox posture override (#1305) and
// the accepted-but-inert repo checkout declaration (#649).
func TestGaggleSchemaSandboxAndCheckout(t *testing.T) {
	v := newV(t)
	gaggle := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Gaggle",
		"metadata": {"name": "example"},
		"spec": {
			"project": {"provider": "github", "owner": "acme", "name": "web"CHECKOUT},
			"backlog": {"provider": "github", "project": "acme/web"},
			"isolation": {"namespace": "gaggle-example"}SANDBOX
		}
	}`
	for _, tc := range []struct {
		name     string
		checkout string
		sandbox  string
		wantErr  bool
	}{
		{name: "neither declared"},
		{
			name:     "sparse checkout and enforced sandbox",
			checkout: `, "checkout": {"sparse": ["services/web", "docs"]}`,
			sandbox:  `, "sandbox": {"agentic": "enforced"}`,
		},
		{name: "sandbox disabled", sandbox: `, "sandbox": {"agentic": "disabled"}`},
		{name: "sandbox empty object", sandbox: `, "sandbox": {}`},
		{
			name:    "unknown sandbox posture rejected",
			sandbox: `, "sandbox": {"agentic": "paranoid"}`,
			wantErr: true,
		},
		{
			name:    "unknown sandbox key rejected",
			sandbox: `, "sandbox": {"deterministic": "enforced"}`,
			wantErr: true,
		},
		{
			name:     "empty sparse path rejected",
			checkout: `, "checkout": {"sparse": [""]}`,
			wantErr:  true,
		},
		{
			name:     "unknown checkout key rejected",
			checkout: `, "checkout": {"partial": true}`,
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(gaggle, "CHECKOUT", tc.checkout, 1)
			doc = strings.Replace(doc, "SANDBOX", tc.sandbox, 1)
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

// TestGaggleCheckoutSparseValidatesCleanly pins that the local runner now
// honors project.checkout.sparse (#649): a well-formed declaration on both
// the project repo and an additionalRepos entry validates without any
// error or warning naming "checkout".
func TestGaggleCheckoutSparseValidatesCleanly(t *testing.T) {
	gaggleYAML := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project:
    provider: github
    owner: acme
    name: web
    checkout:
      sparse: [services/web, docs]
  additionalRepos:
    - provider: github
      owner: acme
      name: assets
      checkout:
        sparse: [images]
  backlog:
    provider: github
    project: acme/web
  isolation:
    namespace: gaggle-example
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gaggle.yaml"), []byte(gaggleYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	for _, issue := range report.Issues {
		if strings.Contains(issue.Message, "checkout") || strings.Contains(issue.Message, "sparse") {
			t.Errorf("well-formed checkout.sparse must validate cleanly, got: %v", issue)
		}
	}
}

// TestGaggleCheckoutSparseRejectsInvalidCones covers every malformed-cone
// case (#649): an absolute path, an empty list, a glob pattern, a ".."
// traversal segment, and a duplicate entry are all validation errors with
// actionable messages, on both spec.project.checkout and an additionalRepos
// entry.
func TestGaggleCheckoutSparseRejectsInvalidCones(t *testing.T) {
	cases := []struct {
		name       string
		sparseYAML string
		wantSubstr string
	}{
		{"empty list", "checkout:\n      sparse: []", "must declare at least one cone"},
		{"absolute path", "checkout:\n      sparse: [/services/web]", "repo-relative, not absolute"},
		{"glob pattern", "checkout:\n      sparse: [\"services/*\"]", "does not support glob patterns"},
		{"parent traversal", "checkout:\n      sparse: [../escape]", `must not contain ".." segments`},
		{"empty cone string", "checkout:\n      sparse: [\"\"]", "must not be empty"},
		{"duplicate cone", "checkout:\n      sparse: [services/web, services/web]", "duplicates cone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gaggleYAML := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project:
    provider: github
    owner: acme
    name: web
    ` + tc.sparseYAML + `
  backlog:
    provider: github
    project: acme/web
  isolation:
    namespace: gaggle-example
`
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gaggle.yaml"), []byte(gaggleYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			var found bool
			for _, issue := range report.Issues {
				if issue.Severity != Error {
					continue
				}
				if issue.Code == errorGaggleCheckoutSparse && strings.Contains(issue.Message, tc.wantSubstr) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected an error containing %q, got: %v", tc.wantSubstr, report.Issues)
			}
		})
	}
}
