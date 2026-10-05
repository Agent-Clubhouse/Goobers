package main

import (
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func TestWorkbenchResolutionAvailabilityRequiresInstalledSessionAndSourcePolicy(t *testing.T) {
	setup, pin := interactiveExecutionFixture(t)
	g := setup.Definitions.Gaggles[0].DeepCopy()
	g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "issues", Kind: "backlog", Writes: &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"labels"}}}}}
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "backlog.resolve")
	if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	permission := func() apicontract.InteractiveActionPermission {
		result, err := setup.InteractiveAccess.InteractiveCapabilities(t.Context(), p, g.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range result.Actions {
			if action.Action == "backlog.resolve" {
				return action
			}
		}
		t.Fatal("missing declared resolution action")
		return apicontract.InteractiveActionPermission{}
	}
	if permission().Available {
		t.Fatal("resolver advertised before installation")
	}
	queue := acceptedService(t, filepath.Join(pin.layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
	u := &upSession{}
	u.setup, u.l, u.durableTriggers = setup, pin.layout, queue
	read := &workbenchservice.Service{Permissions: setup.InteractiveAccess, Backlog: workbenchservice.ProviderFactory{SchedulerDirectory: pin.layout.SchedulerDir(), Registrar: setup.SharedRegistry}.Backlog}
	u.installWorkbenchWrites(read)
	if setup.SessionBacklogResolver != nil || permission().Available {
		t.Fatal("resolver advertised without scoped tool runtime")
	}
	u.credentialPlane = &daemonCredentialService{grants: &stageGrantIssuer{endpoint: "https://daemon.invalid"}}
	u.installWorkbenchResolution(read, true)
	if setup.SessionBacklogResolver == nil {
		t.Fatal("resolver factory was not installed")
	}
	if permission().Available {
		t.Fatal("resolver advertised without session runtime")
	}
	setup.InteractiveAccess.SetSessionsAvailable(true)
	if !permission().Available {
		t.Fatal("installed resolver unavailable", permission())
	}
	g.Spec.Workbench.Sources[0].Writes.Fields = []apiv1.WorkbenchField{"title"}
	if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	if permission().Available {
		t.Fatal("resolution bypassed source label policy")
	}
	g.Spec.Workbench.Sources[0].Writes.Fields = []apiv1.WorkbenchField{"labels"}
	g.Spec.InteractiveAccess.Actions = nil
	if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	if permission().Authorized || permission().Available {
		t.Fatal("resolution survived action revocation")
	}
}
