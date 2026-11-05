package localscheduler

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestExactWorkItemPreparedTriggerCarriesUnambiguousItemIdentity(t *testing.T) {
	scheduler := &Scheduler{}
	trigger, _, err := scheduler.prepareOrdinaryTrigger(t.Context(), WorkflowEntry{Workflow: "implementation"}, PreparedTriggerOptions{WorkItemID: "42"})
	if err != nil || trigger.Kind != journal.TriggerManual || trigger.Ref != "work-item:42" {
		t.Fatal(trigger, err)
	}
	ordinary, _, err := scheduler.prepareOrdinaryTrigger(t.Context(), WorkflowEntry{Workflow: "implementation"}, PreparedTriggerOptions{})
	if err != nil || ordinary.Ref != "implementation" {
		t.Fatal("ordinary trigger changed", ordinary, err)
	}
}
