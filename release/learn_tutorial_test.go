package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLearnGoobersCurriculumContract(t *testing.T) {
	entry := readTutorialFile(t, "learn-goobers.md")
	authoring := readTutorialFile(t, "learn-workflow-authoring.md")
	operations := readTutorialFile(t, "learn-goobers-operations.md")

	assertSubstringsInOrder(
		t,
		"Learn Goobers progression",
		entry,
		"## The mental model",
		"## Chapter 1: first success without credentials",
		"## Guided repository setup",
		"## Continue learning",
	)

	for _, want := range []string{
		"goobers init --demo ./demo-instance",
		"goobers run demo ./demo-instance",
		"goobers init --guided",
		"goobers validate --check-harness --check-repos <instance-path>",
		"learn-workflow-authoring.md",
		"learn-goobers-operations.md",
		"state-machine graph",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("learn-goobers.md missing %q", want)
		}
	}

	for _, stale := range []string{
		"goobers getting-started",
		"goobers init --guided ./",
		"goobers init --guided <",
	} {
		if strings.Contains(entry, stale) {
			t.Errorf("learn-goobers.md contains obsolete guided setup form %q", stale)
		}
	}

	for _, want := range []string{
		"validated state machine",
		"goobers workflow show --dot",
		"Compile-time versus run-time decisions",
		"Test at the smallest useful layer",
	} {
		if !strings.Contains(authoring, want) {
			t.Errorf("learn-workflow-authoring.md missing %q", want)
		}
	}

	for _, want := range []string{
		"Tutorial defaults versus production defaults",
		"goobers up <instance-path>",
		"Manage credentials",
		"Bound concurrency and autonomous work",
		"Back up or move an Instance",
		"Upgrade Goobers",
		"Extend the Instance",
	} {
		if !strings.Contains(operations, want) {
			t.Errorf("learn-goobers-operations.md missing %q", want)
		}
	}
}

func readTutorialFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "docs", "guides", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
