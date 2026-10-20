package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestParentKitUsesRetainedSourceAndExistingModelGrants(t *testing.T) {
	for _, selected := range []apiv1.Harness{apiv1.HarnessClaudeCode, apiv1.HarnessCodex, apiv1.HarnessCopilot} {
		t.Run(string(selected), func(t *testing.T) { testParentKitUsesRetainedSourceAndExistingModelGrants(t, selected) })
	}
}

func testParentKitUsesRetainedSourceAndExistingModelGrants(t *testing.T, selected apiv1.Harness) {
	t.Setenv("PARENT_TEST_MODEL_TOKEN", "parent-test-secret-must-not-be-in-kit")
	f := containedParentHarnessFixture(t, selected)
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
	snapshot, release, err := writer.snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := (agenticKitWriter{registrar: service.shared}).buildSnapshotKit(service.layout, snapshot, env, kitModeFor(attempt))
	release()
	if err != nil {
		t.Fatal(err)
	}
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
	if bytes.Contains(raw, []byte("parent-test-secret-must-not-be-in-kit")) {
		t.Fatal("materialized model credential escaped into kit")
	}
	var kit agentickit.Kit
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatal(err)
	}
	if kit.Envelope.Workspace != "" || !reflect.DeepEqual(kit.Envelope.ChildWorkflowOrigin, env.ChildWorkflowOrigin) || kit.Envelope.ConfigGeneration != id.ConfigGeneration {
		t.Fatal("parent kit lost retained invocation authority")
	}
	files := kit.SkillPackages[childworkflow.ParentAuthoringSkillName]
	if len(files) != 2 || files[0].Path != "SKILL.md" || files[1].Path != "catalog.json" {
		t.Fatal("actual parent kit omitted authoring inputs")
	}
	var catalog childworkflow.AuthoringCatalog
	if err := json.Unmarshal([]byte(files[1].Content), &catalog); err != nil || catalog.Gaggle != id.Gaggle || len(catalog.Goobers) != 1 || catalog.Goobers[0].Name != "coder" {
		t.Fatal("actual parent authoring catalog lost pinned scope", err)
	}
	if len(kit.Grants) != 1 {
		t.Fatalf("expected exactly one model grant, got %d", len(kit.Grants))
	}
	for _, grant := range kit.Grants {
		if grant.Capability != "agent:model" || !slices.Contains(env.Capabilities, grant.Capability) || !slices.ContainsFunc(baseline.Grants, func(existing agentickit.Grant) bool { return reflect.DeepEqual(existing, grant) }) {
			t.Fatal("provider credential grant escaped", grant.Capability)
		}
	}
	for key, binding := range kit.EnvCapabilities {
		if key != "agent:model" || !slices.Contains(env.Capabilities, key) || !reflect.DeepEqual(binding, baseline.EnvCapabilities[key]) {
			t.Fatal("parent kit introduced a credential binding", key)
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

func TestCopilotParentDoesNotEnableGeneratedChildKit(t *testing.T) {
	kit := &agentickit.Kit{Goobers: map[string]apiv1.GooberSpec{"parent": {Harness: apiv1.HarnessCopilot}}}
	ceiling := credentials.NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"})
	if err := narrowAgenticKitCredentials(kit, ceiling); err != nil {
		t.Fatal(err)
	}
	if len(kit.Grants) != 0 || len(kit.EnvCapabilities) != 0 {
		t.Fatal("parent acquired a model grant absent from its original stage")
	}
	if err := narrowChildKit(kit, ceiling); err == nil {
		t.Fatal("Copilot model token delegated to a generated child")
	}
}
