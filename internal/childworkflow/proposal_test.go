package childworkflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

const validProposal = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: generated-check
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: check
  tasks:
    - name: check
      type: deterministic
      goal: Run the local check
      run:
        command: ["true"]
      next: ""
`

func testContext() AdmissionContext {
	return AdmissionContext{
		Config: &instance.Config{},
		Gaggle: apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: "github"}}},
		ParentTask: apiv1.Task{Name: "plan", Type: apiv1.TaskAgentic, ChildWorkflows: &apiv1.ChildWorkflowPolicy{
			AllowedGoobers: []string{"coder"}, AllowedCapabilities: []string{"agent:model", "repo:read"},
		}},
		ConfigDigest:        "sha256:pinned",
		GrantedCapabilities: []string{"agent:model", "repo:read"},
		Goobers:             map[string]apiv1.GooberSpec{"coder": {Gaggle: "web", Harness: "fake", Capabilities: []string{"agent:model", "repo:read"}}},
		KnownChecks:         []string{"status-equals"}, KnownHarnesses: []string{"fake"},
		Backend: BackendRunner,
	}
}

func validator(t *testing.T, context AdmissionContext) *Validator {
	t.Helper()
	v, err := NewValidator(context)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func requireCode(t *testing.T, v *Validator, source, code string) {
	t.Helper()
	result, err := v.Validate([]byte(source))
	var problem *ValidationError
	if result != nil || !errors.As(err, &problem) || problem.Diagnostics[0].Code != code {
		t.Fatalf("Validate got result=%v error=%v; want %s", result, err, code)
	}
}

func TestProposalPreservesSourceAndCanonicalIdentity(t *testing.T) {
	v := validator(t, testContext())
	source := []byte(validProposal)
	first, err := v.Validate(source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := v.Validate([]byte("# another representation\n" + validProposal))
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceDigest == second.SourceDigest || first.CanonicalDigest != second.CanonicalDigest || first.Machine.Digest() != second.Machine.Digest() {
		t.Fatal("formatting must change source identity but preserve canonical and compiled identities")
	}
	if first.ConfigDigest != "sha256:pinned" || first.PolicyDigest == "" || first.Workflow.Spec.Triggers[0].Type != apiv1.TriggerManual {
		t.Fatal("pinned provenance or source trigger was lost")
	}
	source[0] = '!'
	if string(first.Source) != validProposal {
		t.Fatal("returned source aliases caller bytes")
	}
	first.Workflow.Spec.Tasks[0].Run.Command[0] = "other"
	third, err := v.Validate([]byte(validProposal))
	if err != nil || third.Workflow.Spec.Tasks[0].Run.Command[0] != "true" {
		t.Fatal("result mutation changed subsequent validation")
	}
}

func TestCanonicalIdentityDoesNotRoundLargeIntegers(t *testing.T) {
	v := validator(t, testContext())
	a := strings.Replace(validProposal, "name: generated-check", "name: generated-check\n  generation: 9007199254740992", 1)
	b := strings.Replace(a, "9007199254740992", "9007199254740993", 1)
	first, err := v.Validate([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	second, err := v.Validate([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	if first.CanonicalDigest == second.CanonicalDigest {
		t.Fatal("canonical digest rounded distinct authored integers")
	}
}

func TestProposalParserRefusesAmbiguousOrUnboundedInput(t *testing.T) {
	v := validator(t, testContext())
	cases := []struct{ name, source, code string }{
		{"empty", "", "size"},
		{"oversized", strings.Repeat(" ", MaxProposalBytes+1), "size"},
		{"documents", validProposal + "\n---\n" + validProposal, "documents"},
		{"empty second document", validProposal + "\n---\n", "documents"},
		{"new resource", validProposal + "\n---\nkind: Goober\n", "documents"},
		{"scalar", "hello", "syntax"},
		{"duplicate", strings.Replace(validProposal, "gaggle: web", "gaggle: other\n  gaggle: web", 1), "syntax"},
		{"alias", strings.Replace(validProposal, "command: [\"true\"]", "command: &cmd [\"true\"]\n        env: {UNTRUSTED: *cmd}", 1), "syntax"},
		{"missing version", strings.Replace(validProposal, "dslVersion: \"3.1\"\n", "", 1), "dsl_version"},
		{"old version", strings.Replace(validProposal, "\"3.1\"", "\"3.0\"", 1), "dsl_version"},
		{"schedule", strings.Replace(validProposal, "type: manual", "type: schedule, schedule: '@every 1h'", 1), "trigger"},
		{"empty trigger", strings.Replace(validProposal, "[{type: manual}]", "[]", 1), "trigger"},
		{"extra trigger field", strings.Replace(validProposal, "type: manual", "type: manual, priority: 0", 1), "trigger"},
		{"unknown field", strings.Replace(validProposal, "start: check", "start: check\n  invented: yes", 1), "schema"},
		{"wrong kind", strings.Replace(validProposal, "kind: Workflow", "kind: Goober", 1), "schema"},
		{"recursion", strings.Replace(validProposal, "type: deterministic", "type: deterministic\n      childWorkflows: null", 1), "recursion"},
		{"foreign gaggle", strings.Replace(validProposal, "gaggle: web", "gaggle: another", 1), "scope"},
		{"foreign namespace", strings.Replace(validProposal, "name: generated-check", "name: generated-check\n  namespace: another", 1), "scope"},
		{"host path", strings.Replace(validProposal, "start: check", "start: check\n  outboxMirrorPath: /tmp/escape", 1), "scope"},
		{"broken graph", strings.Replace(validProposal, "next: \"\"", "next: missing", 1), "compile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { requireCode(t, v, tc.source, tc.code) })
	}
	var wf apiv1.Workflow
	good, err := v.Validate([]byte(validProposal))
	if err != nil {
		t.Fatal(err)
	}
	wf = good.Workflow
	for i := 1; i < 129; i++ {
		task := wf.Spec.Tasks[0]
		task.Name = fmt.Sprintf("task-%d", i)
		wf.Spec.Tasks = append(wf.Spec.Tasks, task)
	}
	raw, _ := json.Marshal(wf)
	requireCode(t, v, string(raw), "states")
}

func TestTaskAndReviewerAuthorityAreDifferent(t *testing.T) {
	context := testContext()
	context.Goobers["coder"] = apiv1.GooberSpec{Gaggle: "web", Harness: "fake", Capabilities: []string{"agent:model", "repo:read", "github:pr:write"}}
	v := validator(t, context)
	agentic := strings.Replace(validProposal, "type: deterministic", "type: agentic\n      goober: coder\n      capabilities: [agent:model]", 1)
	agentic = strings.Replace(agentic, "      run:\n        command: [\"true\"]\n", "", 1)
	if _, err := v.Validate([]byte(agentic)); err != nil {
		t.Fatalf("task subset rejected: %v", err)
	}
	requireCode(t, v, strings.Replace(agentic, "goober: coder", "goober: missing", 1), "goober")
	requireCode(t, v, strings.Replace(agentic, "capabilities: [agent:model]", "capabilities: [agent:model, github:pr:write]", 1), "capability")
	gate := strings.Replace(validProposal, "next: \"\"", "next: review", 1) + `  gates:
    - name: review
      evaluator: agentic
      agentic: {goober: coder}
      branches: {pass: "", fail: "@abort"}
