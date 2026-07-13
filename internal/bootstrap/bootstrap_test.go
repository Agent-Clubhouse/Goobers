package bootstrap

import (
	"context"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/scheduler"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

const fixtureRoot = "../../test/fixtures/e2e/walking-skeleton"

// fakeStarter records started run ids and reports duplicates as already-running.
type fakeStarter struct {
	mu      sync.Mutex
	started map[string]int
	last    engine.RunInput
}

func (f *fakeStarter) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started == nil {
		f.started = map[string]int{}
	}
	f.last = in
	if f.started[in.RunID] > 0 {
		f.started[in.RunID]++
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true}, nil
	}
	f.started[in.RunID] = 1
	return engine.StartResult{RunID: in.RunID}, nil
}

func TestLoadAndRegisterFixture(t *testing.T) {
	loaded, err := LoadAndRegister(fixtureRoot, "")
	if err != nil {
		t.Fatalf("LoadAndRegister: %v", err)
	}
	if loaded.Manifest == nil {
		t.Fatal("expected a manifest")
	}
	if len(loaded.Gaggles) == 0 || len(loaded.Workflows) == 0 {
		t.Fatalf("expected gaggles + workflows, got %d gaggles, %d workflows", len(loaded.Gaggles), len(loaded.Workflows))
	}
	// Every workflow is registered and resolvable.
	for _, w := range loaded.Workflows {
		def, ok := loaded.Registry.Latest(w.Name)
		if !ok {
			t.Errorf("workflow %q was not registered", w.Name)
			continue
		}
		if _, err := wf.Compile(def); err != nil {
			t.Errorf("registered workflow %q does not compile: %v", w.Name, err)
		}
	}
}

func TestSchedulerForWiresConfigToStart(t *testing.T) {
	loaded, err := LoadAndRegister(fixtureRoot, "")
	if err != nil {
		t.Fatalf("LoadAndRegister: %v", err)
	}
	gaggle := loaded.Gaggles[0]
	workflow := loaded.Workflows[0]

	st := &fakeStarter{}
	sched, err := loaded.SchedulerFor(gaggle.Name, SchedulerDeps{Starter: st})
	if err != nil {
		t.Fatalf("SchedulerFor: %v", err)
	}

	item := providers.WorkItem{Provider: providers.ProviderGitHub, ID: "101", Title: "smoke"}
	d, err := sched.Dispatch(context.Background(), scheduler.Event{
		WorkflowName: workflow.Name,
		Item:         &item,
		DedupeKey:    "github:101",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !d.Started {
		t.Fatalf("expected the wired path to start a run, got %+v", d)
	}
	// The run carried the gaggle's project repo and a pinned definition.
	if st.last.RepoRef.Name != gaggle.Spec.Project.Name {
		t.Errorf("run repo = %q, want the gaggle project %q", st.last.RepoRef.Name, gaggle.Spec.Project.Name)
	}
	if st.last.Version != 1 || st.last.WorkflowName != workflow.Name {
		t.Errorf("run input = %+v, want pinned %q v1", st.last, workflow.Name)
	}
	if st.last.Item == nil || st.last.Item.ID != "101" {
		t.Errorf("run input missing backlog item: %+v", st.last.Item)
	}
}

func TestSchedulerForUnknownGaggle(t *testing.T) {
	loaded, err := LoadAndRegister(fixtureRoot, "")
	if err != nil {
		t.Fatalf("LoadAndRegister: %v", err)
	}
	if _, err := loaded.SchedulerFor("ghost", SchedulerDeps{Starter: &fakeStarter{}}); err == nil {
		t.Error("expected an error for an unknown gaggle")
	}
}

func TestLoadAndRegisterBadDirErrors(t *testing.T) {
	if _, err := LoadAndRegister("does-not-exist", ""); err == nil {
		t.Error("expected an error for a missing config dir")
	}
}
