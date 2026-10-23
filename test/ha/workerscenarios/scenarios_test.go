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
			ChargedAttempts: 2,
		},
		sideAfter: SideEffectObservation{
			RunID: "run", Attempt: 2, StagePod: "stage-pod",
			Adopted: true, ChargedAttempts: 2, Terminal: true,
		},
	}
	if err := RunCHAOS13(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	if !fixture.evicted {
		t.Fatal("worker eviction was not injected")
	}
	fixture.sideAfter.StagePod = "second-pod"
	if err := RunCHAOS13(t.Context(), fixture); err == nil {
		t.Fatal("second pod passed CHAOS-13")
	}
	fixture.sideBefore.StagePod = ""
	if err := RunCHAOS13(t.Context(), fixture); err == nil {
		t.Fatal("incomplete fixture evidence passed CHAOS-13")
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
