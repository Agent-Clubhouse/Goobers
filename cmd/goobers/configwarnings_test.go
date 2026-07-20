package main

import (
	"context"
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
	statusWarnings := warningLines(statusOut)
	upWarnings := warningLines(upOut.String())
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
	want := `{"warnings":[{"code":"MODEL002","severity":"warning","scope":"Goober/coder","explanation":"requested model is unavailable; using the harness default"}],"runs":[]}` + "\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestWarningFreeCommandsPrintNoWarningLines(t *testing.T) {
	root := initDeterministicDemo(t)

	statusCode, statusOut, statusErr := runArgs(t, "status", root)
	if statusCode != 0 {
		t.Fatalf("status code = %d, stderr = %q", statusCode, statusErr)
	}
	if warnings := warningLines(statusOut); len(warnings) != 0 {
		t.Fatalf("status printed warnings for warning-free config: %#v", warnings)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var upOut, upErr strings.Builder
	if code := runUpContext(ctx, []string{"--quiet", root}, &upOut, &upErr); code != 0 {
		t.Fatalf("up code = %d, stderr = %q", code, upErr.String())
	}
	if warnings := warningLines(upOut.String()); len(warnings) != 0 {
		t.Fatalf("up printed warnings for warning-free config: %#v", warnings)
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
	if got := warningLines(statusErr); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("status warnings = %#v, want %#v", got, want)
	}
	if got := warningLines(upErr.String()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("up warnings = %#v, want %#v", got, want)
	}
}
