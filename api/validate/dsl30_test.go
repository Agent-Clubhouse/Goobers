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

// --- Agentic-gate placement (decision 001, #3798) -------------------------

// gatedDSL30Workflow renders a one-task, one-agentic-gate workflow at the
// given dslVersion with the caller's runsOn block on the gate (empty for
// none). The gate reviews with the coder goober the dsl30Config tree already
// declares.
func gatedDSL30Workflow(dslVersion, evaluator, gateRunsOn string) string {
	evaluatorBlock := "      agentic:\n        goober: coder\n"
	branches := "        needs-changes: implement\n"
	if evaluator == "automated" {
		evaluatorBlock = "      automated:\n        check: status-equals\n"
		branches = ""
	}
	return fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: %q
metadata:
  name: gated
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
      next: review
  gates:
    - name: review
      evaluator: %s
%s%s      branches:
        pass: ""
        fail: "@abort"
%s`, dslVersion, evaluator, evaluatorBlock, gateRunsOn, branches)
}

const placedGateRunsOnYAML = "      runsOn:\n        cpu: 1000m\n        memory: 2Gi\n"

func TestValidateTwoPointOhGateRunsOnIsRefused(t *testing.T) {
	report := validateDSL30(t, dsl30Config(false, gatedDSL30Workflow("2.0", "agentic", placedGateRunsOnYAML)))
	var matched bool
	for _, issue := range issuesWithCode(report, errorWorkflowAdmission) {
		if strings.Contains(issue.Message, `gate "review" declares runsOn, which requires dslVersion "3.0"`) {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("issues = %+v, want a WF010 version-gate refusal naming the gate and dslVersion 3.0", report.Issues)
	}
	if len(issuesWithCode(report, errorGateRunsOn)) != 0 {
		t.Fatalf("issues = %+v, want no WF023 on a 2.0 document (the router refuses the field first)", report.Issues)
	}
}

func TestValidateGateRunsOnRules(t *testing.T) {
	t.Run("agentic gate with cpu and memory is accepted clean", func(t *testing.T) {
		report := validateDSL30(t, dsl30Config(true, gatedDSL30Workflow("3.0", "agentic", placedGateRunsOnYAML)))
		for _, issue := range report.Issues {
			if issue.Severity == Error {
				t.Fatalf("issues = %+v, want no errors for a placed agentic gate", report.Issues)
			}
		}
		// The "not yet honoured" WF024 warning that rode here between the
		// DSL half of decision 001 and its engine/pod half is gone with the
		// engine half: a placed gate is now honoured at execution, and the
		// code must not come back under any spelling.
		for _, issue := range report.Issues {
			if issue.Code == "WF024" || strings.Contains(issue.Message, "no execution path honours a gate placement") {
				t.Fatalf("issues = %+v, want no retired WF024 warning on a placed agentic gate", report.Issues)
			}
		}
	})
	t.Run("agentic gate with runsOn but no agentic block is WF023", func(t *testing.T) {
		noReviewer := strings.Replace(gatedDSL30Workflow("3.0", "agentic", placedGateRunsOnYAML), "      agentic:\n        goober: coder\n", "", 1)
		report := validateDSL30(t, dsl30Config(true, noReviewer))
		var matched bool
		for _, issue := range issuesWithCode(report, errorGateRunsOn) {
			if strings.Contains(issue.Message, `gate "review" declares runsOn but has no agentic: block`) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("issues = %+v, want a WF023 naming the missing reviewer block", report.Issues)
		}
	})
	t.Run("automated gate with runsOn is WF023", func(t *testing.T) {
		report := validateDSL30(t, dsl30Config(true, gatedDSL30Workflow("3.0", "automated", placedGateRunsOnYAML)))
		found := issuesWithCode(report, errorGateRunsOn)
		if len(found) != 1 || !strings.Contains(found[0].Message, `gate "review" declares runsOn but its evaluator is "automated"`) {
			t.Fatalf("issues = %+v, want one WF023 naming the non-agentic evaluator", report.Issues)
		}
	})
	t.Run("agentic gate without memory is WF023", func(t *testing.T) {
		report := validateDSL30(t, dsl30Config(true, gatedDSL30Workflow("3.0", "agentic", "      runsOn:\n        cpu: 1000m\n")))
		found := issuesWithCode(report, errorGateRunsOn)
		if len(found) != 1 || !strings.Contains(found[0].Message, `gate "review" declares runsOn without memory:`) {
			t.Fatalf("issues = %+v, want one WF023 naming the missing quantity", report.Issues)
		}
	})
	t.Run("os token on a gate is CAP004", func(t *testing.T) {
		report := validateDSL30(t, dsl30Config(true, gatedDSL30Workflow("3.0", "agentic",
			"      runsOn:\n        cpu: 1000m\n        memory: 2Gi\n        capabilities: [os=linux]\n")))
		found := issuesWithCode(report, errorOSTokenInV3)
		if len(found) != 1 || !strings.Contains(found[0].Message, `gate "review" runsOn.capabilities contains "os=linux"`) {
			t.Fatalf("issues = %+v, want one CAP004 attributed to the gate", report.Issues)
		}
	})
	t.Run("unknown restriction on a gate is CAP005", func(t *testing.T) {
		// The schema pins the restriction enum, so a config tree refuses an
		// unknown token at SCHEMA003; CAP005 is the belt for the API path.
		def := wf.Definition{
			Name: "gate-restriction", Version: 1, DSLVersion: "3.0",
			Spec: apiv1.WorkflowSpec{
				Gaggle: "web", Start: "implement",
				Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "review"}},
				Gates: []apiv1.Gate{{
					Name: "review", Evaluator: apiv1.EvaluatorAgentic,
					Agentic:  &apiv1.AgenticGate{Goober: "coder"},
					RunsOn:   &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi", Restrictions: []string{"network:allow-list"}},
					Branches: map[string]string{"pass": "", "fail": "@abort"},
				}},
			},
		}
		problems := wf.CheckRunsOnRestrictions(def, nil)
		if len(problems) != 1 || !strings.HasPrefix(problems[0], `gate "review" runsOn.restrictions:`) || !strings.Contains(problems[0], `did you mean "network:allowlist"?`) {
			t.Fatalf("CheckRunsOnRestrictions = %v, want one gate-attributed suggestion-carrying problem", problems)
		}
	})
}
