package shippedworkflows

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
)

func TestDecompositionWorkflowContract(t *testing.T) {
	root := repositoryRoot(t)
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "reference-workflows"))
	if err != nil {
		t.Fatalf("load reference workflows: %v\n%v", err, report)
	}
	definition := findWorkflow(t, set.Workflows, "goobers", "decomposition")
	spec := definition.Spec

	if len(spec.Triggers) != 1 || spec.Triggers[0].Type != apiv1.TriggerSchedule {
		t.Fatalf("triggers = %+v, want one schedule trigger", spec.Triggers)
	}
	if spec.Start != "select-source" || spec.Readiness.MaxConcurrentRuns != 1 ||
		spec.Readiness.MaxRunsPerHour != 1 || spec.RunControls == nil || spec.RunControls.MaxRepasses != 1 {
		t.Fatalf("workflow entry/readiness contract = %+v", spec)
	}

	selectSource := requireTask(t, spec, "select-source")
	design := requireTask(t, spec, "design-slices")
	validate := requireTask(t, spec, "validate-plan")
	publish := requireTask(t, spec, "publish-slices")
	park := requireTask(t, spec, "park-for-human")
	if selectSource.Next != "design-slices" || design.Next != "validate-plan" ||
		validate.Next != "parent-unchanged" || publish.Next != "publication-clean" || park.Next != "@escalate" {
		t.Fatal("unexpected decomposition stage wiring")
	}
	if design.Goober != "decomposer" ||
		!reflect.DeepEqual(design.Capabilities, []string{"repo:read", "github:issues:read", "agent:model"}) ||
		len(design.PolicyActions) != 0 || design.Inputs["artifactFile"] != "plan.json" {
		t.Fatalf("design-slices is not read-only or does not declare plan.json: %+v", design)
	}
	if !reflect.DeepEqual(publish.Capabilities, []string{"github:issues:write"}) ||
		!reflect.DeepEqual(park.Capabilities, []string{"github:issues:write"}) {
		t.Fatalf("deterministic mutation grants are not confined: publish=%v park=%v", publish.Capabilities, park.Capabilities)
	}

	parentGate := requireGate(t, spec, "parent-unchanged")
	decisionGate := requireGate(t, spec, "decision-resolved")
	planGate := requireGate(t, spec, "plan-valid")
	publicationGate := requireGate(t, spec, "publication-clean")
	if parentGate.Automated.Params["key"] != "conflict" || parentGate.Automated.Params["equals"] != "false" ||
		parentGate.Branches["pass"] != "decision-resolved" || parentGate.Branches["fail"] != "park-for-human" {
		t.Fatalf("parent-unchanged branches = %v", parentGate.Branches)
	}
	if decisionGate.Automated.Params["key"] != "unresolvedDecision" ||
		decisionGate.Automated.Params["equals"] != "false" ||
		decisionGate.Branches["pass"] != "plan-valid" || decisionGate.Branches["fail"] != "park-for-human" {
		t.Fatalf("decision-resolved gate = %+v", decisionGate)
	}
	if planGate.Branches["pass"] != "publish-slices" || planGate.Branches["fail"] != "design-slices" ||
		planGate.Branches["escalate"] != "park-for-human" || planGate.MaxRepasses != 1 {
		t.Fatalf("plan-valid bounded branches = %+v", planGate)
	}
	if publicationGate.Automated.Params["key"] != "publicationConflict" ||
		publicationGate.Automated.Params["equals"] != "false" ||
		publicationGate.Branches["pass"] != "" || publicationGate.Branches["fail"] != "park-for-human" {
		t.Fatalf("publication-clean gate = %+v", publicationGate)
	}
	assertOutputGateRoute(t, parentGate, map[string]interface{}{"conflict": true}, gate.OutcomeFail)
	assertOutputGateRoute(t, decisionGate, map[string]interface{}{"unresolvedDecision": true}, gate.OutcomeFail)
	assertOutputGateRoute(t, publicationGate, map[string]interface{}{"publicationConflict": true}, gate.OutcomeFail)

	var decomposer apiv1.Goober
	for _, candidate := range set.Goobers {
		if candidate.Name == "decomposer" {
			decomposer = candidate
			break
		}
	}
	if decomposer.Name == "" {
		t.Fatal("decomposer goober not found")
	}
	if !reflect.DeepEqual(decomposer.Spec.Capabilities, []string{"repo:read", "github:issues:read", "agent:model"}) ||
		len(decomposer.Spec.PolicyActions) != 0 {
		t.Fatalf("decomposer grants are not read-only: %+v", decomposer.Spec)
	}
	if strings.Contains(strings.Join(decomposer.Spec.Capabilities, ","), ":write") {
		t.Fatalf("decomposer has write capability: %v", decomposer.Spec.Capabilities)
	}
}

func assertOutputGateRoute(t *testing.T, workflowGate apiv1.Gate, outputs map[string]interface{}, want string) {
	t.Helper()
	check := gate.DefaultChecks()[workflowGate.Automated.Check]
	got, err := check(outputs, workflowGate.Automated.Params)
	if err != nil {
		t.Fatalf("evaluate gate %q: %v", workflowGate.Name, err)
	}
	if got != want || workflowGate.Branches[got] != "park-for-human" {
		t.Fatalf("gate %q routed output %v to %q/%q, want %q/park-for-human",
			workflowGate.Name, outputs, got, workflowGate.Branches[got], want)
	}
}
