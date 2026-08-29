package bootstrap

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

func placementSpec(tasks ...apiv1.Task) workflow.Definition {
	return workflow.Definition{
		Name: "impl",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    tasks[0].Name,
			Tasks:    tasks,
		},
	}
}

func distributedConfig() *instance.Config {
	return &instance.Config{Runners: []instance.RunnerEntry{
		{
			// A declared self OS keeps the pin host-independent in this test:
			// SelectRunner is Linux-preferring, so self wins every stage the
			// remote runner does not uniquely satisfy.
			Name: "self", Host: "self",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux},
		},
		{
			Name: "win-ci", Host: "ghcr.io/example/win:v1",
			Provides: instance.RunnerProvides{
				OS:           instance.RunnerOSWindows,
				Memory:       "16Gi",
				Capabilities: []string{"win-tools"},
			},
		},
	}}
}

func placementConfigSet() *instance.ConfigSet {
	return &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{
			ObjectMeta: metav1.ObjectMeta{Name: "web"},
			Spec:       apiv1.GaggleSpec{},
		}},
	}
}

// Zero-declaration invariance (architecture §11 item 1): no runners: block,
// and a runners: block whose every entry is self, pin NOTHING — RunInput then
// carries no placements and every workflow arm behaves byte-identically to
// before the cutover.
func TestPinStagePlacementsLocalModePinsNothing(t *testing.T) {
	def := placementSpec(apiv1.Task{Name: "build", Type: apiv1.TaskDeterministic,
		Run: &apiv1.DeterministicRun{Command: []string{"true"}}})
	for _, tc := range []struct {
		name string
		cfg  *instance.Config
	}{
		{"no runners block", &instance.Config{}},
		{"self-only inventory", &instance.Config{Runners: []instance.RunnerEntry{{Name: "self", Host: "self"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placements, err := PinStagePlacements(tc.cfg, placementConfigSet(), "web", def)
			if err != nil {
				t.Fatalf("PinStagePlacements: %v", err)
			}
			if placements != nil {
				t.Fatalf("placements = %+v, want nil (the local path untouched)", placements)
			}
		})
	}
}

// On a distributed inventory: a stage only the remote runner satisfies pins
// to its (gaggle × runner) queue with the eligible spec and requirement
// facts; a stage self satisfies pins Self and carries none of them.
func TestPinStagePlacementsDistributedInventory(t *testing.T) {
	def := placementSpec(
		apiv1.Task{Name: "build", Type: apiv1.TaskDeterministic,
			RequiredCapabilities: []string{"win-tools"},
			Run:                  &apiv1.DeterministicRun{Command: []string{"build.cmd"}},
			Next:                 "close-out"},
		apiv1.Task{Name: "close-out", Type: apiv1.TaskDeterministic,
			PolicyActions: []string{"claim-backlog-items"},
			Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}},
	)
	placements, err := PinStagePlacements(distributedConfig(), placementConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v", err)
	}
	if len(placements) != 2 {
		t.Fatalf("placements = %+v, want one per task", placements)
	}

	build := placements[0]
	if build.Stage != "build" || build.Self {
		t.Fatalf("build placement = %+v, want a remote pin", build)
	}
	if build.Queue != "goobers-dispatch.web.win-ci" {
		t.Fatalf("build queue = %q, want the (gaggle x runner) queue", build.Queue)
	}
	if len(build.Eligible) != 1 || build.Eligible[0].Name != "win-ci" || build.Eligible[0].Host != "ghcr.io/example/win:v1" {
		t.Fatalf("build eligible = %+v, want the resolved remote runner spec", build.Eligible)
	}

	closeOut := placements[1]
	if !closeOut.Self || closeOut.Queue != "" || closeOut.Eligible != nil {
		t.Fatalf("close-out placement = %+v, want the self pin with no dispatch facts", closeOut)
	}
	if !closeOut.LedgerTouching {
		t.Fatal("a stage declaring claim-backlog-items must pin as ledger-touching (architecture D12)")
	}
}

// A ledger-touching stage whose only eligible runner is Windows is refused at
// START with the structural fact named — never green-lit into a run that can
// only die at dispatch (architecture D12: ledger-touching never places on
// Windows).
func TestPinStagePlacementsRefusesLedgerTouchingOnWindowsOnly(t *testing.T) {
	def := placementSpec(apiv1.Task{Name: "claim", Type: apiv1.TaskDeterministic,
		RequiredCapabilities: []string{"win-tools"},
		PolicyActions:        []string{"claim-backlog-items"},
		Run:                  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}})
	_, err := PinStagePlacements(distributedConfig(), placementConfigSet(), "web", def)
	if err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("error = %v, want the ledger-touching Windows refusal named", err)
	}
}

// An unplaceable stage refuses the start with the solver's diagnostic rather
// than stranding the run at schedule-to-start.
func TestPinStagePlacementsRefusesUnsatisfiableStage(t *testing.T) {
	def := placementSpec(apiv1.Task{Name: "build", Type: apiv1.TaskDeterministic,
		RequiredCapabilities: []string{"gpu"},
		Run:                  &apiv1.DeterministicRun{Command: []string{"true"}}})
	_, err := PinStagePlacements(distributedConfig(), placementConfigSet(), "web", def)
	if err == nil || !strings.Contains(err.Error(), "cannot place") {
		t.Fatalf("error = %v, want the unsatisfiable refusal with the diagnostic", err)
	}
}
