package main

import (
	"maps"
	"testing"
)

// Frozen qualification candidates must not depend on the moving main ref's
// cancellable CI run. Dispatch uses the same matrix on a dedicated branch;
// no separate, weaker validation workflow or arbitrary checkout input exists.
func TestCIManualDispatchPreservesFullGate(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	dispatch, ok := w.On["workflow_dispatch"]
	if !ok || len(dispatch.Content) != 0 {
		t.Fatal("CI must support manual dispatch without checkout/skip inputs")
	}
	// A title/body-edit run alone gets a run-unique group (#6360); dispatch
	// renders the shared ref group (TestCIRunsOnBaseRetargetNotMetadataEdits).
	if w.Concurrency.Group != "ci-${{ github.workflow }}-${{ github.ref }}${{ "+ciMetadataEdit+" && format('-metadata-edit-{0}', github.run_id) || '' }}" || !w.Concurrency.CancelInProgress {
		t.Fatal("CI concurrency must isolate frozen refs while preserving same-ref cancellation")
	}
	if !maps.Equal(w.Permissions, map[string]string{"actions": "read", "contents": "read", "pull-requests": "read"}) {
		t.Fatal("manual CI must retain read-only default permissions")
	}
	aggregate := w.Jobs["required-ci"]
	if aggregate.If != "${{ always() && github.event_name != 'push' && !"+ciMetadataEdit+" }}" || aggregate.ContinueOnError || len(aggregate.Needs) == 0 {
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
		if ref := checkout.with("ref"); ref != "" && ref != "${{ github.sha }}" {
			t.Errorf("required job %s must check out the dispatched event SHA, not %q", id, ref)
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
