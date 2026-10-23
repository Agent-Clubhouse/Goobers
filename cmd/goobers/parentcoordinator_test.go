package main

import (
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func TestContainedParentCoordinatorKeepsOrdinaryPlacementEnforcement(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote parent", true: "mixed ordinary stage"}[ordinary], func(t *testing.T) {
			f := containedParentFixture(t)
			_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
			if err != nil {
				t.Fatal(err)
			}
			if ordinary {
				task := machine.Def.Spec.Tasks[0]
				task.Name, task.ChildWorkflows = "ordinary", nil
				machine.Def.Spec.Tasks = append(machine.Def.Spec.Tasks, task)
			}
			identity := localscheduler.WorkflowIdentity{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow}
			machines := map[localscheduler.WorkflowIdentity]*workflow.Machine{identity: machine}
			placements := localPlacements(f.cfg, f.applied, machines)
			selected := placements[identity]
			if len(selected.containedParentStages) != 1 || !selected.containedParentStages["plan"] {
				t.Fatal("parent coordinator not selected", selected)
			}
			caps := selected.parentCoordinatorHostCapabilities([]string{"isolated-parent"})
			if slices.Contains(caps, "isolated-parent") != ordinary {
				t.Fatal("ordinary host capability changed", caps)
			}
			decisions, err := placementRefusals(f.cfg, f.applied, goobersByName(f.applied), machines, placements)
			if err != nil {
				t.Fatal(err)
			}
			refusal, refused := decisions.Refusals[identity]
			if refused != ordinary || (ordinary && !strings.Contains(refusal, "ordinary")) {
				t.Fatal("ordinary placement enforcement changed", refusal)
			}
		})
	}
}

func TestContainedParentCoordinatorRequiresAuthenticatedPlacement(t *testing.T) {
	f := containedParentFixture(t)
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.API.PodTokenKeyFile = ""
	identity := localscheduler.WorkflowIdentity{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow}
	machines := map[localscheduler.WorkflowIdentity]*workflow.Machine{identity: machine}
	placements := localPlacements(f.cfg, f.applied, machines)
	selected := placements[identity]
	if selected.refusal == "" || len(selected.containedParentStages) != 0 {
		t.Fatal("unsigned parent transport admitted")
	}
	decisions, err := placementRefusals(f.cfg, f.applied, goobersByName(f.applied), machines, placements)
	if err != nil {
		t.Fatal(err)
	}
	if decisions.Refusals[identity] == "" {
		t.Fatal("unsupported parent fell back to the host")
	}
}

func TestLocalPlacementsRefusesUnsupportedParentWithoutFailingAllDefinitions(t *testing.T) {
	f := containedParentFixture(t)
	f.applied.Gaggles[0].Spec.AdditionalRepos = []apiv1.RepoRef{{
		Provider: apiv1.ProviderGitHub,
		Owner:    "example",
		Name:     "secondary",
	}}
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{
		Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan",
	}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow}
	machines := map[localscheduler.WorkflowIdentity]*workflow.Machine{identity: machine}
	placements := localPlacements(f.cfg, f.applied, machines)
	if refusal := placements[identity].refusal; !strings.Contains(refusal, "additional repositories") {
		t.Fatalf("local placement refusal = %q", refusal)
	}
	decisions, err := placementRefusals(f.cfg, f.applied, goobersByName(f.applied), machines, placements)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decisions.Refusals[identity], "additional repositories") {
		t.Fatalf("workflow refusal = %+v", decisions.Refusals)
	}
}
