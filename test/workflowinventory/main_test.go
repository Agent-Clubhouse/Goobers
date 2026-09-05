package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dormantWorkflow = `name: Dormant
on:
  workflow_dispatch:
  # Enable once #1478 lands.
  # schedule:
  #   - cron: "43 7 * * 1"
jobs: {}
`

const activeWorkflow = `name: Active
on:
  schedule:
    - cron: "17 8 * * *"
  workflow_dispatch:
jobs: {}
`

func TestClassifyMarksDispatchOnlyWorkflowWithCommentedScheduleDormant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		source   string
		triggers []string
		want     string
	}{
		{"commented schedule", dormantWorkflow, []string{"workflow_dispatch"}, dormantStatus},
		{"live schedule", activeWorkflow, []string{"schedule", "workflow_dispatch"}, activeStatus},
		{
			// A dispatch-only workflow with no commented schedule is a manual
			// tool, not a gate waiting to be switched on.
			"dispatch only",
			"name: Manual\non:\n  workflow_dispatch:\njobs: {}\n",
			[]string{"workflow_dispatch"},
			activeStatus,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			triggers, err := enabledTriggers([]byte(tc.source))
			if err != nil {
				t.Fatalf("enabledTriggers: %v", err)
			}
			if strings.Join(triggers, ",") != strings.Join(tc.triggers, ",") {
				t.Fatalf("triggers = %q, want %q", triggers, tc.triggers)
			}
			if got := classify(triggers, commentedSchedule.MatchString(tc.source)); got != tc.want {
				t.Fatalf("classify = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnabledTriggersReadsSequenceAndScalarForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source string
		want   string
	}{
		{"on: [push, workflow_dispatch]\njobs: {}\n", "push,workflow_dispatch"},
		{"on: push\njobs: {}\n", "push"},
	} {
		triggers, err := enabledTriggers([]byte(tc.source))
		if err != nil {
			t.Fatalf("enabledTriggers(%q): %v", tc.source, err)
		}
		if got := strings.Join(triggers, ","); got != tc.want {
			t.Errorf("triggers = %q, want %q", got, tc.want)
		}
	}
}

func TestRunAcceptsAnInventoryThatMatchesTheWorkflows(t *testing.T) {
	t.Parallel()
	dir := writeWorkflows(t)
	inventory := writeInventory(t, dir, "| `active.yml` | schedule, workflow_dispatch | active | — |\n"+
		"| `dormant.yml` | workflow_dispatch | dormant | #1478 |\n")

	if err := run(filepath.Join(dir, "workflows"), inventory, false); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRunRejectsUnrecordedAndUnattributedDormantWorkflows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		table string
		want  string
	}{
		{
			"missing row",
			"| `active.yml` | schedule, workflow_dispatch | active | — |\n",
			"dormant.yml: dormant workflow is missing from the inventory",
		},
		{
			"no blocking issue",
			"| `active.yml` | schedule, workflow_dispatch | active | — |\n" +
				"| `dormant.yml` | workflow_dispatch | dormant | — |\n",
			"dormant.yml: dormant workflow names no blocking issue",
		},
		{
			"stale status",
			"| `active.yml` | schedule, workflow_dispatch | active | — |\n" +
				"| `dormant.yml` | workflow_dispatch | active | — |\n",
			`dormant.yml: inventory says "active", workflow is dormant`,
		},
		{
			"stale triggers",
			"| `active.yml` | push | active | — |\n" +
				"| `dormant.yml` | workflow_dispatch | dormant | #1478 |\n",
			`active.yml: inventory lists triggers "push", workflow has "schedule, workflow_dispatch"`,
		},
		{
			"deleted workflow",
			"| `active.yml` | schedule, workflow_dispatch | active | — |\n" +
				"| `dormant.yml` | workflow_dispatch | dormant | #1478 |\n" +
				"| `gone.yml` | push | active | — |\n",
			"gone.yml: inventory lists a workflow that does not exist",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeWorkflows(t)
			inventory := writeInventory(t, dir, tc.table)

			err := run(filepath.Join(dir, "workflows"), inventory, false)
			if err == nil {
				t.Fatal("run accepted a stale inventory")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRunWritesTheTableAndKeepsRecordedBlockers(t *testing.T) {
	t.Parallel()
	dir := writeWorkflows(t)
	inventory := writeInventory(t, dir, "| `dormant.yml` | workflow_dispatch | dormant | #1478 |\n")

	if err := run(filepath.Join(dir, "workflows"), inventory, true); err != nil {
		t.Fatalf("run -write: %v", err)
	}
	rendered, err := os.ReadFile(inventory)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| `active.yml` | schedule, workflow_dispatch | active | — |",
		"| `dormant.yml` | workflow_dispatch | dormant | #1478 |",
	} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("rendered inventory missing %q:\n%s", want, rendered)
		}
	}
	if err := run(filepath.Join(dir, "workflows"), inventory, false); err != nil {
		t.Fatalf("regenerated inventory does not validate: %v", err)
	}
}

// TestRunFlagsANewDormantWorkflowAfterRegeneration proves the blocker column
// cannot be left unfilled: regenerating marks it TODO, and the check still
// fails until a human names what the workflow waits on.
func TestRunFlagsANewDormantWorkflowAfterRegeneration(t *testing.T) {
	t.Parallel()
	dir := writeWorkflows(t)
	inventory := writeInventory(t, dir, "| `active.yml` | schedule, workflow_dispatch | active | — |\n")

	if err := run(filepath.Join(dir, "workflows"), inventory, true); err != nil {
		t.Fatalf("run -write: %v", err)
	}
	err := run(filepath.Join(dir, "workflows"), inventory, false)
	if err == nil || !strings.Contains(err.Error(), "names no blocking issue") {
		t.Fatalf("error = %v, want a missing-blocker failure", err)
	}
}

func writeWorkflows(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	workflows := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(workflows, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"active.yml":  activeWorkflow,
		"dormant.yml": dormantWorkflow,
		"notes.md":    "not a workflow",
	} {
		if err := os.WriteFile(filepath.Join(workflows, name), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeInventory(t *testing.T, dir, table string) string {
	t.Helper()
	path := filepath.Join(dir, "workflow-inventory.md")
	document := "# Inventory\n\n" + beginMarker + "\n\n" +
		"| Workflow | Enabled triggers | Status | Blocked on |\n| --- | --- | --- | --- |\n" +
		table + "\n" + endMarker + "\n"
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
