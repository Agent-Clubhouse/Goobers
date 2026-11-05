package harness

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

// WithInvocationGuidance copies the immutable executor configuration for one
// source-bound invocation. Shared Goober profiles retain their original skills
// and instructions when a parent receives its host-supplied authoring catalog.
func (e *Executor) WithInvocationGuidance(instructions string, selected apiv1.Harness, packages map[string][]workflow.SkillFile) *Executor {
	copy := *e
	copy.instructions = instructions
	WithSkills(selected, packages)(&copy)
	return &copy
}
