package vcurrent

import (
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Required-input analysis. This closes the exact gap that caused the #1061
// election outage, which no existing check could see.
//
// The motivating live failure: merge-review's apply-verdict node lost its
// `selectedHeadSha`/`selectedBaseSha` inputsFrom wiring in a hand-maintained
// instance config, while the `goobers apply-verdict` binary — advanced by
// #1039 to hard-require those SHAs — kept demanding them. Every apply-verdict
// invocation exited 1 with "selectedHeadSha is required", so no merge-review
// election completed for the life of that build. The workflow was
// structurally valid, compiled clean, and passed `goobers validate`: nothing
// knew that `goobers apply-verdict` needs an input the node never wired,
// because a deterministic stage's required inputs live in Go code
// (providerInput + an emptiness check), invisible to the DSL.
//
// CheckStageContracts already guards the mirror image — a stage that READS an
// upstream output no predecessor emits — but an ABSENT inputsFrom/inputs entry
// is not a reference, so there is nothing there for it to flag. This check
// supplies the missing half: for a stage whose command is a known `goobers`
// subcommand with hard-required inputs, every such input must be wired by the
// node itself (a static `inputs` value or an `inputsFrom` edge). It is the
// input-side analog of expectedOutputs.

// stageRequiredInputs maps a `goobers` provider subcommand to the input keys it
// hard-requires from its workflow node — the ones it reads via providerInput
// and refuses to run without. ONLY workflow-supplied inputs belong here: a
// value the runner always injects itself (GOOBERS_RUN_ID and friends) is never
// listed, because a workflow cannot and should not wire it.
//
// This registry mirrors the emptiness checks in cmd/goobers/<subcommand>.go.
// It is deliberately conservative: an unlisted subcommand is treated as
// "requirements unknown" and never flagged, so a stale entry can only ever
// MISS a real problem, never invent a false one. A test locks every shipped
// and example workflow to this registry, so a shipped stage that stops
// satisfying it fails CI.
var stageRequiredInputs = map[string][]string{
	// cmd/goobers/applyverdict.go — selectedNumber/selectedHeadSha/selectedBaseSha
	// (the #1039 SHA pin). This is the exact set whose omission caused #1061.
	"apply-verdict": {"selectedNumber", "selectedHeadSha", "selectedBaseSha"},
	// cmd/goobers/electlander.go — selectedNumber only; the SHAs are optional
	// pass-throughs there (empty is tolerated), so they are NOT required.
	"elect-lander": {"selectedNumber"},
	// cmd/goobers/mergepr.go — the merge conjuncts and the D6 SHA pin. `verdict`
	// is normally a static `inputs` value ("pass"); the rest arrive via inputsFrom.
	"merge-pr": {"pullNumber", "verdict", "headSha", "baseSha"},
	// cmd/goobers/postmerge.go
	"post-merge": {"pullNumber"},
	// cmd/goobers/mergequeuepoll.go (the queue-watch stage)
	"merge-queue-poll": {"pullNumber"},
}

// CheckStageRequiredInputs reports deterministic stages that invoke a known
// `goobers` subcommand without wiring an input that subcommand hard-requires.
// A required input is satisfied when the node declares it as a static `inputs`
// key or as an `inputsFrom` input key; the predecessor-emits-it half is left
// to CheckStageContracts' unsatisfiableInputsFromProblems, which reports an
// inputsFrom edge whose source no predecessor produces.
//
// Like CheckStageContracts it is a no-op on a structurally broken graph — those
// problems are reported field-by-field elsewhere and walking a broken graph
// only cascades noise.
func CheckStageRequiredInputs(def Definition) []string {
	m, buildProblems := newMachineForCheck(def)
	if len(buildProblems) > 0 {
		return buildProblems
	}
	if len(structuralProblems(m)) > 0 {
		return nil
	}
	var problems []string
	for _, task := range def.Spec.Tasks {
		sub, ok := goobersSubcommand(task)
		if !ok {
			continue
		}
		required, known := stageRequiredInputs[sub]
		if !known {
			continue
		}
		var missing []string
		for _, key := range required {
			if _, ok := task.Inputs[key]; ok {
				continue
			}
			if _, ok := task.InputsFrom[key]; ok {
				continue
			}
			missing = append(missing, key)
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		problems = append(problems, fmt.Sprintf(
			"task %q runs `goobers %s`, which requires input%s %v, but the stage wires %s through neither inputs nor inputsFrom; the command reads them via providerInput and exits non-zero when empty, so this stage fails every run",
			task.Name, sub, plural(len(missing), "", "s"), missing, plural(len(missing), "it", "them"),
		))
	}
	return problems
}

// goobersSubcommand returns the provider subcommand a deterministic shell stage
// invokes (the argv after "goobers"), if it is one. A non-shell stage, a stage
// whose command is not the goobers CLI, or a bare "goobers" with no subcommand
// yields ok=false.
func goobersSubcommand(task apiv1.Task) (string, bool) {
	if !isShellStage(task) || task.Run == nil {
		return "", false
	}
	cmd := task.Run.Command
	if len(cmd) < 2 || cmd[0] != "goobers" {
		return "", false
	}
	return cmd[1], true
}
