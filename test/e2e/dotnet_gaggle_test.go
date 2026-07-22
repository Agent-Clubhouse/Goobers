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
