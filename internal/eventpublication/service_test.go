package eventpublication

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
)

func TestPublicationOriginRequiresActiveExactBranchAttempt(t *testing.T) {
	id := journal.RunIdentity{RunID: "0af7651916cd43dd8448eb211c80319c", Workflow: "producer", WorkflowVersion: 1, Gaggle: "one", ConfigGeneration: "pinned", Trigger: journal.Trigger{Kind: journal.TriggerManual}}
	writer, err := journal.Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err = writer.Append(journal.Event{Type: journal.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	start := journal.Event{Type: journal.EventStageStarted, Stage: "publish", Branch: 1, Attempt: 1}
	if err = writer.AppendPublicationStageStarted(start, false); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{RunID: id.RunID, WorkflowID: id.Workflow, Gaggle: id.Gaggle, ConfigGeneration: id.ConfigGeneration, TaskID: id.RunID + ":publish", Attempt: 1, Capabilities: []string{string(capability.EventPublish)}}
	_, first, err := activeOrigin(rd, 1, env)
	if err != nil {
		t.Fatal(err)
	}
	// Sibling execution is not a replacement of this branch's active attempt.
	sibling := start
	sibling.Branch = 2
	if err = writer.AppendPublicationStageStarted(sibling, false); err != nil {
		t.Fatal(err)
	}
	if _, current, err := activeOrigin(rd, 1, env); err != nil || current != first {
		t.Fatal(current, err)
	}
	for _, mutate := range []func(*apiv1.InvocationEnvelope){func(e *apiv1.InvocationEnvelope) { e.RunID = "foreign" }, func(e *apiv1.InvocationEnvelope) { e.Gaggle = "foreign" }, func(e *apiv1.InvocationEnvelope) { e.ConfigGeneration = "foreign" }, func(e *apiv1.InvocationEnvelope) { e.Capabilities = nil }} {
		changed := env
		mutate(&changed)
		if _, _, err := activeOrigin(rd, 1, changed); err == nil {
			t.Fatal("accepted mismatched scope")
		}
	}
	if _, _, err := activeOrigin(rd, 0, env); err == nil {
		t.Fatal("accepted wrong branch")
	}
	start.Attempt = 2
	if err = writer.AppendPublicationStageStarted(start, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := activeOrigin(rd, 1, env); err == nil {
		t.Fatal("accepted superseded attempt")
	}
	env.Attempt = 2
	if _, current, err := activeOrigin(rd, 1, env); err != nil || current != first {
		t.Fatal(current, err)
	}
	if err = writer.Append(journal.Event{Type: journal.EventStageFinished, Stage: "publish", Branch: 1, Attempt: 2, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := activeOrigin(rd, 1, env); err == nil {
		t.Fatal("accepted completed attempt")
	}
}
