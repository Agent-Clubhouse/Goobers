package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const workflowMapFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: default-implement
  annotations: {goobers.dev/allow-preview-features: "true"}
spec:
  gaggle: example
  triggers:
    - type: manual
  start: plan
  tasks:
    - name: plan
      type: deterministic
      goal: Select the shard roster.
      run: {command: ["true"], workspace: scratch}
      inputs: {artifactManifestFile: manifest.json}
      artifactSlots:
        - {name: shards, mediaType: application/json}
      next: run-shards
    - name: run-shard
      type: deterministic
      goal: Run one shard.
      run: {command: ["true"], workspace: scratch}
      next: "@join"
    - name: collate
      type: deterministic
      goal: Collate every shard.
      run: {command: ["true"], workspace: scratch}
  maps:
    - name: run-shards
      source: {task: plan, slot: shards}
      task: run-shard
      failurePolicy: fail_fast
      onFailure: "@abort"
      maxConcurrent: 4
      join: collate
`

// TestWorkflowShowTextRendersMaps is TestWorkflowShowTextRendersParallels for
// a DSL 3.1 map: the text view lists the map with its concurrency, its
// per-item task and failure route, and resolves the mapped task's "@join".
func TestWorkflowShowTextRendersMaps(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowMapFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "workflow", "show", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"  plan (kind: deterministic) -> run-shards\n",
		"  run-shard (kind: deterministic) -> collate\n",
		"  run-shards (kind: map, maxConcurrent: 4)\n    per-item task: run-shard\n    branch-failed target: @abort\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("workflow show stdout missing %q:\n%s", want, stdout)
		}
	}
	if got := mapConcurrency(nil, "absent"); got != "maxConcurrent: unknown" {
		t.Fatalf("mapConcurrency(absent) = %q", got)
	}
}
