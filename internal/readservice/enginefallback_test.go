package readservice

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
)

func fallbackEvent(workflow string) journal.Event {
	return journal.Event{Schema: journal.EventSchema, Seq: 2, Time: fixedTime, Type: journal.EventRunnerAnnotation,
		Gaggle: "example", Workflow: workflow, RunID: "fallback-run",
		Runner: map[string]any{"kind": journal.RunnerAnnotationEngineSelection, "reason": "implement is self-pinned", "reasonClass": "placement_ineligible", "placementDeclared": true, "selfPinnedStages": []string{"implement"}},
	}
}

func TestEngineFallbackFoldBoundAndLifecycle(t *testing.T) {
	var fold engineFallbackFold
	for i := 0; i < maxEngineFallbackWorkflows+5; i++ {
		fold.apply(fallbackEvent(fmt.Sprintf("workflow-%d", i)))
	}
	if len(fold.items) != maxEngineFallbackWorkflows || len(fold.order) != maxEngineFallbackWorkflows {
		t.Fatal("unbounded fold")
	}
	if _, exists := fold.items[localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "workflow-0"}]; exists {
		t.Fatal("oldest workflow was not evicted")
	}
	clone := fold.clone()
	key := clone.order[0]
	copyValue := clone.items[key]
	copyValue.SelfPinnedStages[0] = "mutated"
	if fold.items[key].SelfPinnedStages[0] != "implement" {
		t.Fatal("snapshot aliases retained state")
	}
	for _, kind := range []journal.EventType{journal.EventConfigReloaded, journal.EventDaemonStarted} {
		fold.apply(journal.Event{Type: kind})
		if len(fold.items) != 0 || len(fold.order) != 0 {
			t.Fatal("stale decision survived lifecycle boundary")
		}
		fold.apply(fallbackEvent("new"))
	}
}

func TestEngineFallbackRunProjectionRoundTrip(t *testing.T) {
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	machine := fixtureMachine(t)
	run, clock := createFixtureRun(t, layout, machine, "fallback-run", machine.Def.Name, machine.Def.Spec.Gaggle, fixedTime, journal.Trigger{Kind: journal.TriggerManual}, false)
	event := fallbackEvent(machine.Def.Name)
	event.Gaggle = machine.Def.Spec.Gaggle
	if err := run.Append(event); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ProjectRunDir(ctx, filepath.Join(layout.RunsDir(), "fallback-run")); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions(), ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.EnableReadModelReads()
	projected, err := service.ListStatusRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service.DisableReadModelReads()
	authoritative, err := service.ListStatusRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || len(authoritative) != 1 {
		t.Fatalf("run counts: %d / %d", len(projected), len(authoritative))
	}
	got := projected[0].EngineFallback
	if got == nil || got.ReasonClass != "placement_ineligible" || got.RunID != "fallback-run" || !reflect.DeepEqual(got, authoritative[0].EngineFallback) {
		t.Fatalf("projection mismatch: %+v / %+v", got, authoritative[0].EngineFallback)
	}
	if category, visible := classifyRunEvent(event); category != RunEventDecision || !visible {
		t.Fatal("fallback remains hidden bookkeeping")
	}
}

func TestEngineFallbackWorkflowAPISurfacesAndReload(t *testing.T) {
	service, layout := newInventoryService(t, inventoryDefinitions(), nil)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	event := fallbackEvent("deploy")
	event.Gaggle = "alpha"
	if err := log.Append(event); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	(&schedulerStateProjector{service: service}).refresh(ctx)
	detail, err := service.Workflow(ctx, "alpha", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.Workflows(ctx, "alpha", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if detail.EngineFallback == nil || !detail.EngineFallback.PlacementDeclared {
		t.Fatalf("detail: %+v", detail)
	}
	found := false
	for _, item := range page.Items {
		if item.Identity.Name == "deploy" {
			found = reflect.DeepEqual(item.EngineFallback, detail.EngineFallback)
		}
	}
	if !found {
		t.Fatal("list/detail disagree")
	}
	if err := log.Append(journal.Event{Type: journal.EventConfigReloaded}); err != nil {
		t.Fatal(err)
	}
	(&schedulerStateProjector{service: service}).refresh(ctx)
	detail, err = service.Workflow(ctx, "alpha", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if detail.EngineFallback != nil {
		t.Fatal("stale eligibility survived reload")
	}
}
