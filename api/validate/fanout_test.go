package validate

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// fanOutSelectiveWorkflow is the design §5.1 example
// (docs/design/plan-driven-dynamic-fan-out.md).
const fanOutSelectiveWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: selective-review
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: triage
  tasks:
    - name: triage
      type: deterministic
      goal: Decide which review lenses apply to this change.
      run:
        command: ["lens-selector", "--json", "--out", "lenses.json"]
        workspace: repo-readonly
      artifactSlots:
        - name: lenses
          mediaType: application/json
      next: review
    - {name: review-baseline, type: agentic, goober: coder, goal: Baseline review., workspace: repo-readonly, next: "@join"}
    - {name: review-security, type: agentic, goober: coder, goal: Security review., workspace: repo-readonly, next: "@join"}
    - {name: review-migration, type: agentic, goober: coder, goal: Migration review., workspace: repo-readonly, next: "@join"}
    - {name: review-ui, type: agentic, goober: coder, goal: UI review., workspace: repo-readonly, next: "@join"}
    - name: collate
      type: agentic
      goober: coder
      goal: Collate every lens that ran; note the lenses that were skipped.
      workspace: scratch
      next: ""
  parallels:
    - name: review
      failurePolicy: continue_on_error
      branchTimeoutSeconds: 1800
      maxConcurrentBranches: 3
      join: collate
      selection:
        source: { task: triage, slot: lenses }
      branches:
        - { name: baseline,  start: review-baseline }
        - { name: security,  start: review-security,  optional: true }
        - { name: migration, start: review-migration, optional: true }
        - { name: ui,        start: review-ui,        optional: true }
`

// fanOutMapWorkflow is the design §6.1 example.
const fanOutMapWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: shard-map
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: plan
  tasks:
    - name: plan
      type: agentic
      goober: coder
      goal: Select the immutable test-shard roster.
      workspace: repo-readonly
      artifactSlots:
        - name: shards
          mediaType: application/json
      next: run-shards
    - name: run-shard
      type: deterministic
      goal: Run one shard.
      run:
        command: ["shard-runner"]
        workspace: repo-readonly
      next: "@join"
    - name: collate
      type: agentic
      goober: coder
      goal: Summarize every shard result in identity order.
      workspace: scratch
      next: ""
  maps:
    - name: run-shards
      source: { task: plan, slot: shards }
      task: run-shard
      bindings:
        shard: item
        shardIdentity: identity
      failurePolicy: continue_on_error
      itemTimeoutSeconds: 1800
      maxConcurrent: 4
      join: collate
`

func validationErrors(report *Report) []Issue {
	var errs []Issue
	for _, issue := range report.Issues {
		if issue.Severity == Error {
			errs = append(errs, issue)
		}
	}
	return errs
}

func TestValidateFanOutDesignExamples(t *testing.T) {
	for name, doc := range map[string]string{"selective parallel": fanOutSelectiveWorkflow, "map": fanOutMapWorkflow} {
		t.Run(name, func(t *testing.T) {
			raw, err := yaml.YAMLToJSON([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if err := newV(t).ValidateJSON("workflow.schema.json", raw); err != nil {
				t.Fatalf("workflow.schema.json rejected the design example: %v", err)
			}
			if errs := validationErrors(validateDSL30(t, dsl30Config(true, doc))); len(errs) != 0 {
				t.Fatalf("design example with preview acknowledgement must validate, got %+v", errs)
			}
			errs := validationErrors(validateDSL30(t, dsl30Config(false, doc)))
			if len(errs) == 0 || !strings.Contains(issueMessages(errs), "is preview and requires explicit") {
				t.Fatalf("design example without preview acknowledgement must be refused as preview, got %+v", errs)
			}
		})
	}
}

func TestFanOutRejectedBeforeDSL31(t *testing.T) {
	for name, doc := range map[string]string{"selective parallel": fanOutSelectiveWorkflow, "map": fanOutMapWorkflow} {
		for _, version := range []string{"2.0", "3.0"} {
			t.Run(name+"/"+version, func(t *testing.T) {
				// Drop the 3.1-only artifact slot so the fan-out field alone
				// decides the verdict.
				older := strings.Replace(doc, `dslVersion: "3.1"`, `dslVersion: "`+version+`"`, 1)
				older = strings.Replace(older, "      artifactSlots:\n", "      outbox: [plan.json]\n      x-slots:\n", 1)
				older = removeLinesWithPrefix(older, "      x-slots:", "        - name: lenses", "        - name: shards", "          mediaType: application/json")
				raw, err := yaml.YAMLToJSON([]byte(older))
				if err != nil {
					t.Fatal(err)
				}
				if err := newV(t).ValidateJSON("workflow.schema.json", raw); err == nil {
					t.Fatalf("workflow.schema.json accepted a fan-out construct at DSL %s", version)
				}
				if errs := validationErrors(validateDSL30(t, dsl30Config(true, older))); !strings.Contains(issueMessages(errs), `requires dslVersion "3.1"`) {
					t.Fatalf("validation must refuse a fan-out construct at DSL %s by version, got %+v", version, errs)
				}
			})
		}
	}
}

func TestValidateFanOutCompilerDiagnosticCarriesSlotLocation(t *testing.T) {
	doc := strings.Replace(fanOutMapWorkflow, "          mediaType: application/json\n      next: run-shards", "          mediaType: text/plain\n      next: run-shards", 1)
	config := dsl30Config(true, doc)
	report := validateDSL30(t, config)
	issue := findIssueContaining(t, report, `map "run-shards" source slot "shards" of task "plan" must declare mediaType "application/json"`)
	assertIssueAtAnchor(t, issue, config, "shards\n          mediaType: text/plain")
}

func issueMessages(issues []Issue) string {
	messages := make([]string, 0, len(issues))
	for _, issue := range issues {
		messages = append(messages, issue.Message)
	}
	return strings.Join(messages, "\n")
}

func removeLinesWithPrefix(doc string, prefixes ...string) string {
	lines := strings.Split(doc, "\n")
	kept := lines[:0]
	for _, line := range lines {
		drop := false
		for _, prefix := range prefixes {
			drop = drop || strings.HasPrefix(line, prefix)
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
