package dslmigrate

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const workflowWithUnpinnedCIPoll = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: unpinned
spec:
  gaggle: golden
  triggers:
    - type: backlog-item
  start: poll
  tasks:
    - name: poll
      type: deterministic
      goal: Poll CI.
      run:
        command: ["goobers", "ci-poll"]
      inputs:
        kind: "ci-poll"
        prNumber: "42"
      next: ci
  gates:
    - name: ci
      evaluator: automated
      automated:
        check: ci-status
      branches:
        pass: ""
        fail: "@abort"
        timeout: "@escalate"
`

const workflowWithPinnedCIPoll = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: pinned
spec:
  gaggle: golden
  start: poll
  tasks:
    - name: poll
      type: deterministic
      goal: Poll CI.
      inputs:
        kind: "ci-poll"
      next: ci
  gates:
    - name: ci
      evaluator: automated
      automated:
        check: ci-status
        pollIntervalSeconds: 7
      branches:
        pass: ""
`

const workflowWithNoCIPoll = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: no-ci-poll
spec:
  gaggle: golden
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement the item.
`

func TestMigratePinsUnsetPollInterval(t *testing.T) {
	result, err := Migrate([]byte(workflowWithUnpinnedCIPoll), "2.0")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !result.Changed {
		t.Fatalf("Changed = false, want true")
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], `gate "ci"`) {
		t.Fatalf("Notes = %v, want one note naming gate \"ci\"", result.Notes)
	}
	after := decodeWorkflow(t, result.After)
	if after.DSLVersion != "2.0" {
		t.Fatalf("after dslVersion = %q, want 2.0", after.DSLVersion)
	}
	if after.PollIntervalSeconds != 10 {
		t.Fatalf("after pollIntervalSeconds = %d, want 10 (the DSL 2.0 default made explicit)", after.PollIntervalSeconds)
	}
}

func TestMigrateTransformPreservesUnrelatedSourceBytes(t *testing.T) {
	source := `# workflow docs must stay byte-stable
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4" # keep this comment

metadata:
  name: unpinned
spec:
  gaggle: golden
  start: poll
  tasks:
    - name: poll
      type: deterministic
      goal: >-
        Keep this hand-wrapped text
        on multiple source lines.
      inputs:
        kind: "ci-poll"
      next: ci
  gates:
    - name: ci
      evaluator: automated
      automated:
        check: ci-status
      branches:
        pass: ""
`
	want := `# workflow docs must stay byte-stable
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0" # keep this comment

metadata:
  name: unpinned
spec:
  gaggle: golden
  start: poll
  tasks:
    - name: poll
      type: deterministic
      goal: >-
        Keep this hand-wrapped text
        on multiple source lines.
      inputs:
        kind: "ci-poll"
      next: ci
  gates:
    - name: ci
      evaluator: automated
      automated:
        check: ci-status
        pollIntervalSeconds: 10
      branches:
        pass: ""
`

	result, err := Migrate([]byte(source), "2.0")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if result.After != want {
		t.Fatalf("transform migration rewrote unrelated source bytes\nwant:\n%s\ngot:\n%s", want, result.After)
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], `gate "ci"`) {
		t.Fatalf("Notes = %v, want one note naming gate \"ci\"", result.Notes)
	}
}

func TestMigrateTransformEditsFlowAutomatedMapping(t *testing.T) {
	tests := []struct {
		name      string
		automated string
		want      string
	}{
		{
			name:      "non-empty flow",
			automated: "{check: ci-status}",
			want:      "{pollIntervalSeconds: 10, check: ci-status}",
		},
		{
			name:      "empty flow",
			automated: "{}",
			want:      "{pollIntervalSeconds: 10}",
		},
		{
			name:      "existing poll interval",
			automated: "{check: ci-status, pollIntervalSeconds: 0}",
			want:      "{check: ci-status, pollIntervalSeconds: 10}",
		},
		{
			name:      "multi-line flow",
			automated: "{\n        check: ci-status\n      }",
			want:      "{pollIntervalSeconds: 10, \n        check: ci-status\n      }",
		},
		{
			name:      "commented flow",
			automated: "{ # keep comment\n        check: ci-status\n      }",
			want:      "{pollIntervalSeconds: 10,  # keep comment\n        check: ci-status\n      }",
		},
		{
			name:      "nested flow",
			automated: "{check: ci-status, params: {description: keep}}",
			want:      "{pollIntervalSeconds: 10, check: ci-status, params: {description: keep}}",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := workflowWithAutomatedMapping(test.automated)
			want := strings.Replace(source, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1)
			want = strings.Replace(want, "automated: "+test.automated, "automated: "+test.want, 1)

			result, err := Migrate([]byte(source), "2.0")
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if result.After != want {
				t.Fatalf("flow transform changed bytes beyond the version/poll edits\nwant:\n%s\ngot:\n%s", want, result.After)
			}
			if got := decodeWorkflow(t, result.After); got.DSLVersion != "2.0" || got.PollIntervalSeconds != 10 {
				t.Fatalf("decoded migrated workflow = %+v, want dslVersion 2.0 pollIntervalSeconds 10\n%s", got, result.After)
			}
		})
	}
}

func TestMigrateTransformPreservesMultilineAutomatedValues(t *testing.T) {
	tests := []struct {
		name      string
		automated string
	}{
		{
			name: "direct literal block scalar",
			automated: `        check: ci-status
        description: |
          first line
          second line
