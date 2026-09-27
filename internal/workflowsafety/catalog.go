// Package workflowsafety provides bounded, advisory analysis of compiled
// workflows. It has no execution adapters and never changes the machine.
package workflowsafety

import (
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// CatalogVersion identifies the supported effects and diagnostic contract.
const CatalogVersion = "goobers.dev/workflow-safety/v1"

// Effects describe supported command forms, not successful live execution.
// Unknown commands must not be interpreted as evidence of absence.
type Effects struct {
	Known             bool
	Patch             bool
	EmptySuccess      bool
	Changes           bool
	CodeSubject       bool
	SelectsPR         bool
	Rebinds           bool
	ConditionalRebind bool
	NoWork            bool
	Publishes         string
	Parks             bool
}

// CommandEffects deliberately recognizes argv, never task names, shell text,
// prompts, or capability grants.
func CommandEffects(t apiv1.Task) Effects {
	if t.Type == apiv1.TaskAgentic {
		return Effects{Changes: t.EffectiveWorkspace() == "" || t.EffectiveWorkspace() == apiv1.WorkspaceRepo}
	}
	kind := strings.TrimSpace(t.Inputs["kind"])
	if t.Run == nil || t.Run.Script != "" || (kind != "" && kind != "shell") {
		return Effects{}
	}
	cmd := t.Run.Command
	if slices.Equal(cmd, []string{"true"}) {
		return Effects{Known: true, EmptySuccess: true}
	}
	if len(cmd) >= 2 && cmd[0] == "git" && cmd[1] == "diff" {
		return gitDiffEffects(cmd)
	}
	if len(cmd) < 2 || cmd[0] != "goobers" {
		return Effects{}
	}
	args := cmd[2:]
	switch cmd[1] {
	case "apply-verdict":
		return verdictEffects(args)
	case "pr-select", "gather-pr-context", "gather-sibling-context", "update-behind-pr", "backlog-query":
		return selectionEffects(cmd[1], args)
	case "push-branch", "push-remediated", "rebase-pr":
		if len(args) == 0 {
			return Effects{Known: true, Changes: true, CodeSubject: true}
		}
	case "pr-claim":
		if len(args) == 0 || slices.Equal(args, []string{"--release"}) {
			return Effects{Known: true}
		}
	case "remediation-checkpoint":
		if slices.Equal(args, []string{"--escalate"}) {
			return Effects{Known: true, Parks: true}
		}
	}
	return Effects{}
}

func gitDiffEffects(cmd []string) Effects {
	if slices.Equal(cmd, []string{"git", "diff", "--check"}) {
		return Effects{Known: true, EmptySuccess: true, CodeSubject: true}
	}
	if len(cmd) == 3 && strings.HasSuffix(cmd[2], "...HEAD") && !strings.HasPrefix(cmd[2], "-") {
		if cmd[2] == "HEAD...HEAD" || cmd[2] == "...HEAD" {
			return Effects{Known: true, EmptySuccess: true, CodeSubject: true}
		}
		return Effects{Known: true, Patch: true, CodeSubject: true}
	}
	return Effects{}
}

func verdictEffects(args []string) Effects {
	name := "review"
	if len(args) == 2 && (args[0] == "--gate" || args[0] == "-gate") && args[1] != "" {
		name = args[1]
	} else if len(args) == 1 && strings.HasPrefix(args[0], "--gate=") && len(args[0]) > len("--gate=") {
		name = strings.TrimPrefix(args[0], "--gate=")
	} else if len(args) != 0 {
		return Effects{}
	}
	return Effects{Known: true, Publishes: name}
}

func selectionEffects(command string, args []string) Effects {
	switch command {
	case "pr-select":
		if len(args) == 0 {
			return Effects{Known: true, SelectsPR: true, NoWork: true}
		}
	case "gather-pr-context", "gather-sibling-context":
		if len(args) == 0 || (command == "gather-sibling-context" && slices.Equal(args, []string{"--no-verdict-cache"})) {
			return Effects{Known: true, SelectsPR: true, Rebinds: command == "gather-pr-context",
				ConditionalRebind: command == "gather-sibling-context", NoWork: command == "gather-pr-context"}
		}
	case "update-behind-pr":
		if len(args) == 0 {
			return Effects{Known: true, SelectsPR: true, NoWork: true, Changes: true}
		}
	case "backlog-query":
		if len(args) == 0 || slices.Equal(args, []string{"--claim"}) {
			return Effects{Known: true, NoWork: true}
		}
	}
	return Effects{}
}
