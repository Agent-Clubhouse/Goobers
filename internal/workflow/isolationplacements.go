package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnersolve"
)

// IsolationStagePlacements overlays operator policy metadata without changing
// any versioned interpreter. Absent mandates and self-denial preserve the original rows.
// Covered gates that cannot dispatch get a self-only requirement, not a new
// placement declaration: policy must never silently migrate a legacy gate.
func IsolationStagePlacements(def Definition, gaggle apiv1.GaggleSpec, goobers map[string]apiv1.GooberSpec, mandates map[string][]string, selfDenied ...bool) ([]runnersolve.StageRequirement, error) {
	rows, err := StagePlacements(def, gaggle, goobers)
	deny := len(selfDenied) > 0 && selfDenied[0]
	if err != nil || (len(mandates) == 0 && !deny) {
		return rows, err
	}
	classes := make(map[string]string, len(def.Spec.Tasks)+len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		classes[task.Name] = string(task.Type)
	}
	for _, gate := range def.Spec.Gates {
		class := "deterministic"
		if gate.Evaluator == apiv1.EvaluatorAgentic {
			class = "agentic"
		}
		classes[gate.Name] = class
	}
	placed := make(map[string]bool, len(rows))
	for i := range rows {
		rows[i].StageClass = classes[rows[i].Stage]
		placed[rows[i].Stage] = true
	}
	for _, gate := range def.Spec.Gates {
		class := classes[gate.Name]
		if !placed[gate.Name] && (len(mandates[class]) > 0 || (deny && gate.Evaluator == apiv1.EvaluatorAgentic)) {
			rows = append(rows, runnersolve.StageRequirement{Stage: gate.Name, StageClass: class, ControlPlane: true})
		}
	}
	return rows, nil
}
