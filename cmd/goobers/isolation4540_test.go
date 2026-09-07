package main

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workflow"
)

func TestIsolationMandatesRefuseWholeWorkflowLocalFallback(t *testing.T) {
	cfg := &instance.Config{
		Isolation: &instance.IsolationConfig{Mandates: []instance.IsolationMandate{{Match: instance.IsolationMatch{StageClass: "agentic"}, Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral}}}},
		Runners:   []instance.RunnerEntry{{Name: "self", Host: "self"}, {Name: "protected", Host: "image:protected", Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral}}},
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: "web", Workflow: "implement"}
	machines := map[localscheduler.WorkflowIdentity]*workflow.Machine{identity: {Def: workflow.Definition{Name: identity.Workflow, Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{Name: "agent", Type: apiv1.TaskAgentic}}}}}}
	decisions, err := placementRefusals(cfg, &instance.ConfigSet{}, nil, machines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason := decisions.Refusals[identity]; !strings.Contains(reason, "agent") || !strings.Contains(reason, "tmp:ephemeral") {
		t.Fatalf("fallback was admitted without the operator floor: %+v", decisions)
	}
	decisions, err = placementRefusals(cfg, &instance.ConfigSet{}, nil, machines, map[localscheduler.WorkflowIdentity]engineSelection{identity: {UseEngine: true}})
	if err != nil || len(decisions.Refusals) != 0 || decisions.EngineDeferred[identity] == "" {
		t.Fatalf("engine-selected protected lane refused: %+v, %v", decisions, err)
	}
}

func TestIsolationMandatesStatusTextAndJSON(t *testing.T) {
	status := readservice.SchedulerStatus{IsolationMandates: map[string][]string{"agentic": {"tmp:ephemeral", "network:allowlist"}}}
	if got := isolationMandateStatusLines(status); got != "Isolation mandate (agentic): tmp:ephemeral, network:allowlist\n" {
		t.Fatal(got)
	}
	if got := isolationMandateStatusLines(readservice.SchedulerStatus{}); got != "" {
		t.Fatal(got)
	}
	raw, err := json.Marshal(statusJSONOutput{IsolationMandates: status.IsolationMandates})
	if err != nil || !strings.Contains(string(raw), `"isolationMandates":{"agentic":["tmp:ephemeral","network:allowlist"]}`) {
		t.Fatalf("status JSON: %s, %v", raw, err)
	}
}
