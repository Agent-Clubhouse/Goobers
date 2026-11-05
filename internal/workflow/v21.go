package workflow

import (
	"fmt"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	v20 "github.com/goobers/goobers/internal/workflow/v_2_0"
)

// DSL 2.1 has the DSL 2.0 language surface with one explicit behavioral
// opt-in: pr-comment-watch reserves a durable routing budget in provider
// comments. Reuse the frozen interpreter without importing the DSL 3.x
// placement/artifact surface or changing the default version. As with DSL
// 3.0/3.1, api/validate owns version-level preview admission (DVL010/DVL011).
var v21Interpreter = func() versionedInterpreter {
	interpreter := v20Interpreter
	interpreter.compile = compileV21
	interpreter.checkWorkflowAdmission = checkV21WorkflowAdmission
	interpreter.featuresForWorkflow = featuresForV21Workflow
	interpreter.featuresForGaggle = featuresForV21Gaggle
	interpreter.featuresForGoober = featuresForV21Goober
	interpreter.checkWorkflowFeatureSupport = checkV21WorkflowFeatureSupport
	return interpreter
}()

func compileV21(def Definition, config compileConfig) (*Machine, error) {
	if problems := commentWatchBudgetPolicyProblems(def); len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}
	// Keep the 2.1 pin in the compiled machine and its digest. The inherited
	// compiler still checks all declared actions' capabilities and preserves
	// the independent blockUnansweredFeedback/disarm-landing contract.
	return compileNext(def, config)
}

func checkV21WorkflowAdmission(def Definition, goobers map[string]apiv1.GooberSpec) []string {
	return append(v20.CheckWorkflowAdmission(def, goobers), commentWatchBudgetPolicyProblems(def)...)
}

func commentWatchBudgetPolicyProblems(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		// Match the frozen policy dispatcher exactly; do not infer permission
		// from token scope, provider identity, or unrelated command strings.
		if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" || task.Run.Command[1] != "pr-comment-watch" {
			continue
		}
		if !slices.Contains(task.PolicyActions, "comment-on-issue") {
			problems = append(problems, fmt.Sprintf("task %q command %q prescribes policy action %q but policyActions does not declare it", task.Name, "goobers pr-comment-watch", "comment-on-issue"))
		}
	}
	return problems
}

// Preserve feature-level lifecycle metadata while adding the new version's
// row. Version-level preview status is a separate supportmatrix contract.
func inheritedV21Features(features []Feature) []Feature {
	out := make([]Feature, 0, len(features))
	for _, feature := range features {
		inherited := cloneFeature(feature)
		for _, support := range feature.DSLVersions {
			if support.Version != supportmatrix.V2DSLVersion {
				continue
			}
			inherited.DSLVersions = []DSLFeatureSupport{{Version: supportmatrix.V21DSLVersion, Level: support.Level}}
			break
		}
		// Retain non-2.0 metadata for diagnostics, without granting it a 2.1
		// support row (for example an explicitly used 3.1 artifact field).
		out = append(out, inherited)
	}
	return out
}

func featuresForV21Workflow(def Definition) ([]Feature, error) {
	features, err := featuresForNextWorkflow(def)
	return inheritedV21Features(features), err
}

func featuresForV21Gaggle(spec apiv1.GaggleSpec) ([]Feature, error) {
	features, err := featuresForNextGaggle(spec)
	return inheritedV21Features(features), err
}

func featuresForV21Goober(spec apiv1.GooberSpec) ([]Feature, error) {
	features, err := featuresForNextGoober(spec)
	return inheritedV21Features(features), err
}

func checkV21WorkflowFeatureSupport(def Definition, allowPreview bool) []FeatureDiagnostic {
	features, err := featuresForV21Workflow(def)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return checkNextFeatureSupport(features, allowPreview)
}
