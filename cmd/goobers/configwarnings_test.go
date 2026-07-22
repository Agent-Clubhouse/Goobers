package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
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
