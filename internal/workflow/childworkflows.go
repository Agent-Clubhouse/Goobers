package workflow

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// ErrChildWorkflowExecutionUnsupported distinguishes admitted preview policy
// from executable child support. Both runtimes fail closed until that lands.
var ErrChildWorkflowExecutionUnsupported = errors.New("task.childWorkflows execution is not implemented; remove childWorkflows to run this workflow")

// RefuseChildWorkflowExecution rejects policy that a runtime cannot yet honor.
// Call before any execution effects, including when resuming a pinned machine.
func RefuseChildWorkflowExecution(spec apiv1.WorkflowSpec) error {
	for _, task := range spec.Tasks {
		if task.ChildWorkflows != nil {
			return fmt.Errorf("task %q: %w", task.Name, ErrChildWorkflowExecutionUnsupported)
		}
	}
	return nil
}

func childWorkflowProblems(def Definition, goobers map[string]apiv1.GooberSpec, checkNames bool) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		policy := task.ChildWorkflows
		if policy == nil {
			continue
		}
		add := func(format string, args ...any) {
			problems = append(problems, fmt.Sprintf("task %q childWorkflows: %s", task.Name, fmt.Sprintf(format, args...)))
		}
		if def.DSLVersion != supportmatrix.V31DSLVersion {
			add("requires dslVersion %q (workflow pins %q)", supportmatrix.V31DSLVersion, def.DSLVersion)
		}
		if task.Type != apiv1.TaskAgentic {
			add("requires type=agentic")
		}
		if len(policy.AllowedGoobers) == 0 || len(policy.AllowedGoobers) > 128 {
			add("allowedGoobers must contain 1 to 128 existing Goober names")
		}
		seenNames := map[string]bool{}
		for _, name := range policy.AllowedGoobers {
			if name == "" || strings.TrimSpace(name) != name || len(name) > 128 {
				add("allowedGoobers contains invalid name %q", name)
			}
			if seenNames[name] {
				add("allowedGoobers contains duplicate %q", name)
			}
			seenNames[name] = true
			if checkNames {
				goober, ok := goobers[name]
				if !ok {
					add("allowedGoobers references unknown Goober %q", name)
				} else if goober.Gaggle != "" && goober.Gaggle != def.Spec.Gaggle {
					add("allowedGoobers references Goober %q from another gaggle", name)
				}
			}
		}
		if len(policy.AllowedCapabilities) > 128 {
			add("allowedCapabilities must contain at most 128 entries")
		}
		seenCapabilities := map[string]bool{}
		for _, grant := range policy.AllowedCapabilities {
			if !capability.StageDeclarable(grant) {
				add("allowedCapabilities contains unknown or runner-only capability %q", grant)
			}
			if seenCapabilities[grant] {
				add("allowedCapabilities contains duplicate %q", grant)
			}
			seenCapabilities[grant] = true
		}
		if policy.MaxChildren < 0 || policy.MaxChildren > apiv1.MaxChildWorkflows {
			add("maxChildren must be 1 to %d when set (omission defaults to %d)", apiv1.MaxChildWorkflows, apiv1.DefaultMaxChildWorkflows)
		}
	}
	return problems
}

func childWorkflowFeatures() []Feature {
	var features []Feature
	for _, suffix := range []string{"", ".allowedGoobers", ".allowedCapabilities", ".maxChildren", ".allowPRPublication"} {
		features = append(features, Feature{
			ID: FeatureID("task.childWorkflows" + suffix), Level: SupportPreview,
			SinceVersion: "dev",
			History:      []SupportTransition{{Level: SupportPreview, SinceVersion: "dev"}},
			DSLVersions:  []DSLFeatureSupport{{Version: supportmatrix.V31DSLVersion, Level: SupportPreview}},
		})
	}
	return features
}

func usedChildWorkflowFeatures(def Definition) []Feature {
	used := map[FeatureID]bool{}
	for _, task := range def.Spec.Tasks {
		if p := task.ChildWorkflows; p != nil {
			used["task.childWorkflows"] = true
			used["task.childWorkflows.allowedGoobers"] = true
			if p.AllowedCapabilities != nil {
				used["task.childWorkflows.allowedCapabilities"] = true
			}
			if p.MaxChildren != 0 {
				used["task.childWorkflows.maxChildren"] = true
			}
			if p.AllowPRPublication {
				used["task.childWorkflows.allowPRPublication"] = true
			}
		}
	}
	var features []Feature
	for _, feature := range childWorkflowFeatures() {
		if used[feature.ID] {
			features = append(features, feature)
		}
	}
	return features
}
