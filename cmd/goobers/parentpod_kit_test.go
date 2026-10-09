package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestParentKitUsesRetainedSourceAndModelOnlyCredentials(t *testing.T) {
	f := containedParentFixture(t)
	run, env := configuredChildStage(t, f)
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	start := events[len(events)-1]
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	defer unregisterDaemonStageGrants(f.layout.Root, service)
	if err := service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	service.Replace(credentialPlaneDefinitionsFromSet(f.applied))
	blobs := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
	writer := parentKitWriter{service: service, identity: id, blobs: blobs, recorder: run}
	attempt := dispatcher.Attempt{InstanceID: id.InstanceID, RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Stage: "plan", Number: 1, PodAttempt: int(start.Seq), Agentic: true, WorkflowParent: true, Envelope: &env}
	// A mutable source tree cannot replace the archived definition after admission.
	if err := os.WriteFile(f.sourcePath, []byte("invalid live source"), 0600); err != nil {
		t.Fatal(err)
	}
	ceiling := credentials.NewChildCeiling(false, env.Capabilities, env.Capabilities)
	digest, err := writer.WriteKit(t.Context(), attempt, ceiling)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := blobs.Get(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	var kit agentickit.Kit
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatal(err)
	}
	if kit.Envelope.Workspace != "" || !reflect.DeepEqual(kit.Envelope.ChildWorkflowOrigin, env.ChildWorkflowOrigin) || kit.Envelope.ConfigGeneration != id.ConfigGeneration {
		t.Fatal("parent kit lost retained invocation authority")
	}
	for _, grant := range kit.Grants {
		if grant.Capability != "agent:model" {
			t.Fatal("provider credential grant escaped", grant.Capability)
		}
	}
	for _, change := range []func(*dispatcher.Attempt){
		func(a *dispatcher.Attempt) { a.PodAttempt++ },
		func(a *dispatcher.Attempt) { a.WorkflowParent = false },
		func(a *dispatcher.Attempt) { a.Review = true },
	} {
		bad := attempt
		change(&bad)
		before := run.Seq()
		if _, err := writer.WriteKit(t.Context(), bad, ceiling); err == nil || run.Seq() != before {
			t.Fatal("invalid parent kit wrote custody", err)
		}
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteKit(t.Context(), attempt, ceiling); err == nil {
		t.Fatal("ended parent wrote kit")
	}
}