`
	result, err := v.Validate([]byte(gate))
	var problem *ValidationError
	if result != nil || !errors.As(err, &problem) || problem.Diagnostics[0].Code != "capability" || problem.Diagnostics[0].Stage != "review" {
		t.Fatalf("reviewer full grant escaped policy: %v", err)
	}
}

func TestPublicationRequiresBothGrantsAndRecognizesCredentialSurfaces(t *testing.T) {
	for _, grant := range []string{"repo:push", "configrepo:write", "provider:pr:write", "github:pr:write", "ado:pr:write", "github:pr:merge", "ado:pr:complete"} {
		t.Run(grant, func(t *testing.T) {
			context := testContext()
			context.ParentTask.ChildWorkflows.AllowedCapabilities = append(context.ParentTask.ChildWorkflows.AllowedCapabilities, grant)
			context.GrantedCapabilities = append(context.GrantedCapabilities, grant)
			source := strings.Replace(validProposal, "type: deterministic", fmt.Sprintf("type: deterministic\n      capabilities: [%s]", grant), 1)
			requireCode(t, validator(t, context), source, "publication")
			context.ParentTask.ChildWorkflows.AllowPRPublication = true
			requireCode(t, validator(t, context), source, "publication")
			context.AllowPRPublication = true
			if _, err := validator(t, context).Validate([]byte(source)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPinnedInputsCannotBeWidenedAfterConstruction(t *testing.T) {
	context := testContext()
	v := validator(t, context)
	context.ParentTask.ChildWorkflows.AllowedCapabilities = append(context.ParentTask.ChildWorkflows.AllowedCapabilities, "github:issues:write")
	context.GrantedCapabilities = append(context.GrantedCapabilities, "github:issues:write")
	context.ParentTask.ChildWorkflows.AllowPRPublication = true
	context.ParentTask.ChildWorkflows.AllowedGoobers[0] = "other"
	context.Goobers["coder"] = apiv1.GooberSpec{Gaggle: "another"}
	source := strings.Replace(validProposal, "type: deterministic", "type: deterministic\n      capabilities: [github:issues:write]", 1)
	requireCode(t, v, source, "capability")
	if _, err := v.Validate([]byte(validProposal)); err != nil {
		t.Fatal(err)
	}
	agentic := strings.Replace(validProposal, "type: deterministic", "type: agentic\n      goober: coder\n      capabilities: [agent:model]", 1)
	agentic = strings.Replace(agentic, "      run:\n        command: [\"true\"]\n", "", 1)
	if _, err := v.Validate([]byte(agentic)); err != nil {
		t.Fatalf("input map/slice mutation changed pinned Goober: %v", err)
	}
}

func TestNormalContractChecksApplyToGeneratedMachines(t *testing.T) {
	source := strings.Replace(validProposal, "      next: \"\"", "      expectedOutputs: [present]\n      inputs: {resultFile: result.json}\n      next: consume", 1) + `    - name: consume
      type: deterministic
      goal: Consume an output the producer never declared
      inputsFrom: {value: check.absent}
      run: {command: ["true"]}
