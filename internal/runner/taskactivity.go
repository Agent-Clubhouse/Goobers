package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func taskStartedEvent(task apiv1.Task, attempt int, class journal.AttemptClass) journal.Event {
	event := journal.Event{Type: journal.EventStageStarted, Stage: task.Name, Attempt: attempt, AttemptClass: class}
	if task.Type == apiv1.TaskAgentic {
		event.Runner = map[string]any{"goober": task.Goober}
	}
	return event
}
