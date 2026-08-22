package v30

// runson.go validates the DSL 3.0 scheduling surface (dsl-3.0.md §2, decision
// record D2): the stage-level runsOn block, the gaggle-level floor it merges
// with, the CAP004 os=* token ban, the CAP005 closed restriction vocabulary,
// and the refusal of surface that no longer exists in 3.0.
//
// Quantity handling (dsl-3.0.md open point 6, decided here): quantities are
// parsed with the vendored k8s.io/apimachinery resource parser — the exact
// grammar the pod spec will consume in mode 3 and the same parser the
// instance inventory already uses (internal/instance/runners.go), so the two
// sides of the constraint solve can never disagree about well-formedness.
// Diagnostics and digests keep the author's verbatim spelling: D4 pins
// "Kubernetes quantity strings verbatim", and the definition digest hashes
// the raw document, so no canonicalization (2000m vs 2) is ever applied.

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnercap"
)

// runsOnOSValues is the validated os enum (D2): the product spellings, not
// GOOS (`macOS`, not `darwin`). It mirrors the instance inventory's
// provides.os enum (internal/instance RunnerOS) by construction — both quote
// dsl-3.0.md §2 — and the shared schema pins both with enum markers.
var runsOnOSValues = []string{"linux", "windows", "macOS"}

func validRunsOnOS(os string) bool {
	for _, v := range runsOnOSValues {
		if os == v {
			return true
		}
	}
	return false
}

// removedSurfaceProblems refuses 2.0 surface that does not exist in DSL 3.0,
// each with a pointer to its 3.0 home so the error is actionable without
// reading the design doc:
//
//   - task/gaggle requiredCapabilities → runsOn.capabilities (D1/D12); the
//     gaggle-level check lives in runsOnProblems because it needs the gaggle.
//   - run.network: none → runsOn.restrictions: [network:none] (D16 — one
//     restrictions model, not two network surfaces).
//
// The gaggle sandbox override (featureGaggleSandbox) is likewise gone in 3.0;
// FeaturesForGaggle refuses it with the same pointer-diagnostic style.
func removedSurfaceProblems(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.RequiredCapabilities != nil {
			problems = append(problems, fmt.Sprintf(
				"task %q declares requiredCapabilities, which does not exist in DSL 3.0: move the tokens to runsOn.capabilities and any os=* token to runsOn.os (`goobers fix --to 3.0` performs this rewrite)",
				task.Name))
		}
		if task.Run != nil && task.Run.Network != "" {
			problems = append(problems, fmt.Sprintf(
				"task %q declares run.network, which does not exist in DSL 3.0: declare runsOn.restrictions: [%s] instead (one restrictions model, dsl-3.0.md D16)",
				task.Name, runnercap.RestrictionNetworkNone))
		}
	}
	return problems
}

// runsOnProblems reports structural problems in the declared runsOn blocks:
// an os value outside the validated enum, a malformed or non-positive
// quantity, a malformed capability token, and a gaggle-vs-stage OS conflict
// (the merge rule of dsl-3.0.md §2: capabilities and restrictions union; an
// OS conflict is a compile error, never a silent override).
func runsOnProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	gaggleOS := ""
	if gaggleRunsOn != nil {
		gaggleOS = gaggleRunsOn.OS
		if gaggleOS != "" && !validRunsOnOS(gaggleOS) {
			problems = append(problems, fmt.Sprintf(
				"gaggle runsOn.os %q is not one of %s", gaggleOS, strings.Join(runsOnOSValues, ", ")))
		}
		for i, token := range gaggleRunsOn.Capabilities {
			if err := runnercap.ValidateToken(token); err != nil {
				problems = append(problems, fmt.Sprintf("gaggle runsOn.capabilities[%d]: %v", i, err))
			}
		}
	}
	for _, task := range def.Spec.Tasks {
		runsOn := task.RunsOn
		if runsOn == nil {
			continue
		}
		if runsOn.OS != "" && !validRunsOnOS(runsOn.OS) {
			problems = append(problems, fmt.Sprintf(
				"task %q runsOn.os %q is not one of %s", task.Name, runsOn.OS, strings.Join(runsOnOSValues, ", ")))
		}
		if runsOn.OS != "" && gaggleOS != "" && runsOn.OS != gaggleOS && validRunsOnOS(runsOn.OS) && validRunsOnOS(gaggleOS) {
			problems = append(problems, fmt.Sprintf(
				"task %q runsOn.os %q conflicts with the gaggle-level runsOn.os %q; the gaggle floor merges into every stage and an OS conflict is unsatisfiable (dsl-3.0.md §2)",
				task.Name, runsOn.OS, gaggleOS))
		}
		for _, quantity := range []struct {
			field string
			value string
		}{
			{field: "cpu", value: runsOn.CPU},
			{field: "memory", value: runsOn.Memory},
			{field: "disk", value: runsOn.Disk},
		} {
			if quantity.value == "" {
				continue
			}
			parsed, err := resource.ParseQuantity(quantity.value)
			if err != nil {
				problems = append(problems, fmt.Sprintf(
					"task %q runsOn.%s %q must be a Kubernetes quantity string (for example \"2000m\", \"4Gi\"): %v",
					task.Name, quantity.field, quantity.value, err))
				continue
			}
			if parsed.Sign() <= 0 {
				problems = append(problems, fmt.Sprintf(
					"task %q runsOn.%s must be positive, got %q", task.Name, quantity.field, quantity.value))
			}
		}
		for i, token := range runsOn.Capabilities {
			if err := runnercap.ValidateToken(token); err != nil {
				problems = append(problems, fmt.Sprintf("task %q runsOn.capabilities[%d]: %v", task.Name, i, err))
			}
		}
	}
	return problems
}

