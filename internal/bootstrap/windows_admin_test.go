package bootstrap

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnercap"
)

// The run-start pin carries the stage's effective runsOn.capabilities
// requirement beside its restrictions (#3619): the dispatcher decides a
// Windows pod's container identity from whether the stage REQUIRES
// privilege=windows-admin, and the workflow cannot recompute the requirement
// mid-run. The eligible runner spec carries the class's provides.capabilities
// for the same reason (the PROVIDES half of the decision).
func TestPinStagePlacementsCarriesRunnerCapabilityRequirement(t *testing.T) {
	cfg := &instance.Config{Runners: []instance.RunnerEntry{
		{
			Name: "self", Host: "self",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux},
		},
		{
			Name: "windows-admin", Host: "ghcr.io/example/win:v1",
			Provides: instance.RunnerProvides{
				OS:           instance.RunnerOSWindows,
				Memory:       "16Gi",
				Shell:        true,
				Capabilities: []string{"dotnet@8", runnercap.CapabilityWindowsAdmin},
			},
			Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral},
		},
	}}
	def := placementSpecV30(
		[]apiv1.Task{{
			Name: "install-service", Type: apiv1.TaskDeterministic, Goal: "install",
			Run: &apiv1.DeterministicRun{Command: []string{"install.cmd"}},
			RunsOn: &apiv1.RunsOn{
				OS:           "windows",
				Capabilities: []string{"dotnet@8", runnercap.CapabilityWindowsAdmin},
				Restrictions: []string{"tmp:ephemeral"},
			},
		}},
		nil,
	)
	placements, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v", err)
	}
	pin := findPin(t, placements, "install-service")
	if pin.Self || pin.Queue != "goobers-dispatch.web.windows-admin" {
		t.Fatalf("pin = %+v, want the remote windows-admin pin", pin)
	}
	// Effective requirement = declared ∪ derived (a command stage derives
	// run:shell) — the whole solved set, like Restrictions.
	want := []string{"dotnet@8", runnercap.CapabilityWindowsAdmin, runnercap.DerivedShellTag}
	if !reflect.DeepEqual(pin.Capabilities, want) {
		t.Fatalf("pin.Capabilities = %v, want %v", pin.Capabilities, want)
	}
	if !reflect.DeepEqual(pin.Restrictions, []string{"tmp:ephemeral"}) {
		t.Fatalf("pin.Restrictions = %v, want [tmp:ephemeral]", pin.Restrictions)
	}
	if len(pin.Eligible) != 1 || !runnercap.HasWindowsAdmin(pin.Eligible[0].Capabilities) {
		t.Fatalf("pin.Eligible = %+v, want the windows-admin runner spec carrying its provides.capabilities", pin.Eligible)
	}
}

// A 2.0 document cannot reach ContainerAdministrator through
// requiredCapabilities (#3619): the pre-3.0 placement arm has no OS and no
// coherence rule, so without the router refusal the same inventory would pin
// this task to the admin class by the accident of which runner claims the
// token, and the dispatcher would render the pod as ContainerAdministrator.
// Refused at the start, no pin manufactured — the reviewer's probe shape.
func TestPinStagePlacementsRefusesWindowsAdminOnAPreV30Document(t *testing.T) {
	cfg := &instance.Config{Runners: []instance.RunnerEntry{
		{
			Name: "self", Host: "self",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux},
		},
		{
			Name: "windows-admin", Host: "ghcr.io/example/win:v1",
			Provides: instance.RunnerProvides{
				OS:           instance.RunnerOSWindows,
				Capabilities: []string{runnercap.CapabilityWindowsAdmin},
			},
			Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral},
		},
	}}
	def := placementSpecV30(
		[]apiv1.Task{{
			Name: "install-service", Type: apiv1.TaskDeterministic, Goal: "install",
			Run:                  &apiv1.DeterministicRun{Command: []string{"install.cmd"}},
			RequiredCapabilities: []string{runnercap.CapabilityWindowsAdmin},
		}},
		nil,
	)
	def.DSLVersion = "2.0"
	placements, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err == nil || !strings.Contains(err.Error(), `task "install-service" declares requiredCapabilities "privilege=windows-admin"`) {
		t.Fatalf("PinStagePlacements(2.0 requiredCapabilities windows-admin) = %+v, %v; want the router refusal and no pin", placements, err)
	}
	if placements != nil {
		t.Fatalf("placements = %+v, want none for a refused document", placements)
	}
}
