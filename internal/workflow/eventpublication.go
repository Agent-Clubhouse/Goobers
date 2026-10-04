package workflow

import (
	"fmt"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func eventPublicationProblems(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Inputs["kind"] != eventing.KindPublishEvent {
			continue
		}
		if def.DSLVersion != supportmatrix.V31DSLVersion {
			problems = append(problems, fmt.Sprintf("task %q publish-event requires dslVersion %q", task.Name, supportmatrix.V31DSLVersion))
		}
		if task.Type != apiv1.TaskDeterministic || !slices.Contains(task.Capabilities, string(capability.EventPublish)) {
			problems = append(problems, fmt.Sprintf("task %q publish-event requires type=deterministic and capability event:publish", task.Name))
		}
	}
	return problems
}
