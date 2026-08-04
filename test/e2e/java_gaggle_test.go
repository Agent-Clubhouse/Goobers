package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

const javaGaggleDir = "../../config-examples/gaggles/java-service"

func TestJavaServiceGaggleFailsToScheduleWithoutJavaCapability(t *testing.T) {
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(javaGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(javaGaggleDir, "workflows", "java-implementation.yaml"))
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}

	err := instance.CheckCapabilityRequirements([]string{"os=linux"}, set)
	if err == nil || !strings.Contains(err.Error(), "java@21") {
		t.Fatalf("a runner not claiming java@21 must fail to schedule with a diagnostic naming it, got %v", err)
	}
	if err := instance.CheckCapabilityRequirements([]string{"java@21"}, set); err != nil {
		t.Fatalf("a runner claiming java@21 must schedule, got %v", err)
	}
}
