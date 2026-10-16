package childworkflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fanOutProposal composes both DSL 3.1 fan-out constructs: a selective
// parallel whose optional lens runs only when the plan selects it, then a map
// over the plan's roster (plan-driven-dynamic-fan-out.md §11).
const fanOutProposal = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: generated-fan-out
  annotations: {goobers.dev/allow-preview-features: "true"}
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: plan
  tasks:
    - name: plan
      type: deterministic
      goal: Select the lenses and the shard roster
      run: {command: ["planner"], workspace: scratch}
      inputs: {artifactManifestFile: manifest.json}
      artifactSlots:
        - {name: lenses, mediaType: application/json}
        - {name: shards, mediaType: application/json}
      next: review
    - name: review-baseline
      type: deterministic
      goal: Always review the baseline
      run: {command: ["true"], workspace: scratch}
      next: "@join"
    - name: review-security
      type: deterministic
      goal: Review security only when selected
      run: {command: ["true"], workspace: scratch}
      next: "@join"
    - name: settle
      type: deterministic
      goal: Settle the review lenses
      run: {command: ["true"], workspace: scratch}
      next: run-shards
    - name: run-shard
      type: deterministic
      goal: Run one shard
      run: {command: ["true"], workspace: scratch}
      next: "@join"
    - name: collate
      type: deterministic
      goal: Collate every shard
      run: {command: ["true"], workspace: scratch}
  parallels:
    - name: review
      failurePolicy: continue_on_error
      join: settle
      selection: {source: {task: plan, slot: lenses}}
      branches:
        - {name: baseline, start: review-baseline}
        - {name: security, start: review-security, optional: true}
  maps:
    - name: run-shards
      source: {task: plan, slot: shards}
      task: run-shard
      bindings: {shard: item}
      failurePolicy: continue_on_error
      join: collate
`

func previewContext() AdmissionContext {
	context := testContext()
	context.AllowPreviewFeatures = true
	return context
}

func TestProposalAdmitsFanOutOnlyUnderParentPreviewPolicy(t *testing.T) {
	accepted, err := validator(t, previewContext()).Validate([]byte(fanOutProposal))
	if err != nil {
		t.Fatalf("preview-enabled parent refused a selective parallel and map: %v", err)
	}
	if _, ok := accepted.Machine.Parallel("review"); !ok || len(accepted.Workflow.Spec.Maps) != 1 || !accepted.Workflow.Spec.Parallels[0].Branches[1].Optional {
		t.Fatalf("compiled proposal lost a fan-out construct: %+v", accepted.Workflow.Spec)
	}

	// A parent without the preview acknowledgement admits neither construct:
	// the proposal cannot claim preview itself, and without the claim the
	// normal compiler preview gate refuses each feature.
	strict := validator(t, testContext())
	requireCode(t, strict, fanOutProposal, "preview")
	unacknowledged := strings.Replace(fanOutProposal, "  annotations: {goobers.dev/allow-preview-features: \"true\"}\n", "", 1)
	if unacknowledged == fanOutProposal {
		t.Fatal("fixture replacement did not apply")
	}
	requireCode(t, strict, unacknowledged, "compile")
	if _, err := strict.Validate([]byte(unacknowledged)); err == nil || !strings.Contains(err.Error(), "preview") {
		t.Fatalf("compile refusal does not name the preview gate: %v", err)
	}
}

func TestProposalRefusesChildToolsOnMappedTask(t *testing.T) {
	// A generated child can never opt into child tools, so the recursion
	// boundary refuses a mapped task's childWorkflows before compilation; the
	// compiler's own mapped-task rule (§10) stays behind it.
	mapped := strings.Replace(fanOutProposal, "    - name: run-shard\n      type: deterministic\n", "    - name: run-shard\n      type: deterministic\n      childWorkflows: {allowedGoobers: [coder]}\n", 1)
	if mapped == fanOutProposal {
		t.Fatal("fixture replacement did not apply")
	}
	requireCode(t, validator(t, previewContext()), mapped, "recursion")
}

func TestProposalStateBoundCountsMaps(t *testing.T) {
	v := validator(t, previewContext())
	good, err := v.Validate([]byte(fanOutProposal))
	if err != nil {
		t.Fatal(err)
	}
	wf := good.Workflow
	if got := proposalStates(wf.Spec); got != len(wf.Spec.Tasks)+2 {
		t.Fatalf("proposalStates = %d, want tasks plus one parallel and one map", got)
	}
	// Pad until tasks, gates and parallels alone sit exactly at the bound, so
	// only the map's count pushes this proposal over it.
	for i := 0; len(wf.Spec.Tasks)+len(wf.Spec.Gates)+len(wf.Spec.Parallels) < MaxProposalStates; i++ {
		task := wf.Spec.Tasks[1]
		task.Name = fmt.Sprintf("pad-%d", i)
		wf.Spec.Tasks = append(wf.Spec.Tasks, task)
	}
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	requireCode(t, v, string(raw), "states")
}

func TestEngineBackendRefusesFanOutProposal(t *testing.T) {
	context := previewContext()
	context.Backend = BackendEngine
	requireCode(t, validator(t, context), fanOutProposal, "backend")
}