// osTokenProblems is CAP004 (dsl-3.0.md D12): an os=* token anywhere in a 3.0
// document is refused — runsOn.os is the only platform vocabulary, so the
// #659 two-vocabularies drift hazard structurally cannot recur. It scans
// every capability tag position: stage runsOn.capabilities and the
// gaggle-level floor. (requiredCapabilities cannot carry one because the
// whole field is already refused by removedSurfaceProblems; that refusal
// names the os=* rewrite too.)
func osTokenProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	if gaggleRunsOn != nil {
		for _, token := range gaggleRunsOn.Capabilities {
			if goos, ok := strings.CutPrefix(token, "os="); ok {
				problems = append(problems, fmt.Sprintf(
					"gaggle runsOn.capabilities contains %q: os=* tokens do not exist in DSL 3.0 — declare runsOn.os: %s instead", token, canonicalOSName(goos)))
			}
		}
	}
	for _, task := range def.Spec.Tasks {
		if task.RunsOn == nil {
			continue
		}
		for _, token := range task.RunsOn.Capabilities {
			if goos, ok := strings.CutPrefix(token, "os="); ok {
				problems = append(problems, fmt.Sprintf(
					"task %q runsOn.capabilities contains %q: os=* tokens do not exist in DSL 3.0 — declare runsOn.os: %s instead", task.Name, token, canonicalOSName(goos)))
			}
		}
	}
	return problems
}

// canonicalOSName maps a GOOS spelling from a legacy os=* token to the 3.0
// enum spelling for the CAP004 rewrite hint (darwin → macOS, D12).
func canonicalOSName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux", "windows":
		return goos
	default:
		return fmt.Sprintf("<one of %s>", strings.Join(runsOnOSValues, "|"))
	}
}

// restrictionProblems is CAP005 (dsl-3.0.md §5): every restriction token must
// come from the closed v1 effect list (internal/runnercap, the vocabulary the
// instance inventory shares); an unknown token errors with a did-you-mean
// suggestion (the CAP002 idiom).
func restrictionProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	if gaggleRunsOn != nil {
		for _, token := range gaggleRunsOn.Restrictions {
			if !runnercap.KnownRestriction(token) {
				problems = append(problems, fmt.Sprintf("gaggle runsOn.restrictions: %s", unknownRestriction(token)))
			}
		}
	}
	for _, task := range def.Spec.Tasks {
		if task.RunsOn == nil {
			continue
		}
		for _, token := range task.RunsOn.Restrictions {
			if !runnercap.KnownRestriction(token) {
				problems = append(problems, fmt.Sprintf("task %q runsOn.restrictions: %s", task.Name, unknownRestriction(token)))
			}
		}
	}
	return problems
}

func unknownRestriction(token string) string {
	names := make([]string, 0, len(runnercap.KnownRestrictions()))
	for _, r := range runnercap.KnownRestrictions() {
		names = append(names, string(r))
	}
	message := fmt.Sprintf("unknown restriction %q (the closed v1 effect list: %s)", token, strings.Join(names, ", "))
	if suggestion, ok := runnercap.SuggestRestriction(token); ok {
		message += fmt.Sprintf(" (did you mean %q?)", suggestion)
	}
	return message
}

// EffectiveRunsOn resolves a stage's effective placement requirement: the
// gaggle-level floor merged with the stage's own declaration (capabilities
// and restrictions union, OS from whichever side declares one — a conflict is
// already a compile error via runsOnProblems). Quantities come from the stage
// alone; the gaggle floor has none by design.
func EffectiveRunsOn(task apiv1.Task, gaggleRunsOn *apiv1.GaggleRunsOn) apiv1.RunsOn {
	var effective apiv1.RunsOn
	if task.RunsOn != nil {
		effective = *task.RunsOn
		effective.Capabilities = append([]string(nil), task.RunsOn.Capabilities...)
		effective.Restrictions = append([]string(nil), task.RunsOn.Restrictions...)
	}
	if gaggleRunsOn != nil {
		if effective.OS == "" {
			effective.OS = gaggleRunsOn.OS
		}
		effective.Capabilities = append(effective.Capabilities, gaggleRunsOn.Capabilities...)
		effective.Restrictions = append(effective.Restrictions, gaggleRunsOn.Restrictions...)
	}
	effective.Capabilities = sortedDistinct(effective.Capabilities)
	effective.Restrictions = sortedDistinct(effective.Restrictions)
	return effective
}
