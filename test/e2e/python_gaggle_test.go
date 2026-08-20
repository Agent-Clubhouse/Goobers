package e2e

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

const pythonGaggleDir = "../../config-examples/gaggles/python-service"

func TestPythonServiceGaggleFailsToScheduleWithoutPythonCapability(t *testing.T) {
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(pythonGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(pythonGaggleDir, "workflows", "python-implementation.yaml"))
	want := []string{"os=linux", "python@3.12"}
	if got := instance.WorkflowRequiredCapabilities(gaggle, wf); !reflect.DeepEqual(got, want) {
		t.Fatalf("required capabilities = %v, want %v", got, want)
	}

	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}
	err := instance.CheckCapabilityRequirements([]string{"python@3.11"}, set)
	if err == nil || !strings.Contains(err.Error(), "python@3.12") {
		t.Fatalf("runner without python@3.12 must fail to schedule with a diagnostic naming it, got %v", err)
	}
	if err := instance.CheckCapabilityRequirements(want, set); err != nil {
		t.Fatalf("runner claiming Linux and python@3.12 must schedule: %v", err)
	}
}

func TestPythonServiceGaggleResolvesDeclaredCICommand(t *testing.T) {
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(pythonGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(pythonGaggleDir, "workflows", "python-implementation.yaml"))
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}

	instance.ApplyGaggleCICommand(set)
	want := []string{"python3", "-m", "pytest", "-q"}
	var got []string
	for _, task := range set.Workflows[0].Spec.Tasks {
		if task.Name == "local-ci" && task.Run != nil {
			got = task.Run.Command
			break
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved local-ci command = %v, want %v", got, want)
	}
}