`,
		},
		{
			name: "direct folded block scalar",
			automated: `        check: ci-status
        description: >
          first line
          second line
`,
		},
		{
			name: "nested literal block scalar",
			automated: `        check: ci-status
        params:
          description: |
            first line
            second line
`,
		},
		{
			name: "nested folded block scalar",
			automated: `        check: ci-status
        params:
          description: >
            first line
            second line
`,
		},
		{
			name: "multiline double quoted scalar",
			automated: `        check: ci-status
        description: "first line
          second line"
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := workflowWithAutomatedBlock(test.automated)
			want := strings.Replace(source, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1)
			want = strings.Replace(want, "      automated:\n        ", "      automated:\n        pollIntervalSeconds: 10\n        ", 1)

			result, err := Migrate([]byte(source), "2.0")
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if result.After != want {
				t.Fatalf("multiline transform changed bytes beyond the version/poll edits\nwant:\n%s\ngot:\n%s", want, result.After)
			}
			got := decodeWorkflow(t, result.After)
			if got.DSLVersion != "2.0" || got.PollIntervalSeconds != 10 {
				t.Fatalf("decoded migrated workflow = %+v, want dslVersion 2.0 pollIntervalSeconds 10\n%s", got, result.After)
			}
		})
	}
}

func TestMigrateTransformExistingPollIntervalUnsetAndExplicitValues(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "block empty",
			source: workflowWithAutomatedBlock("        pollIntervalSeconds:\n        check: ci-status\n"),
			want:   workflowWithAutomatedBlock("        pollIntervalSeconds: 10\n        check: ci-status\n"),
		},
		{
			name:   "block empty before comment",
			source: workflowWithAutomatedBlock("        pollIntervalSeconds: # keep\n        check: ci-status\n"),
			want:   workflowWithAutomatedBlock("        pollIntervalSeconds: 10 # keep\n        check: ci-status\n"),
		},
		{
			name:   "block tilde null",
			source: workflowWithAutomatedBlock("        pollIntervalSeconds: ~\n        check: ci-status\n"),
			want:   workflowWithAutomatedBlock("        pollIntervalSeconds: 10\n        check: ci-status\n"),
		},
		{
			name:   "block null literal",
			source: workflowWithAutomatedBlock("        pollIntervalSeconds: null\n        check: ci-status\n"),
			want:   workflowWithAutomatedBlock("        pollIntervalSeconds: 10\n        check: ci-status\n"),
		},
		{
			name:   "flow empty before comma",
			source: workflowWithAutomatedMapping("{pollIntervalSeconds: , check: ci-status}"),
			want:   workflowWithAutomatedMapping("{pollIntervalSeconds: 10, check: ci-status}"),
		},
		{
			name:   "flow empty only",
			source: workflowWithAutomatedMapping("{pollIntervalSeconds:}"),
			want:   workflowWithAutomatedMapping("{pollIntervalSeconds: 10}"),
		},
		{
			name:   "flow null literal",
			source: workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: null}"),
			want:   workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: 10}"),
		},
		{
			name:   "flow tilde null",
			source: workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: ~}"),
			want:   workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: 10}"),
		},
		{
			name:   "block zero",
			source: workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: 0\n"),
			want:   workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: 10\n"),
		},
		{
			name:   "block tagged zero",
			source: workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: !!int 0\n"),
			want:   workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: !!int 10\n"),
		},
		{
			name:   "block anchored zero",
			source: workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: &zero 0\n"),
			want:   workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: &zero 10\n"),
		},
		{
			name:   "flow zero",
			source: workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: 0}"),
			want:   workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: 10}"),
		},
		{
			name:   "flow tagged zero",
			source: workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: !!int 0}"),
			want:   workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: !!int 10}"),
		},
		{
			name:   "flow anchored zero",
			source: workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: &zero 0}"),
			want:   workflowWithAutomatedMapping("{check: ci-status, pollIntervalSeconds: &zero 10}"),
		},
		{
			name:   "negative stays explicit",
			source: workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: -1\n"),
			want:   workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: -1\n"),
		},
		{
			name:   "non-numeric stays explicit",
			source: workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: soon\n"),
			want:   workflowWithAutomatedBlock("        check: ci-status\n        pollIntervalSeconds: soon\n"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := strings.Replace(test.want, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1)

			result, err := Migrate([]byte(test.source), "2.0")
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if result.After != want {
				t.Fatalf("migration changed bytes beyond expected edits\nwant:\n%s\ngot:\n%s", want, result.After)
			}
			gotSem, err := parseSemanticYAML([]byte(result.After))
			if err != nil {
				t.Fatalf("parse migrated YAML: %v\n%s", err, result.After)
			}
			wantSem, err := expectedSemanticV14ToV20([]byte(test.source), "2.0")
			if err != nil {
				t.Fatalf("node-transform semantics: %v", err)
			}
			if !reflect.DeepEqual(gotSem, wantSem) {
				t.Fatalf("source edit semantics differ from node transform\ngot:  %#v\nwant: %#v", gotSem, wantSem)
			}
		})
	}
}

