package localscheduler

import (
	"errors"
	"testing"
	"time"
)

// TestSecondDaemonTripsStateOwnershipError is the M5 guard acceptance test:
// two schedulers (two would-be daemons) writing the same instance root's
// guarded state files must not silently interleave — the superseded one gets
// the named ErrStateSeized, for both trigger-evaluations.json and
// schedule-demand.json.
func TestSecondDaemonTripsStateOwnershipError(t *testing.T) {
	dir := t.TempDir()
	identity := WorkflowIdentity{Gaggle: "g", Workflow: "w"}
	evaluations := map[WorkflowIdentity]time.Time{identity: time.Now().UTC()}
	demand := map[WorkflowIdentity]bool{identity: true}

	daemonA := newStateOwner()
	daemonB := newStateOwner()

	// Daemon A claims both files on first write.
	if err := writeTriggerEvaluations(dir, daemonA, evaluations); err != nil {
		t.Fatal(err)
	}
	if err := writeScheduleDemandState(dir, daemonA, demand); err != nil {
		t.Fatal(err)
	}
	// A's own subsequent writes stay valid.
	if err := writeTriggerEvaluations(dir, daemonA, evaluations); err != nil {
		t.Fatal(err)
	}

	// Daemon B claims ownership (its first write supersedes A's generation).
	if err := writeTriggerEvaluations(dir, daemonB, evaluations); err != nil {
		t.Fatal(err)
	}
	if err := writeScheduleDemandState(dir, daemonB, demand); err != nil {
		t.Fatal(err)
	}

	// A's next writes trip the named error rather than clobbering B's state.
	err := writeTriggerEvaluations(dir, daemonA, evaluations)
	if !errors.Is(err, ErrStateSeized) {
		t.Fatalf("trigger evaluations write after seizure: err = %v, want ErrStateSeized", err)
	}
	var ownership *StateOwnershipError
	if !errors.As(err, &ownership) {
		t.Fatalf("err = %v, want *StateOwnershipError", err)
	}
	if ownership.File != triggerEvaluationsFileName {
		t.Fatalf("ownership error names %q, want %q", ownership.File, triggerEvaluationsFileName)
	}
	if err := writeScheduleDemandState(dir, daemonA, demand); !errors.Is(err, ErrStateSeized) {
		t.Fatalf("schedule demand write after seizure: err = %v, want ErrStateSeized", err)
	}

	// The files remain readable, stamped or not (readers ignore the stamp).
	if _, err := ReadTriggerEvaluations(dir); err != nil {
		t.Fatalf("ReadTriggerEvaluations after guarded writes: %v", err)
	}
	if _, err := readScheduleDemandState(dir); err != nil {
		t.Fatalf("readScheduleDemandState after guarded writes: %v", err)
	}
}

// TestStateOwnerClaimsLegacyUnstampedFiles pins the upgrade path: a state
// file written before the M5 guard existed (no stamp) is claimable by the
// first guarded writer without error.
func TestStateOwnerClaimsLegacyUnstampedFiles(t *testing.T) {
	dir := t.TempDir()
	identity := WorkflowIdentity{Gaggle: "g", Workflow: "w"}
	evaluations := map[WorkflowIdentity]time.Time{identity: time.Now().UTC()}

	// A guard-less writer (the pre-M5 shape) leaves no stamp.
	if err := writeTriggerEvaluations(dir, nil, evaluations); err != nil {
		t.Fatal(err)
	}
	owner := newStateOwner()
	if err := writeTriggerEvaluations(dir, owner, evaluations); err != nil {
		t.Fatalf("claiming a legacy unstamped file: %v", err)
	}
	if err := writeTriggerEvaluations(dir, owner, evaluations); err != nil {
		t.Fatalf("second write by the claiming owner: %v", err)
	}
}
