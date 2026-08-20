package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func withValidationIssues(t *testing.T, issues ...validate.Issue) {
	t.Helper()
	previous := loadConfigDirectory
	loadConfigDirectory = func(dir string) (*instance.ConfigSet, *validate.Report, error) {
		set, report, err := previous(dir)
		if report != nil {
			cloned := *report
			cloned.Issues = append(append([]validate.Issue(nil), report.Issues...), issues...)
			report = &cloned
		}
		return set, report, err
	}
	t.Cleanup(func() { loadConfigDirectory = previous })
}

func warningLines(output string) []string {
	var warnings []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "WARNING ") {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

func TestAppendGooberHarnessWarningsMapsModelFallback(t *testing.T) {
	report := &validate.Report{}
	coded, err := appendGooberHarnessWarnings(report, []gooberHarnessWarning{{
		Goober: "coder",
		Warning: harness.ConfigWarning{
			Kind:    harness.ConfigWarningModelFallback,
			Message: `requested model "retired-model" is unavailable; using the harness default`,
		},
	}})
	if err != nil {
		t.Fatalf("appendGooberHarnessWarnings: %v", err)
	}
	if len(report.Issues) != 1 ||
		report.Issues[0].Code != validate.WarningModelFallback ||
		report.Issues[0].Kind != "Goober" ||
		report.Issues[0].Name != "coder" {
		t.Fatalf("report issues = %+v", report.Issues)
	}
	if len(coded) != 1 ||
		coded[0].Code != validate.WarningModelFallback ||
		coded[0].Scope != "Goober/coder" {
		t.Fatalf("coded warnings = %+v", coded)
	}
}

func TestAppendSkillPackageCollisionWarnings(t *testing.T) {
	instanceRoot := t.TempDir()
	configDir := filepath.Join(instanceRoot, "config")
	for _, dir := range []string{
		filepath.Join(instanceRoot, "skills", "testing"),
		filepath.Join(configDir, "gaggles", "alpha", "skills", "testing"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	report := &validate.Report{}
	warnings, err := appendSkillPackageCollisionWarnings(configDir, report, map[string]apiv1.GooberSpec{
		"coder": {Gaggle: "alpha", Skills: []string{"testing", "testing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 ||
		warnings[0].Code != validate.WarningSkillPackageCollision ||
		warnings[0].Scope != "Gaggle/alpha" ||
		!strings.Contains(warnings[0].Explanation, "gaggle-level definition takes effect") {
		t.Fatalf("warnings = %+v", warnings)
	}
	if len(report.Issues) != 1 || report.Issues[0].Severity != validate.Warning {
		t.Fatalf("report issues = %+v", report.Issues)
	}
}

func TestValidateWarnsWhenGaggleSkillShadowsSharedPackage(t *testing.T) {
	root := initDemo(t)
	for _, dir := range []string{
		filepath.Join(root, "skills", "implement"),
		filepath.Join(root, "config", "gaggles", "example", "skills", "implement"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Testing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(root, "config", "gaggles", "example", "skills", "implement", "support.yaml"),
		[]byte("cases:\n  - retry\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	want := `WARNING SKILL001 Gaggle/example: gaggle-level and instance-level packages both define skill "implement"; the gaggle-level definition takes effect`
	if !strings.Contains(stdout, want) {
		t.Fatalf("validate stdout = %q, want %q", stdout, want)
	}
}

func withoutGeneratedPreviewWarnings(output string) ([]string, int) {
	var warnings []string
	previewCount := 0
	for _, warning := range warningLines(output) {
		if strings.Contains(warning, `: DSL feature "`) &&
			strings.Contains(warning, " is preview and unstable (available since ") {
			previewCount++
			continue
		}
		warnings = append(warnings, warning)
	}
	return warnings, previewCount
}

func TestUpAndStatusPrintIdenticalOrderedWarnings(t *testing.T) {
	root := initDeterministicDemo(t)
	withValidationIssues(t,
		validate.Issue{
			Code:     validate.WarningPreviewFeature,
			Severity: validate.Warning,
			File:     "z-preview.yaml",
			Kind:     "Workflow",
			Name:     "preview-flow",
			Message:  "preview feature may change",
		},
		validate.Issue{
			Code:     validate.WarningDeprecatedFeature,
			Severity: validate.Warning,
			File:     "a-deprecated.yaml",
			Kind:     "Workflow",
			Name:     "legacy-flow",
			Message:  "deprecated feature remains supported",
		},
	)

	statusCode, statusOut, statusErr := runArgs(t, "status", root)
	if statusCode != 0 {
		t.Fatalf("status code = %d, stderr = %q", statusCode, statusErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var upOut, upErr strings.Builder
	if code := runUpContext(ctx, []string{"--quiet", root}, &upOut, &upErr); code != 0 {
		t.Fatalf("up code = %d, stderr = %q", code, upErr.String())
	}

	want := []string{
		"WARNING VER001 a-deprecated.yaml Workflow/legacy-flow: deprecated feature remains supported",
		"WARNING VER002 z-preview.yaml Workflow/preview-flow: preview feature may change",
	}
	statusWarnings, statusPreviewCount := withoutGeneratedPreviewWarnings(statusOut)
	upWarnings, upPreviewCount := withoutGeneratedPreviewWarnings(upOut.String())
	// The demo config is GA (#1196), so it generates no preview notices; the
	// point here is that status and up agree on the (now zero) generated preview
	// notices and surface the identical injected VER001/VER002 warnings.
	if statusPreviewCount != upPreviewCount {
		t.Fatalf("status/up disagree on generated preview notices: status=%d up=%d", statusPreviewCount, upPreviewCount)
	}
	if strings.Join(statusWarnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("status warnings = %#v, want %#v", statusWarnings, want)
	}
	if strings.Join(upWarnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("up warnings = %#v, want %#v", upWarnings, want)
	}

	events, err := journal.ReadInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var journaled []map[string]any
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == "config.validation.warning" {
			journaled = append(journaled, event.Runner)
		}
	}
	if len(journaled) != 2 {
		t.Fatalf("journaled config warnings = %+v, want 2", journaled)
	}
	if journaled[0]["code"] != string(validate.WarningDeprecatedFeature) ||
		journaled[0]["scope"] != "a-deprecated.yaml Workflow/legacy-flow" ||
		journaled[1]["code"] != string(validate.WarningPreviewFeature) ||
		journaled[1]["scope"] != "z-preview.yaml Workflow/preview-flow" {
		t.Fatalf("journaled config warnings = %+v", journaled)
	}
}

func TestStatusJSONIncludesStableWarningShape(t *testing.T) {
	root := initScheduledDemo(t)
	withValidationIssues(t, validate.Issue{
		Code:     validate.WarningModelFallback,
		Severity: validate.Warning,
		Kind:     "Goober",
		Name:     "coder",
		Message:  "requested model is unavailable; using the harness default",
	})

	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json code = %d, stderr = %q", code, stderr)
	}
	var got statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status JSON = %q: %v", stdout, err)
	}
	var nonPreview []validate.CodedWarning
	previewCount := 0
	for _, warning := range got.Warnings {
		if warning.Code == validate.WarningPreviewFeature {
			previewCount++
			continue
		}
		nonPreview = append(nonPreview, warning)
	}
	if previewCount != 0 || len(nonPreview) != 1 ||
		nonPreview[0].Code != "MODEL002" ||
		nonPreview[0].Severity != "warning" ||
		nonPreview[0].Scope != "Goober/coder" ||
		nonPreview[0].Explanation != "requested model is unavailable; using the harness default" {
		t.Fatalf("warnings = %+v", got.Warnings)
	}
	if got.Summary == nil || len(got.Runs) != 0 {
		t.Fatalf("summary/runs = %+v / %+v", got.Summary, got.Runs)
	}
}

// TestDemoConfigValidatesCleanWithoutNotices pins the #1196 fix at the command
// level: the demo config uses only GA DSL features, so status and up emit no
// config-validation notices at all (previously every field warned as preview).
func TestDemoConfigValidatesCleanWithoutNotices(t *testing.T) {
	root := initDeterministicDemo(t)

	statusCode, statusOut, statusErr := runArgs(t, "status", root)
	if statusCode != 0 {
		t.Fatalf("status code = %d, stderr = %q", statusCode, statusErr)
	}
	if warnings, previewCount := withoutGeneratedPreviewWarnings(statusOut); len(warnings) != 0 || previewCount != 0 {
		t.Fatalf("status warnings = %#v, preview count = %d; want a clean demo config with no notices (#1196)", warnings, previewCount)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var upOut, upErr strings.Builder
	if code := runUpContext(ctx, []string{"--quiet", root}, &upOut, &upErr); code != 0 {
		t.Fatalf("up code = %d, stderr = %q", code, upErr.String())
	}
	if warnings, previewCount := withoutGeneratedPreviewWarnings(upOut.String()); len(warnings) != 0 || previewCount != 0 {
		t.Fatalf("up warnings = %#v, preview count = %d; want a clean demo config with no notices (#1196)", warnings, previewCount)
	}
}

func TestValidationWarningsDoNotChangeErrorOutcomes(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte("not: valid config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withValidationIssues(t, validate.Issue{
		Code:     validate.WarningCompatibility,
		Severity: validate.Warning,
		File:     "compatibility.yaml",
		Message:  "configuration uses a compatibility path",
	})

	statusCode, _, statusErr := runArgs(t, "status", root)
	if statusCode != 1 {
		t.Fatalf("status code = %d, want validation failure 1; stderr = %q", statusCode, statusErr)
	}
	if !strings.Contains(statusErr, "ERROR") || !strings.Contains(statusErr, "WARNING VER003") {
		t.Fatalf("status stderr = %q, want both error and coded warning", statusErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var upOut, upErr strings.Builder
	if code := runUpContext(ctx, []string{root}, &upOut, &upErr); code != 1 {
		t.Fatalf("up code = %d, want startup failure 1; stderr = %q", code, upErr.String())
	}
	if !strings.Contains(upErr.String(), "ERROR") || !strings.Contains(upErr.String(), "WARNING VER003") {
		t.Fatalf("up stderr = %q, want both error and coded warning", upErr.String())
	}
}

func TestCompileErrorsStillSurfaceIdenticalWarnings(t *testing.T) {
	root := initDeterministicDemo(t)
	replaceInFile(t, filepath.Join(root, "instance.yaml"), "your-org", "acme")
	replaceInFile(t, filepath.Join(root, "instance.yaml"), "your-repo", "widgets")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	for range 2 {
		replaceInFile(t, gagglePath, "your-org", "acme")
		replaceInFile(t, gagglePath, "your-repo", "widgets")
	}
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	workflow := strings.Replace(
		deterministicWorkflowYAML,
		`        command: ["true"]`,
		`        command: ["true"]
      next: compile-check
  gates:
    - name: compile-check
      evaluator: automated
      automated:
        check: unknown-check
      branches:
        pass: ""
        fail: "@abort"`,
		1,
	)
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	withValidationIssues(t, validate.Issue{
		Code:     validate.WarningDeprecatedFeature,
		Severity: validate.Warning,
		File:     "deprecated.yaml",
		Message:  "deprecated feature remains supported",
	})

	statusCode, _, statusErr := runArgs(t, "status", root)
	if statusCode != 1 {
		t.Fatalf("status code = %d, want compile failure 1; stderr = %q", statusCode, statusErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var upOut, upErr strings.Builder
	if code := runUpContext(ctx, []string{root}, &upOut, &upErr); code != 1 {
		t.Fatalf("up code = %d, want compile failure 1; stderr = %q", code, upErr.String())
	}

	want := []string{"WARNING VER001 deprecated.yaml: deprecated feature remains supported"}
	if got, _ := withoutGeneratedPreviewWarnings(statusErr); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("status warnings = %#v, want %#v", got, want)
	}
	if got, _ := withoutGeneratedPreviewWarnings(upErr.String()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("up warnings = %#v, want %#v", got, want)
	}
}
