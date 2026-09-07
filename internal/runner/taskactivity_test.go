package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestTaskStartedRecordsPinnedOwnerOnlyForAgenticTasks(t *testing.T) {
	task := apiv1.Task{Name: "work", Type: apiv1.TaskAgentic, Goober: "pinned-persona"}
	event := taskStartedEvent(task, 2, journal.AttemptClass("infra"))
	if event.Runner["goober"] != "pinned-persona" || event.Attempt != 2 || event.Stage != "work" {
		t.Fatalf("start: %+v", event)
	}
	task.Type = apiv1.TaskDeterministic
	if got := taskStartedEvent(task, 1, ""); got.Runner != nil {
		t.Fatal("invented deterministic owner")
	}
}
