package engine

import wf "github.com/goobers/goobers/internal/workflow"

const workflowDefinitionAnnotationsChange = "engine-workflow-definition-annotations-v1"

// definition reconstructs every digested field from the immutable run input.
// Workflow replay applies its version gate before compiling this definition;
// manifest validation always checks all fields present in the frozen bytes.
func (in RunInput) definition() wf.Definition {
	return wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Annotations: in.Annotations, Spec: in.Spec}
}
