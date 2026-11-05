//go:build cloudintegration

package childengine

import (
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

// The scripted child must publish its summary in the durable journal output
// consumed by both local and cloud terminal-result observers.
func TestQualificationChildResultSurvivesJournalCapture(t *testing.T) {
	root := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&engine.Activities{Journal: writer})
	stages := qualificationStages{}
	for _, name := range []string{engine.ActRunDeterministic, "RunDeterministicCancelSafe"} {
		env.RegisterActivityWithOptions(stages.child, activity.RegisterOptions{Name: name, DisableAlreadyRegisteredCheck: true})
	}
	in := engine.RunInput{RunID: strings.Repeat("c", 32), Gaggle: "example", WorkflowName: "receipt-child", Version: 1, DSLVersion: "3.1", LiveJournal: true,
		Spec: apiv1.WorkflowSpec{Gaggle: "example", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "check", Tasks: []apiv1.Task{{Name: "check", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: wf.TerminalComplete}}}}
	env.ExecuteWorkflow(engine.Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(filepath.Join(root, in.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	input, found, err := childworkflow.TerminalInputFromJournal(rd, events, journal.PhaseFromEvents(events))
	if err != nil || !found || input.State != triggerqueue.ChildCompleted || input.Summary != "qualification child result" {
		t.Fatalf("terminal result=%+v found=%v err=%v", input, found, err)
	}
}
