package workerscenarios

import (
	"context"
	"testing"
)

type fakeFixture struct {
	sideBefore SideEffectObservation
	sideAfter  SideEffectObservation
	hold       HITLObservation
	resolved   HITLObservation
	evicted    bool
	rolled     string
	resolves   int
}

func (f *fakeFixture) StartSideEffectingStage(context.Context) (SideEffectObservation, error) {
	return f.sideBefore, nil
}
func (f *fakeFixture) EvictOwningWorker(context.Context, SideEffectObservation) error {
	f.evicted = true
	return nil
}
func (f *fakeFixture) WaitForSideEffectingStage(context.Context, string) (SideEffectObservation, error) {
	return f.sideAfter, nil
}
func (f *fakeFixture) StartHITLHold(context.Context) (HITLObservation, error) {
	return f.hold, nil
}
func (f *fakeFixture) RolloutWorkerBuild(_ context.Context, build string) error {
	f.rolled = build
	return nil
}
func (f *fakeFixture) ResolveHITL(context.Context, HITLObservation) (HITLObservation, error) {
	f.resolves++
	return f.resolved, nil
}

func TestCHAOS13Contract(t *testing.T) {
	if err := RunCHAOS13(t.Context(), nil); err == nil {
		t.Fatal("missing fixture passed CHAOS-13")
	}
	fixture := &fakeFixture{
		sideBefore: SideEffectObservation{
			RunID: "run", Attempt: 2, StagePod: "stage-pod",
			StagePodUID: "uid-1", OwnerWorker: "worker-a", ChargedAttempts: 2,
		},
		sideAfter: SideEffectObservation{
			RunID: "run", Attempt: 2, StagePod: "stage-pod",
			StagePodUID: "uid-1", OwnerWorker: "worker-b",
			Adopted: true, ChargedAttempts: 2, Terminal: true,
		},
	}
	if err := RunCHAOS13(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	if !fixture.evicted {
		t.Fatal("worker eviction was not injected")
	}
	fixture.sideAfter.StagePod = "renamed-observation"
	if err := RunCHAOS13(t.Context(), fixture); err != nil {
		t.Fatalf("pod name changed with stable UID: %v", err)
	}
	fixture.sideAfter.StagePodUID = "uid-2"
	if err := RunCHAOS13(t.Context(), fixture); err == nil {
		t.Fatal("replacement pod UID passed CHAOS-13")
	}
	fixture.sideAfter.StagePodUID = fixture.sideBefore.StagePodUID
	fixture.sideAfter.OwnerWorker = fixture.sideBefore.OwnerWorker
	if err := RunCHAOS13(t.Context(), fixture); err == nil {
		t.Fatal("unchanged owner worker passed CHAOS-13")
	}
	fixture.sideAfter.OwnerWorker = "worker-b"
	fixture.sideBefore.ChargedAttempts = 0
	if err := RunCHAOS13(t.Context(), fixture); err == nil {
		t.Fatal("zero initial charged attempts passed CHAOS-13")
	}
	fixture.sideBefore.ChargedAttempts = 2
	fixture.sideBefore.StagePodUID = ""
	if err := RunCHAOS13(t.Context(), fixture); err == nil {
		t.Fatal("missing pod UID passed CHAOS-13")
	}
}

func TestUPGRADE11Contract(t *testing.T) {
	if err := RunUPGRADE11(t.Context(), nil, "build-n1"); err == nil {
		t.Fatal("missing fixture passed UPGRADE-11")
	}
	fixture := &fakeFixture{
		hold: HITLObservation{
			RunID: "run", HoldID: "hold", WorkerBuild: "build-n",
		},
		resolved: HITLObservation{
			RunID: "run", HoldID: "hold", WorkerBuild: "build-n1",
			AppliedResolutions: 1, Terminal: true,
		},
	}
	if err := RunUPGRADE11(
		t.Context(), fixture, "build-n1"); err != nil {
		t.Fatal(err)
	}
	if fixture.rolled != "build-n1" || fixture.resolves != 2 {
		t.Fatalf("rollout=%q resolves=%d", fixture.rolled, fixture.resolves)
	}
	fixture.resolved.AppliedResolutions = 2
	if err := RunUPGRADE11(
		t.Context(), fixture, "build-n1"); err == nil {
		t.Fatal("duplicate resolution passed UPGRADE-11")
	}
	fixture.hold.HoldID = ""
	if err := RunUPGRADE11(
		t.Context(), fixture, "build-n1"); err == nil {
		t.Fatal("incomplete fixture evidence passed UPGRADE-11")
	}
}
