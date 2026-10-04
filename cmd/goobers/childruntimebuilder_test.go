package main

import (
	"context"
	"os"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestChildRuntimeBuilderUsesRetainedSourceWithoutCatalogRegistration(t *testing.T) {
	f := newPinnedChildFixture(t)
	a := f.load(t)
	validator, err := childworkflow.NewValidator(a.Admission)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := validator.Validate([]byte(childValidationProposal))
	if err != nil {
		t.Fatal(err)
	}
	store, err := executionGenerationStore(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	retainer := &configgeneration.Retainer{Store: store}
	build := func(layout instance.Layout, set *instance.ConfigSet, report *validate.Report) (*schedulerDefinitions, error) {
		if len(set.Workflows) != 1 || set.Workflows[0].Name != proposal.Workflow.Name {
			t.Fatal("named parent catalog leaked into generated runtime")
		}
		return buildSchedulerDefinitions(schedulerDefinitionsInput{Layout: layout, Config: f.cfg, Definitions: set, Validation: report,
			RunnerRegistry: newDaemonRunnerRegistry(), ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer}})
	}
	start := childExecutionStart{Proposal: proposal, childExecutionRef: childExecutionRef{Envelope: childworkflow.ChildStartEnvelope{
		Backend: childworkflow.BackendRunner, ConfigGeneration: a.ConfigGeneration, Gaggle: a.Origin.Gaggle, Workflow: proposal.Workflow.Name, WorkflowDigest: proposal.Machine.Digest(),
	}}}
	// An unapplied edit must never be used by the execution builder.
	if err = os.WriteFile(f.sourcePath, []byte("pending invalid edit"), 0600); err != nil {
		t.Fatal(err)
	}
	builder := childRuntimeBuilderFor(f.layout, retainer, f.cfg, build)
	runtime, err := builder(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.release()
	if runtime.machine.Digest() != proposal.Machine.Digest() || runtime.entry.Workflow != proposal.Workflow.Name || runtime.entry.Gaggle != a.Origin.Gaggle {
		t.Fatal("runtime pins changed")
	}
	if f.applied.Workflows[0].Name == proposal.Workflow.Name {
		t.Fatal("generated workflow mutated applied catalog")
	}
}
