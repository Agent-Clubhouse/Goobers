package validate

// blankexecutable_test.go covers issue #3661: a deterministic run whose
// command[0] is empty or whitespace-only passed schema validation and
// compilation, and only failed when the runtime handed it to exec.

import (
	"strings"
	"testing"
)

func blankExecutableWorkflow(executable string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: chain
spec:
  gaggle: web
  triggers:
    - type: manual
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: Run CI.
      run:
        command: ["` + executable + `", "ci"]
`
}

func TestValidateRejectsBlankRunExecutable(t *testing.T) {
	for name, executable := range map[string]string{
		"empty":      "",
		"whitespace": "  ",
	} {
		t.Run(name, func(t *testing.T) {
			report := validateDSL30(t, dsl30Config(false, blankExecutableWorkflow(executable)))
			got := joinIssues(report)
			if !strings.Contains(got, "spec/tasks/0/run/command/0") {
				t.Fatalf("diagnostics missing a command[0] rejection:\n%s", got)
			}
		})
	}
}

func TestValidateAcceptsRunExecutableWithArguments(t *testing.T) {
	report := validateDSL30(t, dsl30Config(false, blankExecutableWorkflow("make")))
	if got := joinIssues(report); strings.Contains(got, "run/command") {
		t.Fatalf("valid command reported a diagnostic:\n%s", got)
	}
}
