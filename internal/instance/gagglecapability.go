package instance

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnercap"
)

// LocalCIStageName is the well-known name of the deterministic stage that runs a
// gaggle's local CI-equivalent (build + lint + tests) — the stage that "owns
// `make ci`" (internal/runner, cmd/goobers/openprbody.go both key on this exact
// name). A gaggle's GaggleSpec.CICommand overrides the command this stage
// declares (MGV-1/#1009).
const LocalCIStageName = "local-ci"

// ApplyGaggleCICommand rewrites the command of every workflow's deterministic
// local-ci stage to its gaggle's declared GaggleSpec.CICommand, when the gaggle
// declares one (MGV-1/#1009). It resolves the effective config in place before
// the workflows are compiled, so the compiled machine — and therefore the
// runner — executes the gaggle's own CI suite in place of the stage's declared
// default (`["make","ci"]`). A gaggle that declares no CICommand, or a workflow
// with no local-ci stage, is left untouched, so a single Go gaggle behaves
// exactly as before.
func ApplyGaggleCICommand(set *ConfigSet) {
	if set == nil {
		return
	}
	commands := make(map[string][]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if len(g.Spec.CICommand) > 0 {
			commands[g.Name] = g.Spec.CICommand
		}
	}
	if len(commands) == 0 {
		return
	}
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		command, ok := commands[wf.Spec.Gaggle]
		if !ok {
			continue
		}
		for j := range wf.Spec.Tasks {
			t := &wf.Spec.Tasks[j]
			if t.Type != apiv1.TaskDeterministic || t.Name != LocalCIStageName || t.Run == nil {
				continue
			}
			// Copy so a later mutation of one workflow's slice can never alias
			// the gaggle's declared command (or another workflow's).
			t.Run.Command = append([]string(nil), command...)
		}
	}
}

// WorkflowRequiredCapabilities returns the runner capabilities a single run of
// wf needs: its gaggle's own GaggleSpec.RequiredCapabilities plus every stage's
// Task.RequiredCapabilities in wf. Because a run executes all of a workflow's
// stages on one runner, this whole set must be claimed by that runner at
// schedule time. The result is sorted and de-duplicated; a workflow whose
// gaggle name does not match is still measured against gaggle's requirements
// (callers pair a workflow with its own gaggle).
func WorkflowRequiredCapabilities(gaggle apiv1.Gaggle, wf apiv1.Workflow) []string {
	seen := make(map[string]struct{})
	add := func(caps []string) {
		for _, c := range caps {
			seen[c] = struct{}{}
		}
	}
	add(gaggle.Spec.RequiredCapabilities)
	for j := range wf.Spec.Tasks {
		add(wf.Spec.Tasks[j].RequiredCapabilities)
	}
	return sortedKeys(seen)
}

// RequiredCapabilities returns the union of every runner capability the given
// gaggle and its workflows require — the gaggle's own GaggleSpec.RequiredCapabilities
// plus every stage's Task.RequiredCapabilities across the workflows bound to
// that gaggle. The result is sorted and de-duplicated. This whole-gaggle union
// is what the static config-load cross-check validates as satisfiable; the
// per-run schedule check uses the narrower WorkflowRequiredCapabilities so one
// workflow is never refused for a sibling workflow's requirement.
func RequiredCapabilities(gaggle apiv1.Gaggle, workflows []apiv1.Workflow) []string {
	seen := make(map[string]struct{})
	add := func(caps []string) {
		for _, c := range caps {
			seen[c] = struct{}{}
		}
	}
	add(gaggle.Spec.RequiredCapabilities)
	for i := range workflows {
		wf := &workflows[i]
		if wf.Spec.Gaggle != gaggle.Name {
			continue
		}
		for j := range wf.Spec.Tasks {
			add(wf.Spec.Tasks[j].RequiredCapabilities)
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CheckCapabilityRequirements fails closed at config-load when a gaggle or stage
// requires a runner capability the runner does not claim (RRQ-1/#1101). It is
// the static, whole-instance counterpart of the scheduler's per-run admit
// check: it needs both the instance.yaml runner claims and the config-as-code
// requirements together, so it runs at daemon startup once both are loaded,
// rejecting an unsatisfiable config before any run is scheduled rather than
// leaving it to fail every schedule tick at runtime. The diagnostic names each
// unclaimed ("unknown") capability and the gaggle that requires it.
func CheckCapabilityRequirements(runnerCaps []string, set *ConfigSet) error {
	if set == nil {
		return nil
	}
	claimed := runnercap.NewClaimed(runnerCaps)
	for i := range set.Gaggles {
		gaggle := set.Gaggles[i]
		required := RequiredCapabilities(gaggle, set.Workflows)
		missing := claimed.Missing(required)
		if len(missing) > 0 {
			return fmt.Errorf("gaggle %q requires runner capability %s which no runner claims (runner.capabilities in instance.yaml)",
				gaggle.Name, strings.Join(missing, ", "))
		}
	}
	return nil
}
