// Package childscenarios defines the child-family nightly HA assertions. The
// kind harness supplies the fixture operations; this package owns the
// no-silent-pass evidence checks.
package childscenarios

import (
	"context"
	"errors"
	"fmt"
)

// Chaos13Child identifies the generated-child worker-eviction scenario: the
// child-family variant of CHAOS-13.
const Chaos13Child = "CHAOS-13-CHILD"

// ChildFamilyObservation is durable custody observed around worker eviction
// while a generated child is mid side-effecting stage.
type ChildFamilyObservation struct {
	ParentRunID string
	ChildID     string
	// ChildTemporalRunID is the child workflow execution's run ID; a new one
	// means the sealed child was restarted rather than adopted.
	ChildTemporalRunID string
	Attempt            int
	// StagePodUID is the stage pod's Kubernetes UID. Pod names can be
	// deterministic, so only the UID tells adoption from recreation.
	StagePodUID string
	// OwnerWorker identifies the worker pod (by UID) that owns the child's
	// execution; eviction must move ownership to a different worker.
	OwnerWorker     string
	Adopted         bool
	ChargedAttempts int
	// AdmittedChildren counts accepted lineage rows for the parent stage
	// occurrence; anything but one is a duplicate admission.
	AdmittedChildren int
	ChildTerminal    bool
	ParentTerminal   bool
}

// Fixture is implemented by the hosted kind harness. Every operation must
// return durable observations, not only successful command exit statuses.
type Fixture interface {
	StartChildMidStage(context.Context) (ChildFamilyObservation, error)
	EvictOwningWorker(context.Context, ChildFamilyObservation) error
	WaitForChildFamily(context.Context, string) (ChildFamilyObservation, error)
}

// RunCHAOS13Child proves worker eviction adopts a generated child's original
// execution and stage pod: no restart, no second admission, no extra charge.
func RunCHAOS13Child(ctx context.Context, fixture Fixture) error {
	if fixture == nil {
		return errors.New("CHAOS-13-CHILD: fixture is required")
	}
	before, err := fixture.StartChildMidStage(ctx)
	if err != nil {
		return err
	}
	if before.ParentRunID == "" || before.ChildID == "" ||
		before.ChildTemporalRunID == "" || before.Attempt < 1 ||
		before.StagePodUID == "" || before.OwnerWorker == "" ||
		before.ChargedAttempts < 1 || before.AdmittedChildren != 1 {
		return fmt.Errorf("CHAOS-13-CHILD: incomplete initial observation: %+v", before)
	}
	if err := fixture.EvictOwningWorker(ctx, before); err != nil {
		return err
	}
	after, err := fixture.WaitForChildFamily(ctx, before.ParentRunID)
	if err != nil {
		return err
	}
	return compare(before, after)
}

func compare(before, after ChildFamilyObservation) error {
	switch {
	case after.ChildID != before.ChildID:
		return fmt.Errorf("CHAOS-13-CHILD: child %q replaced %q", after.ChildID, before.ChildID)
	case after.AdmittedChildren != 1:
		return fmt.Errorf("CHAOS-13-CHILD: %d children admitted, want 1", after.AdmittedChildren)
	case after.ChildTemporalRunID != before.ChildTemporalRunID:
		return fmt.Errorf("CHAOS-13-CHILD: child restarted as run %q (was %q)",
			after.ChildTemporalRunID, before.ChildTemporalRunID)
	case after.OwnerWorker == "" || after.OwnerWorker == before.OwnerWorker:
		return fmt.Errorf("CHAOS-13-CHILD: owner worker %q did not change from %q; eviction was a no-op",
			after.OwnerWorker, before.OwnerWorker)
	case !after.Adopted:
		return fmt.Errorf("CHAOS-13-CHILD: child stage was not adopted: %+v", after)
	case after.StagePodUID != before.StagePodUID:
		return fmt.Errorf("CHAOS-13-CHILD: stage pod %q replaced %q",
			after.StagePodUID, before.StagePodUID)
	case after.Attempt != before.Attempt:
		return fmt.Errorf("CHAOS-13-CHILD: attempt changed %d -> %d", before.Attempt, after.Attempt)
	case after.ChargedAttempts != before.ChargedAttempts:
		return fmt.Errorf("CHAOS-13-CHILD: attempt charge changed %d -> %d",
			before.ChargedAttempts, after.ChargedAttempts)
	case !after.ChildTerminal || !after.ParentTerminal:
		return fmt.Errorf("CHAOS-13-CHILD: child family did not settle: %+v", after)
	default:
		return nil
	}
}
