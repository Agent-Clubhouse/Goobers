package bootstrap

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

// DSL 3.0 §9.6 spans more than the selector: an unspecified OS must survive
// requirement extraction and solving, and the pinned queue must agree with
// dispatch-time selection. Put Windows first so inventory order cannot stand
// in for the Linux preference. Explicit OS remains a hard requirement.
func TestDSL30PlacementQueueAgreesWithDispatch(t *testing.T) {
	windows := instance.RunnerEntry{
		Name: "windows", Host: "ghcr.io/example/windows:v1",
		Provides: instance.RunnerProvides{OS: instance.RunnerOSWindows, Capabilities: []string{"stage-tools"}},
	}
	linux := instance.RunnerEntry{
		Name: "linux", Host: "ghcr.io/example/linux:v1",
		Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Capabilities: []string{"stage-tools"}},
	}
	for _, tc := range []struct {
		name     string
		runners  []instance.RunnerEntry
		os       string
		selected string
		eligible int
	}{
		{"unspecified mixed", []instance.RunnerEntry{windows, linux}, "", "linux", 2},
		{"unspecified Windows only", []instance.RunnerEntry{windows}, "", "windows", 1},
		{"explicit Windows mixed", []instance.RunnerEntry{windows, linux}, "windows", "windows", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := placementSpecV30([]apiv1.Task{{
				Name: "build", Type: apiv1.TaskDeterministic,
				RunsOn: &apiv1.RunsOn{OS: tc.os, Capabilities: []string{"stage-tools"}},
				Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "docs-churn"}, Workspace: apiv1.WorkspaceScratch},
			}}, nil)
			if _, err := workflow.Compile(def, workflow.WithPreviewFeatures(true)); err != nil {
				t.Fatalf("compile fixture: %v", err)
			}
			pins, err := PinStagePlacements(&instance.Config{Runners: tc.runners}, placementConfigSet(), "web", def)
			if err != nil {
				t.Fatalf("pin placements: %v", err)
			}
			if len(pins) != 1 {
				t.Fatalf("pins = %+v, want one stage", pins)
			}
			pin := pins[0]
			if pin.Self || pin.Stage != "build" || len(pin.Eligible) != tc.eligible {
				t.Fatalf("pin = %+v, want remote build with %d eligible runners", pin, tc.eligible)
			}
			if want := dispatcher.QueueName("web", tc.selected); pin.Queue != want {
				t.Fatalf("pinned queue = %q, want %q", pin.Queue, want)
			}
			for _, attempt := range []int{1, 2} {
				selected, err := dispatcher.SelectRunner(dispatcher.Attempt{
					RunID: "placement-acceptance", Stage: pin.Stage, Number: attempt,
					LedgerTouching: pin.LedgerTouching,
				}, pin.Eligible)
				if err != nil {
					t.Fatalf("dispatch attempt %d: %v", attempt, err)
				}
				if selected.Name != tc.selected || dispatcher.QueueName("web", selected.Name) != pin.Queue {
					t.Fatalf("attempt %d selected %q, inconsistent with pin %+v", attempt, selected.Name, pin)
				}
			}
		})
	}
}
