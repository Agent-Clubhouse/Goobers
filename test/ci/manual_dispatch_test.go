package main

import (
	"maps"
	"testing"
)

// Frozen qualification candidates must not depend on the moving main ref's
// cancellable CI run. Dispatch uses the same matrix on a dedicated branch;
// no separate, weaker validation workflow exists, and the only checkout input
// is a retarget merge that scope verifies.
func TestCIManualDispatchPreservesFullGate(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	// The only inputs are ci-retarget.yml's optional merge inputs (#7017);
	// left empty, a dispatch validates the dispatched commit.
	assertRetargetDispatchInputs(t, w)
	if w.Concurrency.Group != "ci-${{ github.workflow }}-${{ inputs.pr_number && format('refs/pull/{0}/merge', inputs.pr_number) || github.ref }}" || !w.Concurrency.CancelInProgress {
		t.Fatal("CI concurrency must isolate frozen refs while preserving same-ref cancellation")
	}
	if !maps.Equal(w.Permissions, map[string]string{"actions": "read", "contents": "read", "pull-requests": "read"}) {
		t.Fatal("manual CI must retain read-only default permissions")
	}
	aggregate := w.Jobs["required-ci"]
	if aggregate.If != "${{ always() && github.event_name != 'push' }}" || aggregate.ContinueOnError || len(aggregate.Needs) == 0 {
		t.Fatal("manual CI must require the full fail-closed aggregate")
	}
	ctx := map[string]any{"github": map[string]any{"event_name": "workflow_dispatch", "event": map[string]any{}}}
	ran := simulateCIJobs(t, w, ctx)
	for _, id := range aggregate.Needs {
		job, exists := w.Jobs[id]
		if !exists {
			t.Fatalf("required job %s is undefined", id)
		}
		if !ran[id] {
			t.Errorf("required job %s has a condition not guaranteed to run on dispatch: %s", id, job.If)
		}
		if job.ContinueOnError || len(job.Permissions) != 0 {
			t.Errorf("required job %s must remain fail-closed with inherited read-only permissions", id)
		}
		checkout := job.stepUsing(t, "actions/checkout@")
		if ref := checkout.with("ref"); ref != "${{ inputs.merge_sha }}" || renderTemplate(t, ref, ctx) != "" {
			t.Errorf("required job %s must check out the dispatched event SHA unless a retarget merge is given, not %q", id, ref)
		}
		if checkout.If != "" || checkout.ContinueOnError || checkout.with("repository") != "" {
			t.Errorf("required job %s must unconditionally check out this repository", id)
		}
	}
	gate := aggregate.step(t, "Verify required gates")
	if gate.If != "" || gate.ContinueOnError || gate.Run != "go run ./test/cipolicy gate" {
		t.Fatal("manual aggregate must reject missing, skipped, cancelled, and failed gates")
	}
}
