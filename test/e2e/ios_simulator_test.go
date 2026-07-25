package e2e

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

const iosSimulatorGaggleDir = "../../config-examples/gaggles/ios-simulator"

func TestIOSSimulatorWorkflowDeclaresFailClosedHostRequirements(t *testing.T) {
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(iosSimulatorGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(iosSimulatorGaggleDir, "workflows", "ios-simulator-test.yaml"))
	want := []string{"os=darwin", "xcode"}
	if got := instance.WorkflowRequiredCapabilities(gaggle, wf); !reflect.DeepEqual(got, want) {
		t.Fatalf("required capabilities = %v, want %v", got, want)
	}

	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}
	for _, test := range []struct {
		name    string
		claims  []string
		missing string
	}{
		{name: "non-macOS", claims: []string{"os=linux", "xcode"}, missing: "os=darwin"},
		{name: "no Xcode", claims: []string{"os=darwin"}, missing: "xcode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := instance.CheckCapabilityRequirements(test.claims, set)
			if err == nil || !strings.Contains(err.Error(), test.missing) {
				t.Fatalf("claims %v must be rejected before invocation with %q, got %v", test.claims, test.missing, err)
			}
		})
	}
	if err := instance.CheckCapabilityRequirements(want, set); err != nil {
		t.Fatalf("matching macOS/Xcode claims must schedule: %v", err)
	}
}

func TestIOSSimulatorWorkflowUsesXCResultStageAndGate(t *testing.T) {
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(iosSimulatorGaggleDir, "workflows", "ios-simulator-test.yaml"))
	if len(wf.Spec.Tasks) != 1 || wf.Spec.Tasks[0].Run == nil {
		t.Fatalf("tasks = %+v, want one deterministic stage", wf.Spec.Tasks)
	}
	task := wf.Spec.Tasks[0]
	wantCommand := []string{
		"goobers", "ios-simulator-test", "--project", "GoobersIOS.xcodeproj",
		"--scheme", "GoobersIOS", "--only-testing", "GoobersIOSUITests",
	}
	if !reflect.DeepEqual(task.Run.Command, wantCommand) {
		t.Fatalf("stage command = %v, want %v", task.Run.Command, wantCommand)
	}
	if task.Inputs["resultFile"] != "ios-simulator-result.json" || task.Next != "xcuitest-gate" {
		t.Fatalf("stage result/gate contract = inputs %v next %q", task.Inputs, task.Next)
	}
	if len(wf.Spec.Gates) != 1 || wf.Spec.Gates[0].Automated == nil ||
		wf.Spec.Gates[0].Automated.Check != "status-equals" {
		t.Fatalf("gates = %+v, want status-equals xcresult gate", wf.Spec.Gates)
	}
	if got := wf.Spec.Gates[0].Branches["fail"]; got != "@abort" {
		t.Fatalf("xcresult gate fail branch = %q, want @abort", got)
	}
}
