package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestEngineFallbackClassifiesDeclarationsAndEmitsTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		declared, self, pinned bool
		class                  string
	}{
		{"never migrated", false, false, false, "no_pinned_placements"},
		{"legacy implicit self", false, true, true, "placement_ineligible"},
		{"regressed", true, true, true, "placement_ineligible"},
		{"local inventory with declarations", true, false, false, "no_pinned_placements"},
		{"migrated", true, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := selectionDefinition(t)
			if tc.declared {
				def.Spec.Tasks[0].RunsOn = &apiv1.RunsOn{}
			}
			var pins []engine.PinnedPlacement
			if tc.pinned {
				pins = []engine.PinnedPlacement{{Stage: "implement", Self: tc.self, Queue: "remote"}}
			}
			selection := selectEngineForEntry(def, pins)
			if selection.ReasonClass != tc.class || selection.PlacementDeclared != tc.declared {
				t.Fatalf("selection = %+v", selection)
			}
			exporter := tracetest.NewInMemoryExporter()
			client, err := telemetry.New(context.Background(), telemetry.Config{SpanExporter: exporter})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
			next := &recordingStarter{}
			starter := selectEntryStarter(entryStarterInput{selection: selection, runnerStarter: next, telemetry: client, def: def})
			if tc.class == "" {
				if _, ok := starter.(*engineStarter); !ok {
					t.Fatalf("migrated routing changed to %T", starter)
				}
				if len(exporter.GetSpans()) != 0 {
					t.Fatal("migrated workflow emitted a fallback signal")
				}
				return
			}
			req := localscheduler.StartRequest{RunID: "0123456789abcdef0123456789abcdef", Gaggle: "web"}
			if _, err := starter.Start(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(next.calls, []localscheduler.StartRequest{req}) {
				t.Fatalf("changed request: %+v", next.calls)
			}
			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("spans = %d", len(spans))
			}
			attrs := map[string]any{}
			for _, attr := range spans[0].Attributes {
				attrs[string(attr.Key)] = attr.Value.AsInterface()
			}
			if attrs[telemetry.AttrWorkflow] != def.Name || attrs[telemetry.AttrGaggle] != req.Gaggle {
				t.Fatalf("lost workflow identity: %v", attrs)
			}
			if attrs["goobers.engine.fallback.reason_class"] != tc.class || attrs["goobers.engine.fallback.placement_declared"] != tc.declared {
				t.Fatalf("attributes = %v", attrs)
			}
			wantSelf := int64(0)
			if tc.self {
				wantSelf = 1
			}
			if attrs["goobers.engine.fallback.self_pinned_count"] != wantSelf || attrs["goobers.engine.fallback.unpinned_gate_count"] != int64(0) {
				t.Fatalf("counts = %v", attrs)
			}
		})
	}
}

func TestEngineFallbackStatusDistinguishesExpectedLocalFromDeclaredPlacement(t *testing.T) {
	if got := engineFallbackStatusLines(readservice.SchedulerStatus{}); got != "" {
		t.Fatalf("unexpected noise: %q", got)
	}
	for _, declared := range []bool{false, true} {
		fallback := readmodel.EngineFallback{Gaggle: "web", Workflow: "implementation", RunID: "run-1", Reason: "implement is self-pinned", ReasonClass: "placement_ineligible", PlacementDeclared: declared}
		got := engineFallbackStatusLines(readservice.SchedulerStatus{EngineFallbacks: []readmodel.EngineFallback{fallback}})
		if strings.HasPrefix(got, "WARNING") != declared || !strings.Contains(got, "web/implementation") || !strings.Contains(got, "run-1") || !strings.Contains(got, "implement is self-pinned") {
			t.Fatalf("status: %q", got)
		}
		wire := statusJSONSummaries([]runSummary{{RunID: "run-1", EngineFallback: &fallback}})
		if len(wire) != 1 || wire[0].EngineFallback == nil {
			t.Fatal("status JSON dropped per-run evidence")
		}
	}
}

func TestEngineFallbackWiresRunMetadataToTrackedStarter(t *testing.T) {
	tracked := &trackedStarter{}
	selection := engineSelection{ReasonClass: "placement_ineligible", PlacementDeclared: true, SelfPinnedStages: []string{"implement"}}
	starter := selectEntryStarter(entryStarterInput{runnerStarter: tracked, selection: selection})
	if _, ok := starter.(*runnerFallbackStarter); !ok {
		t.Fatalf("starter: %T", starter)
	}
	if !reflect.DeepEqual(tracked.starterSelection, selection.annotationFields()) {
		t.Fatalf("run journal metadata not wired: %v", tracked.starterSelection)
	}
	remote := &trackedStarter{}
	selectEntryStarter(entryStarterInput{runnerStarter: remote, selection: engineSelection{UseEngine: true}})
	if remote.starterSelection != nil {
		t.Fatal("engine-selected run received fallback metadata")
	}
}
