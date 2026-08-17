package dslmigrate

import (
	"errors"
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
	if _, ok := FindEdge("2.0", "3.0"); ok {
		t.Fatal("FindEdge(2.0, 3.0) = found, want no registered edge")
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
