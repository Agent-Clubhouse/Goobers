package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/supportmatrix"
)

// A syntactically valid smoke workflow can still fail before building an image
// if its candidate is older than a compiled-in DSL support transition (#4215).
func TestWindowsImageWorkflowUsesRealReleasePolicy(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "windows-image-verify.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On map[string]struct {
			Inputs map[string]struct {
				Default string `yaml:"default"`
			} `yaml:"inputs"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Steps []struct {
				Name, Run, Shell string
				Env              map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 {
		t.Fatalf("image verification must remain manual: %v", workflow.On)
	}
	dispatch, ok := workflow.On["workflow_dispatch"]
	if !ok {
		t.Fatal("missing manual dispatch")
	}
	candidate := dispatch.Inputs["candidate_version"].Default
	line, _, _ := strings.Cut(candidate, "-")
	if candidate == "" || !strings.HasPrefix(line, "v") {
		t.Fatalf("missing release candidate default: %q", candidate)
	}
	if err := supportmatrix.ValidateSupportPolicyForRelease(supportmatrix.GetDSL(), line); err != nil {
		t.Fatalf("default candidate %s cannot pass the release support gate: %v", candidate, err)
	}
	if dispatch.Inputs["baseline_tag"].Default != "v0.3.3" {
		t.Fatal("expected the published v0.3.3 stable baseline by default")
	}
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
		t.Fatalf("verification must not grant publication permissions: %v", workflow.Permissions)
	}
	var runs strings.Builder
	strictNative := false
	for _, step := range workflow.Jobs["windows-image"].Steps {
		runs.WriteString(step.Run)
		if strings.Contains(step.Run, "TestIntegrationWindowsImage") {
			strictNative = step.Shell == "powershell" && step.Env["TESTDEP_STRICT"] == "1" && strings.Contains(step.Run, "-tags=integration")
		}
	}
	text := runs.String()
	for _, required := range []string{"gh release download", "--pattern feature-registry.json", "--pattern dsl-support-matrix.json", "-previous-features", "-previous-support-matrix", "$env:CANDIDATE_VERSION"} {
		if !strings.Contains(text, required) {
			t.Errorf("manual image verification omits real release input %q", required)
		}
	}
	for _, bypass := range []string{"-first-feature-snapshot", "docker push", "docker login"} {
		if strings.Contains(text, bypass) {
			t.Errorf("prepublication verification must not use %q", bypass)
		}
	}
	if !strictNative {
		t.Fatal("manual native verifier must require the integration dependency under Windows PowerShell 5.1")
	}
}
