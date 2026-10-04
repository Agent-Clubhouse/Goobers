package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/runner"
)

func TestPRRepairAdmissionIsBoundedAndExcludesRecovery(t *testing.T) {
	registry := newDaemonRunnerRegistry()
	owner := &runner.Runner{}
	untrack := registry.Track("existing", "workflow", owner)
	release, owners, err := registry.acquirePRRepairAdmission(t.Context())
	if err != nil || len(owners) != 1 || owners[0].owner != owner {
		t.Fatal(owners, err)
	}
	defer release()
	// Owner completion remains possible while the barrier is held.
	untrack()
	if len(registry.ActiveRuns()) != 0 {
		t.Fatal("owner could not complete")
	}
	if relinquish, ok := registry.acquireChildCustody("recovery"); ok {
		relinquish()
		t.Fatal("recovery entered repair custody")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Millisecond)
	defer cancel()
	if _, _, err := registry.acquirePRRepairAdmission(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if relinquish, ok := registry.TrackCompatible("new", owner); ok {
		relinquish()
		t.Fatal("new owner entered repair custody")
	}
	release()
	recovery, ok := registry.acquireChildCustody("recovery")
	if !ok {
		t.Fatal("recovery did not resume")
	}
	defer recovery()
	if _, _, err := registry.acquirePRRepairAdmission(t.Context()); err == nil {
		t.Fatal("repair overlapped recovery")
	}
	recovery()
	again, _, err := registry.acquirePRRepairAdmission(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	again()
}
