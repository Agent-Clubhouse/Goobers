package localscheduler

import (
	"errors"
	"os"
	"path/filepath"
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

// TestStateOwnerSurvivesTransientFirstWriteFailure is the claim-before-write
// regression: stamp() must not commit the claimed generation until the
// guarded write lands. A transient first-write failure (here: an unwritable
// scheduler directory, standing in for ENOSPC/EIO) followed by a successful
// retry must not poison later writes with a false ErrStateSeized.
func TestStateOwnerSurvivesTransientFirstWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("write-failure induction via directory permissions does not work as root")
	}
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	identity := WorkflowIdentity{Gaggle: "g", Workflow: "w"}
	evaluations := map[WorkflowIdentity]time.Time{identity: time.Now().UTC()}
	owner := newStateOwner()

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := writeTriggerEvaluations(dir, owner, evaluations); err == nil {
		t.Fatal("write into an unwritable scheduler directory must fail")
	} else if errors.Is(err, ErrStateSeized) {
		t.Fatalf("transient write failure surfaced as ErrStateSeized: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writeTriggerEvaluations(dir, owner, evaluations); err != nil {
		t.Fatalf("retry after a transient write failure: %v", err)
	}
	if err := writeTriggerEvaluations(dir, owner, evaluations); err != nil {
		t.Fatalf("write after a recovered transient failure: %v", err)
	}
}

// TestStateOwnerReclaimsDeletedStateFile pins the operator-reset path: a
// guarded state file deleted mid-run carries the zero stamp, which the
// current owner reclaims on its next write instead of tripping ErrStateSeized
// until restart.
func TestStateOwnerReclaimsDeletedStateFile(t *testing.T) {
	dir := t.TempDir()
	identity := WorkflowIdentity{Gaggle: "g", Workflow: "w"}
	evaluations := map[WorkflowIdentity]time.Time{identity: time.Now().UTC()}
	demand := map[WorkflowIdentity]bool{identity: true}
	owner := newStateOwner()

	if err := writeTriggerEvaluations(dir, owner, evaluations); err != nil {
		t.Fatal(err)
	}
	if err := writeScheduleDemandState(dir, owner, demand); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, triggerEvaluationsFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, scheduleDemandStateFileName)); err != nil {
		t.Fatal(err)
	}

	if err := writeTriggerEvaluations(dir, owner, evaluations); err != nil {
		t.Fatalf("write after the state file was deleted: %v", err)
	}
	if err := writeScheduleDemandState(dir, owner, demand); err != nil {
		t.Fatalf("schedule-demand write after deletion: %v", err)
	}
	if err := writeTriggerEvaluations(dir, owner, evaluations); err != nil {
		t.Fatalf("write after reclaiming the deleted file: %v", err)
	}
}

// TestStateOwnerRestartClaimsExistingFile pins the single-daemon restart: a
// fresh stateOwner (a restarted daemon) claims the previous generation's
// files without error.
func TestStateOwnerRestartClaimsExistingFile(t *testing.T) {
	dir := t.TempDir()
	identity := WorkflowIdentity{Gaggle: "g", Workflow: "w"}
	evaluations := map[WorkflowIdentity]time.Time{identity: time.Now().UTC()}

	before := newStateOwner()
	if err := writeTriggerEvaluations(dir, before, evaluations); err != nil {
		t.Fatal(err)
	}
	restarted := newStateOwner()
	if err := writeTriggerEvaluations(dir, restarted, evaluations); err != nil {
		t.Fatalf("first write after restart: %v", err)
	}
	if err := writeTriggerEvaluations(dir, restarted, evaluations); err != nil {
		t.Fatalf("second write after restart: %v", err)
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
