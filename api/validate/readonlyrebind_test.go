package validate

import (
	"strings"
	"testing"
)

func readOnlyAfterRebindTail(gateWorkspace string) string {
	return `      next: select
    - name: select
      type: deterministic
      goal: Select the pull request branch to review.
      run:
        command: ["./select.sh"]
      expectedOutputs:
        - workspaceBranch
      next: review
  gates:
    - name: review
      evaluator: agentic
      agentic:
        goober: author
        workspace: ` + gateWorkspace + `
      branches:
        pass: ""
        fail: "@abort"
        needs-changes: "@abort"
`
}

// TestReadOnlyGateAfterRebindIsWS002 is the #5390 regression: a repo-readonly
// agentic gate reachable from a stage that rebinds the workspace branch passed
// validation, then failed on every run with an engine-centric workspace error.
func TestReadOnlyGateAfterRebindIsWS002(t *testing.T) {
	report := validateCompilerAdmission(t, readOnlyAfterRebindTail("repo-readonly"))
	found := issuesWithCode(report, errorReadOnlyAfterRebind)
	if len(found) != 1 {
		t.Fatalf("issues = %+v, want one WS002", report.Issues)
	}
	issue := found[0]
	if issue.Severity != Error {
		t.Fatalf("WS002 severity = %v, want Error", issue.Severity)
	}
	for _, want := range []string{`gate "review"`, `stage "select"`, "agentic.workspace: repo"} {
		if !strings.Contains(issue.Message, want) {
			t.Errorf("WS002 message missing %q: %s", want, issue.Message)
		}
	}
	if issue.Line == 0 {
		t.Errorf("WS002 has no source line: %+v", issue)
	}
}

func TestWritableGateAfterRebindIsClean(t *testing.T) {
	report := validateCompilerAdmission(t, readOnlyAfterRebindTail("repo"))
	if found := issuesWithCode(report, errorReadOnlyAfterRebind); len(found) != 0 {
		t.Fatalf("issues = %+v, want no WS002 for a writable gate", report.Issues)
	}
}
