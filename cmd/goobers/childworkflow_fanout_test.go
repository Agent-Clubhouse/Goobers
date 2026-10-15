package main

import (
	"os"
	"strings"
	"testing"
)

// childFanOutProposal uses both DSL 3.1 fan-out constructs: a selective
// parallel and a map (plan-driven-dynamic-fan-out.md §11).
const childFanOutProposal = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: generated-fan-out
  annotations: {goobers.dev/allow-preview-features: "true"}
spec:
  gaggle: example
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

func TestWorkflowValidateChildAdmitsFanOutOnLocalRunnerOnly(t *testing.T) {
	root, _, proposal := childValidationFixture(t)
	if err := os.WriteFile(proposal, []byte(childFanOutProposal), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, validateChildArgs(root, proposal)...)
	if code != 0 || !strings.Contains(stdout, `"valid": true`) {
		t.Fatalf("preview parent refused a selective parallel and map: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}

	// A child-capable parent is necessarily preview-acknowledged today
	// (childWorkflows and DSL 3.1 are themselves preview), so the
	// parent-without-preview refusal is pinned at the validator level in
	// internal/childworkflow. Here the Temporal engine backend, which executes
	// neither construct, refuses the same proposal before acceptance.
	args := validateChildArgs(root, proposal)
	args = append(args[:len(args)-2], append([]string{"--backend", "engine"}, args[len(args)-2:]...)...)
	code, stdout, _ = runArgs(t, args...)
	if code != 1 || !strings.Contains(stdout, `"code": "backend"`) {
		t.Fatalf("engine backend admitted fan-out child: code=%d %s", code, stdout)
	}
}
