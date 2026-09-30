// Package workflowsafety provides bounded, advisory analysis of compiled
// workflows. It has no execution adapters and never changes the machine.
package workflowsafety

import (
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/builtincmd"
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

type flagKind uint8

const (
	boolFlag flagKind = iota
	valueFlag
	optionalValueFlag
)

var builtinFlagShapes = map[string]map[string]flagKind{
	"apply-verdict":          {"gate": valueFlag},
	"backlog-health":         {"feedback": boolFlag},
	"backlog-query":          {"debug": boolFlag, "read-only": boolFlag, "claim": boolFlag, "resweep": boolFlag, "reconcile": boolFlag, "release": boolFlag},
	"docs-churn":             {"since": valueFlag, "buffer-multiplier": valueFlag},
	"gather-sibling-context": {"no-verdict-cache": boolFlag},
	"remediation-checkpoint": {"budget": valueFlag, "escalate": optionalValueFlag, "escalation-outcome": valueFlag},
	"respond-to-findings":    {"check": boolFlag},
	"security-alerts-query":  {"source": valueFlag, "ref": valueFlag, "severity": valueFlag, "max-results": valueFlag},
	"self" + "-update":       {"include-prerelease": boolFlag},
	"telemetry-query":        {"window": valueFlag, "workflow": valueFlag, "aggregate": valueFlag, "threshold": valueFlag, "format": valueFlag, "learning-action": valueFlag},
	"validate":               {"source-tree": valueFlag},
}

// CommandEffects deliberately recognizes argv, never task names, shell text,
// prompts, or capability grants.
func CommandEffects(t apiv1.Task) Effects {
	if t.Type == apiv1.TaskAgentic {
		return Effects{Changes: t.EffectiveWorkspace() == "" || t.EffectiveWorkspace() == apiv1.WorkspaceRepo}
	}
	kind := strings.TrimSpace(t.Inputs["kind"])
	if kind == "ci-poll" || kind == "external-telemetry" {
		return Effects{Known: true}
	}
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
	return goobersCommandEffects(cmd[1], cmd[2:])
}

func goobersCommandEffects(command string, args []string) Effects {
	var effects Effects
	switch command {
	case "apply-verdict":
		effects = verdictEffects(args)
	case "pr-select", "gather-pr-context", "gather-sibling-context", "update-behind-pr", "backlog-query":
		effects = selectionEffects(command, args)
	case "push-branch", "push-remediated", "rebase-pr":
		if len(args) == 0 {
			effects = Effects{Known: true, Changes: true, CodeSubject: true}
		}
	case "pr-claim":
		if len(args) == 0 || slices.Equal(args, []string{"--release"}) {
			effects = Effects{Known: true}
		}
	case "remediation-checkpoint":
		effects = remediationCheckpointEffects(args)
	}
	if effects.Known {
		return effects
	}
	if validBuiltInInvocation(command, args) {
		return Effects{Known: true}
	}
	return Effects{}
}

func remediationCheckpointEffects(args []string) Effects {
	if !validBuiltInInvocation("remediation-checkpoint", args) {
		return Effects{}
	}
	for _, arg := range args {
		switch {
		case arg == "--escalate":
			return Effects{Known: true, Parks: true}
		case strings.HasPrefix(arg, "--escalate=") && len(arg) > len("--escalate="):
			return Effects{Known: true, Parks: true}
		}
	}
	return Effects{Known: len(args) == 0 || knownFlagArgs(args, "budget", "escalation-outcome")}
}

func validBuiltInInvocation(command string, args []string) bool {
	if !builtincmd.Known(command) {
		return false
	}
	if len(args) == 0 {
		return true
	}
	shape, ok := builtinFlagShapes[command]
	if !ok {
		return false
	}
	return flagsMatchShape(args, shape)
}

func flagsMatchShape(args []string, shape map[string]flagKind) bool {
	for i := 0; i < len(args); i++ {
		name, value, hasInlineValue, ok := splitLongFlag(args[i])
		if !ok {
			return false
		}
		kind, ok := shape[name]
		if !ok {
			return false
		}
		switch kind {
		case boolFlag:
			if hasInlineValue && !validBoolFlagValue(value) {
				return false
			}
		case valueFlag:
			if hasInlineValue {
				if value == "" {
					return false
				}
				continue
			}
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return false
			}
		case optionalValueFlag:
			if hasInlineValue {
				if value == "" {
					return false
				}
				continue
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
		}
	}
	return true
}

func splitLongFlag(arg string) (string, string, bool, bool) {
	if !strings.HasPrefix(arg, "--") || arg == "--" {
		return "", "", false, false
	}
	name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
	return name, value, hasValue, name != ""
}

func validBoolFlagValue(value string) bool {
	return slices.Contains([]string{"1", "t", "T", "TRUE", "true", "True", "0", "f", "F", "FALSE", "false", "False"}, value)
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
		if !validBuiltInInvocation(command, args) {
			return Effects{}
		}
		if len(args) == 0 || flagEnabled(args, "claim") {
			return Effects{Known: true, NoWork: true}
		}
		if knownFlagArgs(args, "read-only", "reconcile", "release", "resweep") {
			return Effects{Known: true}
		}
	}
	return Effects{}
}

func flagEnabled(args []string, name string) bool {
	for _, arg := range args {
		if arg == "--"+name || arg == "--"+name+"=true" {
			return true
		}
	}
	return false
}

func knownFlagArgs(args []string, names ...string) bool {
	for _, arg := range args {
		if knownFlagArg(arg, names) {
			continue
		}
		return false
	}
	return len(args) > 0
}

func knownFlagArg(arg string, names []string) bool {
	for _, name := range names {
		if arg == "--"+name || strings.HasPrefix(arg, "--"+name+"=") {
			return true
		}
	}
	return false
}
