package childscenarios

import (
	"context"
	"errors"
	"testing"
)

type fakeFixture struct {
	before  ChildFamilyObservation
	after   ChildFamilyObservation
	evicted bool
	waited  string
}

func (f *fakeFixture) StartChildMidStage(context.Context) (ChildFamilyObservation, error) {
	return f.before, nil
}
func (f *fakeFixture) EvictOwningWorker(context.Context, ChildFamilyObservation) error {
	f.evicted = true
	return nil
}
func (f *fakeFixture) WaitForChildFamily(_ context.Context, parent string) (ChildFamilyObservation, error) {
	f.waited = parent
	return f.after, nil
}

func settled() *fakeFixture {
	before := ChildFamilyObservation{
		ParentRunID: "parent", ChildID: "child", ChildTemporalRunID: "child-run",
		Attempt: 1, StagePod: "stage-pod", ChargedAttempts: 1, AdmittedChildren: 1,
	}
	after := before
	after.Adopted, after.ChildTerminal, after.ParentTerminal = true, true, true
	return &fakeFixture{before: before, after: after}
}

func TestCHAOS13ChildContract(t *testing.T) {
	if err := RunCHAOS13Child(t.Context(), nil); err == nil {
		t.Fatal("missing fixture passed CHAOS-13-CHILD")
	}
	fixture := settled()
	if err := RunCHAOS13Child(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	if !fixture.evicted {
		t.Fatal("worker eviction was not injected")
	}
	if fixture.waited != "parent" {
		t.Fatalf("waited on %q, want the parent run", fixture.waited)
	}
}

func TestCHAOS13ChildRejectsIncompleteInitialEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*ChildFamilyObservation){
		"no parent":         func(o *ChildFamilyObservation) { o.ParentRunID = "" },
		"no child":          func(o *ChildFamilyObservation) { o.ChildID = "" },
		"no child run":      func(o *ChildFamilyObservation) { o.ChildTemporalRunID = "" },
		"no attempt":        func(o *ChildFamilyObservation) { o.Attempt = 0 },
		"no stage pod":      func(o *ChildFamilyObservation) { o.StagePod = "" },
		"not yet admitted":  func(o *ChildFamilyObservation) { o.AdmittedChildren = 0 },
		"already duplicate": func(o *ChildFamilyObservation) { o.AdmittedChildren = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := settled()
			mutate(&fixture.before)
			if err := RunCHAOS13Child(t.Context(), fixture); err == nil {
				t.Fatal("incomplete evidence passed CHAOS-13-CHILD")
			}
			if fixture.evicted {
				t.Fatal("worker was evicted on incomplete evidence")
			}
		})
	}
}

func TestCHAOS13ChildRejectsEveryRecoveryRegression(t *testing.T) {
	for name, mutate := range map[string]func(*ChildFamilyObservation){
		"replaced child":     func(o *ChildFamilyObservation) { o.ChildID = "other" },
		"duplicate admitted": func(o *ChildFamilyObservation) { o.AdmittedChildren = 2 },
		"child restarted":    func(o *ChildFamilyObservation) { o.ChildTemporalRunID = "second-run" },
		"not adopted":        func(o *ChildFamilyObservation) { o.Adopted = false },
		"second stage pod":   func(o *ChildFamilyObservation) { o.StagePod = "second-pod" },
		"attempt changed":    func(o *ChildFamilyObservation) { o.Attempt = 2 },
		"charge changed":     func(o *ChildFamilyObservation) { o.ChargedAttempts = 2 },
		"child unsettled":    func(o *ChildFamilyObservation) { o.ChildTerminal = false },
		"parent unsettled":   func(o *ChildFamilyObservation) { o.ParentTerminal = false },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := settled()
			mutate(&fixture.after)
			if err := RunCHAOS13Child(t.Context(), fixture); err == nil {
				t.Fatal("regression passed CHAOS-13-CHILD")
			}
		})
	}
}

type failingFixture struct {
	fakeFixture
	start, evict, wait error
}

func (f *failingFixture) StartChildMidStage(ctx context.Context) (ChildFamilyObservation, error) {
	obs, _ := f.fakeFixture.StartChildMidStage(ctx)
	return obs, f.start
}
func (f *failingFixture) EvictOwningWorker(ctx context.Context, obs ChildFamilyObservation) error {
	_ = f.fakeFixture.EvictOwningWorker(ctx, obs)
	return f.evict
}
func (f *failingFixture) WaitForChildFamily(ctx context.Context, parent string) (ChildFamilyObservation, error) {
	obs, _ := f.fakeFixture.WaitForChildFamily(ctx, parent)
	return obs, f.wait
}

func TestCHAOS13ChildPropagatesFixtureErrors(t *testing.T) {
	boom := errors.New("fixture failed")
	for name, fixture := range map[string]*failingFixture{
		"start": {fakeFixture: *settled(), start: boom},
		"evict": {fakeFixture: *settled(), evict: boom},
		"wait":  {fakeFixture: *settled(), wait: boom},
	} {
		t.Run(name, func(t *testing.T) {
			if err := RunCHAOS13Child(t.Context(), fixture); !errors.Is(err, boom) {
				t.Fatalf("error = %v, want the fixture failure", err)
			}
		})
	}
}