func TestMigrateTransformDoesNotNormalizeUnrelatedPollIntervalText(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "quoted scalar",
			source: strings.Replace(workflowWithAutomatedBlock("        check: ci-status\n"),
				"      type: deterministic\n",
				"      type: deterministic\n      goal: \"literal pollIntervalSeconds:} text\"\n", 1),
		},
		{
			name: "block scalar",
			source: strings.Replace(workflowWithAutomatedBlock("        check: ci-status\n"),
				"      type: deterministic\n",
				"      type: deterministic\n      goal: |\n        literal pollIntervalSeconds:} text\n", 1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := strings.Replace(test.source, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1)
			want = strings.Replace(want, "        check: ci-status\n", "        check: ci-status\n        pollIntervalSeconds: 10\n", 1)

			result, err := Migrate([]byte(test.source), "2.0")
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if result.After != want {
				t.Fatalf("migration changed unrelated scalar content\nwant:\n%s\ngot:\n%s", want, result.After)
			}
		})
	}
}

func TestMigrateLeavesExplicitPositivePollIntervalUntouched(t *testing.T) {
	result, err := Migrate([]byte(workflowWithPinnedCIPoll), "2.0")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(result.Notes) != 0 {
		t.Fatalf("Notes = %v, want none (interval was already explicit)", result.Notes)
	}
	after := decodeWorkflow(t, result.After)
	if after.PollIntervalSeconds != 7 {
		t.Fatalf("after pollIntervalSeconds = %d, want unchanged 7", after.PollIntervalSeconds)
	}
}

func workflowWithAutomatedBlock(automated string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: block-automated
spec:
  gaggle: golden
  start: poll
  tasks:
    - name: poll
      type: deterministic
      inputs:
        kind: "ci-poll"
      next: ci
  gates:
    - name: ci
      evaluator: automated
      automated:
` + automated + `      branches:
        pass: ""
`
}

func TestMigrateBumpsVersionEvenWithoutCIPollTasks(t *testing.T) {
	result, err := Migrate([]byte(workflowWithNoCIPoll), "2.0")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !result.Changed {
		t.Fatal("Changed = false, want true for the dslVersion pin")
	}
	after := decodeWorkflow(t, result.After)
	if after.DSLVersion != "2.0" {
		t.Fatalf("after dslVersion = %q, want 2.0", after.DSLVersion)
	}
}

func workflowWithAutomatedMapping(automated string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: flow-automated
spec:
  gaggle: golden
  start: poll
  tasks:
    - name: poll
      type: deterministic
      inputs:
        kind: "ci-poll"
      next: ci
  gates:
    - name: ci
      evaluator: automated
      automated: ` + automated + `
      branches:
        pass: ""
`
}

