package dslmigrate

import (
	"fmt"

	"gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
)

// applyV30ToV31 deliberately changes only the version pin, which Migrate
// patches in the original source. DSL 3.1 carries forward 3.0's positional
// artifact behavior; named slots and consumer bindings are author opt-ins.
// Neither expectedOutputs nor generated artifact names establish an artifact
// contract, so this edge must not infer artifactSlots or artifactInputs from
// them. It does note every shell stage whose expectedOutputs becomes a
// runtime contract under 3.1 (#5175): such a stage now fails when its result
// file omits a declared key, so authors can check each one before writing.
func applyV30ToV31(source []byte, _ *yaml.Node) (bool, []string, error) {
	var wf apiv1.Workflow
	if err := k8syaml.Unmarshal(source, &wf); err != nil {
		return false, nil, fmt.Errorf("parse workflow to find enforced expectedOutputs: %w", err)
	}
	var notes []string
	for _, task := range wf.Spec.Tasks {
		if v30.EnforcesExpectedOutputs(supportmatrix.V31DSLVersion, task) {
			notes = append(notes, fmt.Sprintf(
				"task %s: expectedOutputs %q are enforced from DSL 3.1; the stage fails if its result file omits a declared key",
				task.Name, task.ExpectedOutputs))
		}
	}
	return false, notes, nil
}