`
	requireCode(t, validator(t, testContext()), source, "contracts")
}

func TestEngineAndRunnerPinRemoteExecutablePlacement(t *testing.T) {
	context := testContext()
	context.Config.Runners = []instance.RunnerEntry{
		{Name: "self", Host: "self", Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux}},
		{Name: "tools", Host: "ghcr.io/example/tools:v1", Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Shell: true, Capabilities: []string{"special-tool"}}},
	}
	source := strings.Replace(validProposal, "type: deterministic", "type: deterministic\n      runsOn: {capabilities: [special-tool]}", 1)
	local, err := validator(t, context).Validate([]byte(source))
	if err != nil || len(local.Placements) != 1 || local.Placements[0].Self {
		t.Fatal("coordinating runner lost remote pin", local, err)
	}
	context.Backend = BackendEngine
	v := validator(t, context)
	result, err := v.Validate([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Placements) != 1 || result.Placements[0].Self {
		t.Fatalf("expected remote pin, got %+v", result.Placements)
	}
	result.Placements[0].Eligible[0].Name = "tampered"
	context.Config.Runners[1].Provides.Capabilities[0] = "tampered"
	second, err := v.Validate([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if second.Placements[0].Eligible[0].Name != "tools" {
		t.Fatal("placement result aliases pinned config")
	}
}

func TestEngineRefusesUnsupportedFeaturesBeforeProposalAcceptance(t *testing.T) {
	context := testContext()
	context.Backend = BackendEngine
	human := strings.Replace(validProposal, "next: \"\"", "next: approval", 1) + `  gates:
    - name: approval
      evaluator: human
      human: {}
      branches: {pass: "", fail: "@abort"}
`
	requireCode(t, validator(t, context), human, "backend")
	context.Config.Runners = []instance.RunnerEntry{
		{Name: "self", Host: "self", Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux}},
		{Name: "agents", Host: "ghcr.io/example/agents:v1", Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Harnesses: []string{"fake"}, Capabilities: []string{"special-tool"}}},
	}
	agentic := strings.Replace(validProposal, "type: deterministic", "type: agentic\n      goober: coder\n      capabilities: [agent:model]\n      limits: {maxTokens: 1000}\n      runsOn: {capabilities: [special-tool]}", 1)
	agentic = strings.Replace(agentic, "      run:\n        command: [\"true\"]\n", "", 1)
	requireCode(t, validator(t, context), agentic, "backend")
}

func TestNormalAdmissionRejectsMissingHarnessProviderAndPlacement(t *testing.T) {
	context := testContext()
	agentic := strings.Replace(validProposal, "type: deterministic", "type: agentic\n      goober: coder\n      capabilities: [agent:model]", 1)
	agentic = strings.Replace(agentic, "      run:\n        command: [\"true\"]\n", "", 1)
	context.KnownHarnesses = nil
	requireCode(t, validator(t, context), agentic, "compile")
	context = testContext()
	source := strings.Replace(validProposal, "start: check", "start: check\n  requires: {capabilities: [provider.nonexistent]}", 1)
	requireCode(t, validator(t, context), source, "provider")
	source = strings.Replace(validProposal, "type: deterministic", "type: deterministic\n      runsOn: {capabilities: [missing-tool]}", 1)
	requireCode(t, validator(t, context), source, "placement")
}

func TestPinnedPolicyAndEnclosingGrantAreBothRequired(t *testing.T) {
	context := testContext()
	context.ParentTask.ChildWorkflows = nil
	if _, err := NewValidator(context); err == nil {
		t.Fatal("accepted parent without opt-in")
	}
	context = testContext()
	context.GrantedCapabilities = nil
	source := strings.Replace(validProposal, "type: deterministic", "type: deterministic\n      capabilities: [repo:read]", 1)
	requireCode(t, validator(t, context), source, "capability")
	context = testContext()
	context.Goobers["coder"] = apiv1.GooberSpec{Gaggle: "other"}
	if _, err := NewValidator(context); err == nil {
		t.Fatal("accepted foreign Goober")
	}
}
