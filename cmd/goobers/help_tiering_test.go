package main

import (
	"strings"
	"testing"
)

func TestHelpTiers(t *testing.T) {
	_, core, _ := runArgs(t, "help")
	_, all, _ := runArgs(t, "help", "all")
	_, stages, _ := runArgs(t, "help", "stages")

	for _, command := range []string{"init", "validate", "up", "dashboard", "run", "status", "trace"} {
		if !strings.Contains(core, "\n  "+command) {
			t.Errorf("core help missing %q", command)
		}
	}
	if strings.Contains(core, "\n  open-pr") {
		t.Error("core help exposes workflow-stage command open-pr")
	}
	if strings.Contains(core, "\n  doctor") {
		t.Error("core help exposes advanced command doctor")
	}
	if !strings.Contains(all, "\n  doctor") || !strings.Contains(all, "\n  open-pr") {
		t.Error("complete help does not expose advanced and workflow-stage commands")
	}
	if !strings.Contains(stages, "\n  open-pr") {
		t.Error("stage help missing open-pr")
	}
	if strings.Contains(stages, "\n  init") {
		t.Error("stage help exposes core command init")
	}
}

func TestRegistryTierAnnotations(t *testing.T) {
	var coreCount int
	for _, command := range cliCommands {
		switch command.tier {
		case cliTierCore:
			coreCount++
		case cliTierStage:
			if !strings.Contains(command.short, "(a workflow stage)") &&
				!strings.Contains(command.short, "(a connector stage)") {
				t.Errorf("stage command %q lacks a stage description marker", command.names[0])
			}
		}
		if strings.Contains(command.short, "(a workflow stage)") ||
			strings.Contains(command.short, "(a connector stage)") {
			if command.tier != cliTierStage {
				t.Errorf("stage-marked command %q has tier %d", command.names[0], command.tier)
			}
		}
	}
	if coreCount < 10 || coreCount > 20 {
		t.Fatalf("core top-level command count = %d, want progressive-disclosure range 10..20", coreCount)
	}
}

func TestCompletionDiscoveryUsesCoreTier(t *testing.T) {
	names := strings.Join(commandNames(buildCompletionModel()), " ")
	for _, command := range []string{"help", "init", "validate", "run", "status"} {
		if !strings.Contains(" "+names+" ", " "+command+" ") {
			t.Errorf("completion discovery missing core command %q", command)
		}
	}
	for _, command := range []string{"doctor", "open-pr"} {
		if strings.Contains(" "+names+" ", " "+command+" ") {
			t.Errorf("completion discovery exposes non-core command %q", command)
		}
	}
}

func TestCompletionDiscoveryExposesHelpTiers(t *testing.T) {
	model := buildCompletionModel()
	for _, command := range model.commands {
		if command.id == "help" {
			want := "all stages instance gaggle goober workflow stage gate harness capability"
			if got := strings.Join(command.argValues, " "); got != want {
				t.Fatalf("help completion candidates = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatal("completion model is missing help")
}
