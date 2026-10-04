package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sessionops"
)

func TestSessionNativeWriterFactoryDoesNotGrantMissingAction(t *testing.T) {
	setup, _ := interactiveExecutionFixture(t)
	gaggle := setup.Definitions.Gaggles[0]
	gaggle.Spec.InteractiveAccess.Actions = append(gaggle.Spec.InteractiveAccess.Actions, "session.message")
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{gaggle}, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	actor := sessioning.Actor{Issuer: "issuer", Subject: "human"}
	lease, err := permissions.BeginSessionExecution(t.Context(), httpapi.Principal{Issuer: actor.Issuer, Subject: actor.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}}, gaggle.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	factory := workbenchSessionWriter(nil)
	source := sessionops.SourceContext{Identity: journal.RunIdentity{Gaggle: gaggle.Name}, Actor: actor, Lease: lease, RetainedGaggle: gaggle}
	writer, err := factory(t.Context(), source)
	if writer != nil || err != nil {
		t.Fatal("missing native write policy disabled model-only turn", err)
	}
	source.Actor.Subject = "other"
	if _, err = factory(t.Context(), source); err == nil {
		t.Fatal("source factory accepted another actor")
	}
}