func TestMigratePinOnlyPreservesOriginalBytes(t *testing.T) {
	source := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4" # keep this comment

metadata:
  name: wrapped
spec:
  gaggle: golden
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: >-
        Keep this hand-wrapped text
        on multiple source lines.
`
	want := strings.Replace(source, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1)

	result, err := Migrate([]byte(source), "2.0")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if result.Before != source {
		t.Fatalf("Before changed original bytes:\n%s", result.Before)
	}
	if result.After != want {
		t.Fatalf("pin-only migration changed bytes beyond dslVersion\nwant:\n%s\ngot:\n%s", want, result.After)
	}
	if !result.Changed {
		t.Fatal("Changed = false, want true")
	}
	if len(result.Notes) != 0 {
		t.Fatalf("Notes = %v, want none", result.Notes)
	}
}

func TestMigratePinOnlyPreservesFlowMappingDelimiters(t *testing.T) {
	for _, source := range []string{
		`{dslVersion: 1.4, kind: Workflow, metadata: {name: flow}, spec: {gaggle: golden}}`,
		`{kind: Workflow, metadata: {name: flow}, spec: {gaggle: golden}, dslVersion: 1.4}`,
	} {
		t.Run(source, func(t *testing.T) {
			want := strings.Replace(source, "dslVersion: 1.4", "dslVersion: 2.0", 1)

			result, err := Migrate([]byte(source), "2.0")
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if result.After != want {
				t.Fatalf("After = %q, want %q", result.After, want)
			}
			if got := decodeWorkflow(t, result.After).DSLVersion; got != "2.0" {
				t.Fatalf("after dslVersion = %q, want 2.0", got)
			}
		})
	}
}

func TestMigratePinOnlyRejectsBlockStyleVersion(t *testing.T) {
	for _, style := range []string{"|-", ">-"} {
		t.Run(style, func(t *testing.T) {
			source := strings.Replace(workflowWithNoCIPoll, `dslVersion: "1.4"`, "dslVersion: "+style+"\n  1.4", 1)

			_, err := Migrate([]byte(source), "2.0")
			if err == nil || !strings.Contains(err.Error(), "block-style dslVersion is not supported") {
				t.Fatalf("err = %v, want unsupported block-style error", err)
			}
		})
	}
}

func TestMigratePinOnlyRejectsDecoratedVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		wantErr string
	}{
		{name: "tagged", version: "!!str 1.4", wantErr: "tagged dslVersion is not supported"},
		{name: "anchored", version: "&version 1.4", wantErr: "anchored dslVersion is not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(workflowWithNoCIPoll, `dslVersion: "1.4"`, "dslVersion: "+test.version, 1)

			_, err := Migrate([]byte(source), "2.0")
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestMigrateRefusesAlreadyAtTarget(t *testing.T) {
	source := strings.Replace(workflowWithNoCIPoll, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1)
	_, err := Migrate([]byte(source), "2.0")
	if !errors.Is(err, ErrAlreadyAtTarget) {
		t.Fatalf("err = %v, want ErrAlreadyAtTarget", err)
	}
}

func TestMigrateRefusesNonAdjacentOrUnknownTarget(t *testing.T) {
	for _, to := range []string{"3.0", "1.0"} {
		t.Run(to, func(t *testing.T) {
			_, err := Migrate([]byte(workflowWithNoCIPoll), to)
			if err == nil || errors.Is(err, ErrAlreadyAtTarget) {
				t.Fatalf("err = %v, want a no-direct-edge error", err)
			}
			if !strings.Contains(err.Error(), "no direct migration registered") {
				t.Fatalf("err = %v, want a no-direct-edge diagnostic naming the missing hop", err)
			}
		})
	}
}

func TestMigrateDefaultsMissingDSLVersionToCurrent(t *testing.T) {
	source := strings.Replace(workflowWithNoCIPoll, "dslVersion: \"1.4\"\n", "", 1)
	result, err := Migrate([]byte(source), "2.0")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	after := decodeWorkflow(t, result.After)
	if after.DSLVersion != "2.0" {
		t.Fatalf("after dslVersion = %q, want 2.0", after.DSLVersion)
	}
	want := strings.Replace(source, "kind: Workflow\n", "kind: Workflow\ndslVersion: \"2.0\"\n", 1)
	if result.After != want {
		t.Fatalf("migration without a version pin changed unrelated bytes\nwant:\n%s\ngot:\n%s", want, result.After)
	}
}

func TestFindEdge(t *testing.T) {
	if _, ok := FindEdge("1.4", "2.0"); !ok {
		t.Fatal("FindEdge(1.4, 2.0) = not found, want the registered DVL-5 edge")
	}
	if _, ok := FindEdge("2.0", "3.0"); !ok {
		t.Fatal("FindEdge(2.0, 3.0) = not found, want the registered Goobernetes edge")
	}
	// The migrator remains one-step: no direct 1.4→3.0 edge exists.
	if _, ok := FindEdge("1.4", "3.0"); ok {
		t.Fatal("FindEdge(1.4, 3.0) = found, want no direct edge (chain 1.4→2.0→3.0)")
	}
}

type decodedWorkflow struct {
	DSLVersion          string
	PollIntervalSeconds int
}

func decodeWorkflow(t *testing.T, source string) decodedWorkflow {
	t.Helper()
	var raw struct {
		DSLVersion string `yaml:"dslVersion"`
		Spec       struct {
			Gates []struct {
				Automated struct {
					PollIntervalSeconds int `yaml:"pollIntervalSeconds"`
				} `yaml:"automated"`
			} `yaml:"gates"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(source), &raw); err != nil {
		t.Fatalf("decode migrated document: %v\n%s", err, source)
	}
	out := decodedWorkflow{DSLVersion: raw.DSLVersion}
	if len(raw.Spec.Gates) > 0 {
		out.PollIntervalSeconds = raw.Spec.Gates[0].Automated.PollIntervalSeconds
	}
	return out
}
