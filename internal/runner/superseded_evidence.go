package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
)

// reviewerContextPointers is the local runner's binding of
// ReviewerContextPointers: it reads the run journal and withholds superseded
// validation evidence from an agentic reviewer of an agentic subject. A
// journal that cannot be read leaves the pointers unchanged; the gate's own
// repass-cause resolution reads the same journal and fails the gate closed.
func reviewerContextPointers(jr executionJournal, machine *workflow.Machine, subjectStage string, pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	if jr == nil || machine == nil || len(pointers) == 0 {
		return pointers
	}
	if subject, ok := machine.Task(subjectStage); !ok || subject.Type != apiv1.TaskAgentic {
		return pointers
	}
	reader, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return pointers
	}
	events, err := reader.Events()
	if err != nil {
		return pointers
	}
	return runsemantics.ReviewerContextPointers(events, subjectStage, deterministicTaskPredicate(machine), pointers)
}

func deterministicTaskPredicate(machine *workflow.Machine) func(string) bool {
	return func(stage string) bool {
		task, ok := machine.Task(stage)
		return ok && task.Type == apiv1.TaskDeterministic
	}
}
