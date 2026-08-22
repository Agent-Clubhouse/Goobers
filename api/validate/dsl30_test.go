package validate

// dsl30_test.go exercises the DSL 3.0 validation surface end to end through
// ValidateDir (dsl-3.0.md §5, issue #3505): the CAP004/CAP005 vocabulary
// errors, the WF022 repo-handoff analysis, the DVL010/DVL011 preview gate on
// the 3.0 version pin itself (§9 item 3), and the version-router refusal of
// 3.0-only fields on a 2.0 document.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

// dsl30Config renders a one-gaggle config tree whose Workflow section is
// caller-supplied. optIn adds the instance preview acknowledgement that a
// preview-level dslVersion pin requires (DVL011 otherwise).
func dsl30Config(optIn bool, workflowYAML string) string {
	annotations := ""
	if optIn {
		annotations = "\n  annotations:\n    goobers.dev/allow-preview-features: \"true\""
	}
	return fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: dsl30%s
spec:
  instance:
    name: dsl30
    environment: dev
  gaggles:
    - web
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: web
spec:
  project:
    provider: github
    owner: acme
    name: web
  backlog:
    provider: github
    project: acme/web
  isolation:
    namespace: gaggle-web
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: coder
spec:
  gaggle: web
  role: developer
  instructions: instructions/coder.md
  capabilities: [agent:model]
---
%s`, annotations, workflowYAML)
}

func validateDSL30(t *testing.T, config string) *Report {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "instructions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions", "coder.md"), []byte("# coder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

func issuesWithCode(report *Report, code WarningCode) []Issue {
	var found []Issue
	for _, issue := range report.Issues {
		if issue.Code == code {
			found = append(found, issue)
		}
	}
	return found
}

const dsl30ImplicitChainWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: chain
spec:
  gaggle: web
  triggers:
    - type: manual
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement.
      capabilities: [agent:model]
      next: local-ci
    - name: local-ci
      type: deterministic
      goal: Run CI.
      run:
        command: ["make", "ci"]
`

func TestValidateDSL30PreviewGate(t *testing.T) {
	// Without the instance opt-in the 3.0 pin is refused — DVL011, closed by
	// default (§9 item 3's DVL path; DVL010/011 machinery gates the preview
	// entry the support matrix carries for 3.0).
	report := validateDSL30(t, dsl30Config(false, dsl30ImplicitChainWorkflow))
	if len(issuesWithCode(report, ErrorPreviewDSLVersionBlocked)) == 0 {
		t.Fatalf("issues = %+v, want a DVL011 error for an un-opted-in 3.0 pin", report.Issues)
	}

	// With the opt-in the pin itself is only the DVL010 advisory…
	report = validateDSL30(t, dsl30Config(true, dsl30ImplicitChainWorkflow))
	if len(issuesWithCode(report, ErrorPreviewDSLVersionBlocked)) != 0 {
		t.Fatalf("issues = %+v, want no DVL011 after the opt-in", report.Issues)
	}
	if len(issuesWithCode(report, WarningPreviewDSLVersionOptedIn)) == 0 {
		t.Fatalf("issues = %+v, want the DVL010 advisory", report.Issues)
	}
	// …and the implicit repo chain is the remaining error: WF022 (§9 item 7,
	// compile half).
	handoffs := issuesWithCode(report, errorRepoHandoff)
	if len(handoffs) != 1 ||
		!strings.Contains(handoffs[0].Message, `task "local-ci" runs on the repo workspace after producer(s) "implement" but declares no repoFrom`) {
		t.Fatalf("issues = %+v, want exactly one WF022 undeclared-chain error", report.Issues)
	}
}

func TestValidateDSL30DeclaredChainIsClean(t *testing.T) {
	declared := strings.Replace(dsl30ImplicitChainWorkflow,
		"      run:\n        command: [\"make\", \"ci\"]\n",
		"      run:\n        command: [\"make\", \"ci\"]\n      repoFrom: implement\n", 1)
	report := validateDSL30(t, dsl30Config(true, declared))
	for _, issue := range report.Issues {
		if issue.Severity == Error {
			t.Fatalf("declared chain must validate clean, got error: %+v", issue)
		}
	}
}

func TestValidateDSL30OSTokenIsCAP004(t *testing.T) {
	workflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: cap004
spec:
  gaggle: web
  triggers:
    - type: manual
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement.
      capabilities: [agent:model]
      runsOn:
        capabilities: [os=windows]
`
	report := validateDSL30(t, dsl30Config(true, workflow))
	found := issuesWithCode(report, errorOSTokenInV3)
	if len(found) != 1 ||
		!strings.Contains(found[0].Message, `runsOn.capabilities contains "os=windows"`) ||
		found[0].Severity != Error {
		t.Fatalf("issues = %+v, want exactly one CAP004 error", report.Issues)
	}
}

func TestValidateDSL30UnknownRestrictionIsCAP005(t *testing.T) {
	workflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: cap005
spec:
  gaggle: web
  triggers:
    - type: manual
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement.
      capabilities: [agent:model]
`
	report := validateDSL30(t, dsl30Config(true, workflow))
	if len(issuesWithCode(report, errorUnknownRestriction)) != 0 {
		t.Fatalf("issues = %+v, want no CAP005 for a clean document", report.Issues)
	}
	// The schema pins the restriction enum, so an unknown token in a config
	// tree is refused at SCHEMA003 before CAP005 can see it — CAP005 is the
	// belt for paths that bypass schema validation (compile via the API).
	// Exercise the CAP005 seam directly through the router.
	def := wf.Definition{
		Name: "restriction", Version: 1, DSLVersion: "3.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement",
				RunsOn: &apiv1.RunsOn{Restrictions: []string{"network:allow-list"}},
			}},
		},
	}
	problems := wf.CheckRunsOnRestrictions(def, nil)
	if len(problems) != 1 || !strings.Contains(problems[0], `did you mean "network:allowlist"?`) {
		t.Fatalf("CheckRunsOnRestrictions = %v, want one suggestion-carrying problem", problems)
	}
}

func TestValidateTwoPointOhDocumentWithRunsOnIsRefused(t *testing.T) {
	workflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: wrong-version
spec:
  gaggle: web
  triggers:
    - type: manual
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement.
      capabilities: [agent:model]
      runsOn:
        os: linux
`
	report := validateDSL30(t, dsl30Config(false, workflow))
	found := issuesWithCode(report, errorWorkflowAdmission)
	var matched bool
	for _, issue := range found {
		if strings.Contains(issue.Message, `declares runsOn, which requires dslVersion "3.0"`) {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("issues = %+v, want a WF010 version-gate refusal naming dslVersion 3.0", report.Issues)
	}
}
