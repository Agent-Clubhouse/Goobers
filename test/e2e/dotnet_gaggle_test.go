package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

const dotnetGaggleDir = "../../config-examples/gaggles/dotnet-service"

// dotnetServiceMachine loads the SHIPPED dotnet-service gaggle + implementation
// workflow, resolves the per-gaggle CI command into the local-ci stage exactly
// as the daemon does (#1009), and compiles the result. This is what makes the
// test a "shipped-workflow contract test": the machine under test IS the example
// config, and the local-ci command it runs is proven to be the gaggle's
// `dotnet test` — not a value hard-coded by the test.
func dotnetServiceMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(dotnetGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(dotnetGaggleDir, "workflows", "dotnet-implementation.yaml"))
	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"dotnet-implementer", "dotnet-reviewer"} {
		g := loadYAML[apiv1.Goober](t, filepath.Join(dotnetGaggleDir, "goobers", name, "goober.yaml"))
		goobers[g.Name] = g.Spec
	}

	// Resolve the gaggle's ciCommand into the local-ci stage (the real #1009
	// seam), then assert it actually became `dotnet test` before we run it.
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}
	instance.ApplyGaggleCICommand(set)
	wf = set.Workflows[0]
	if got := localCICommand(wf); fmt.Sprint(got) != fmt.Sprint([]string{"dotnet", "test"}) {
		t.Fatalf("local-ci command = %v, want [dotnet test] after #1009 gaggle-ciCommand resolution", got)
	}

	m, err := workflow.Compile(
		workflow.Definition{Name: wf.Name, Version: 1, Spec: wf.Spec},
		workflow.WithGoobers(goobers),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile dotnet-service machine: %v", err)
	}
	return m
}

func localCICommand(wf apiv1.Workflow) []string {
	for _, task := range wf.Spec.Tasks {
		if task.Name == "local-ci" && task.Run != nil {
			return task.Run.Command
		}
	}
	return nil
}

func loadYAML[T any](t *testing.T, path string) T {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out T
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return out
}

// TestDotnetServiceGaggleFailsToScheduleWithoutDotnetCapability is #1093's
// fail-closed acceptance (needs no SDK): the shipped gaggle declares
// `requiredCapabilities: [dotnet@9]`, so a runner that does not claim it is
// refused UP FRONT with a diagnostic naming the missing capability (RRQ-1/#1101)
// — not a cryptic runtime "command not found". A runner that claims it schedules.
func TestDotnetServiceGaggleFailsToScheduleWithoutDotnetCapability(t *testing.T) {
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(dotnetGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(dotnetGaggleDir, "workflows", "dotnet-implementation.yaml"))
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}

	err := instance.CheckCapabilityRequirements([]string{"os=linux"}, set)
	if err == nil || !strings.Contains(err.Error(), "dotnet@9") {
		t.Fatalf("a runner not claiming dotnet@9 must fail to schedule with a diagnostic naming it, got %v", err)
	}
	if err := instance.CheckCapabilityRequirements([]string{"dotnet@9"}, set); err != nil {
		t.Fatalf("a runner claiming dotnet@9 must schedule, got %v", err)
	}
}
