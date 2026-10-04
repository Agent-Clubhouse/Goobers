package main

import (
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestChildInspectionRetainsPinsWithoutCurrentExecutionAuthority(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	ref, err := retainedChildExecutionRef(t.Context(), service.childQueue, pinned.identity, true)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := service.layout.FindRunDir(ref.Child.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = retainedChildExecutionRef(t.Context(), service.childQueue, id, true); err == nil {
		t.Fatal("old source is still current")
	}
	inspected, err := service.inspectChildExecution(t.Context(), id)
	if err != nil || inspected.Runner != nil || inspected.Machine == nil || inspected.Machine.Digest() != id.WorkflowDigest {
		t.Fatal(inspected, err)
	}
	id.Child.SourceDigest = pinned.identity.Child.EnvelopeDigest
	if _, err = service.inspectChildExecution(t.Context(), id); err == nil {
		t.Fatal("tampered source inspected")
	}
}

func TestChildInspectionCallbackBindsCredentialPlaneAfterServiceConstruction(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	setup := &schedulerSetup{Interventions: newInterventionDefinitionRegistry(interventionDefinitionSet{}), RunnerRegistry: newDaemonRunnerRegistry()}
	interventions := newRunInterventionService(service.layout, setup, nil, nil)
	if interventions == nil {
		t.Fatal("service not constructed")
	}
	setup.CredentialPlane = service
	// The callback is exercised through the actual configured service after the
	// late plane publication; no current catalog workflow is needed for the child.
	setup.Interventions.Replace(interventionDefinitionSet{runners: map[string]*runner.Runner{pinned.identity.Gaggle: nil}})
	human, err := intervention.NewHumanService(interventions, service.interactive, newDaemonRunJournalService(service.layout, nil), journal.NewPatternScrubber())
	if err != nil {
		t.Fatal(err)
	}
	_, err = human.InspectInteractiveRun(t.Context(), httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}, pinned.identity.Child.AcceptanceID[len("trigger-"):])
	if err != nil {
		t.Fatal(err)
	}
}
