package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseAuthorizationWorkflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Permissions map[string]string    `yaml:"permissions"`
	Jobs        map[string]struct {
		Outputs  map[string]string `yaml:"outputs"`
		Env      map[string]string `yaml:"env"`
		RunsOn   string            `yaml:"runs-on"`
		Strategy struct {
			Matrix struct {
				Include []struct{ Runner, Target, Platform string } `yaml:"include"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
		Needs           yaml.Node         `yaml:"needs"`
		If              string            `yaml:"if"`
		ContinueOnError bool              `yaml:"continue-on-error"`
		Permissions     map[string]string `yaml:"permissions"`
		Steps           []struct {
			ID              string            `yaml:"id"`
			Name            string            `yaml:"name"`
			If              string            `yaml:"if"`
			ContinueOnError bool              `yaml:"continue-on-error"`
			Run             string            `yaml:"run"`
			Shell           string            `yaml:"shell"`
			With            map[string]string `yaml:"with"`
			Env             map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadReleaseAuthorizationWorkflow(t *testing.T) releaseAuthorizationWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseAuthorizationWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestReleaseAuthorizationPermissions(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	readOnly := map[string]string{"contents": "read"}
	if !maps.Equal(workflow.Permissions, readOnly) {
		t.Fatalf("default release permissions = %v; want contents:read only", workflow.Permissions)
	}
	for name, job := range workflow.Jobs {
		want := readOnly
		switch name {
		case "sign-windows":
			want = map[string]string{"contents": "read", "id-token": "write"}
		case "verify-and-publish":
			want = map[string]string{"contents": "write"}
		}
		got := job.Permissions
		if got == nil {
			got = workflow.Permissions
		}
		if !maps.Equal(got, want) {
			t.Errorf("job %s permissions = %v; want %v", name, got, want)
		}
	}
}

func TestReleaseAuthorizationPrecedesBuildAndPublication(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Fatal("manual release-tag reruns must remain available")
	}
	var scripts []string
	for _, name := range []string{"build", "validate-release", "verify-and-publish"} {
		job := workflow.Jobs[name]
		if len(job.Steps) < 2 {
			t.Fatalf("job %s has no checkout and authorization steps", name)
		}
		checkout, guard := job.Steps[0], job.Steps[1]
		if checkout.Name != "Checkout" || checkout.With["ref"] != "${{ github.sha }}" || checkout.With["fetch-depth"] != "0" || checkout.With["persist-credentials"] != "false" {
			t.Errorf("job %s must check out the event source with all tags and no persisted credential", name)
		}
		if guard.Name != "Authorize release source" || guard.ID != "release-source" || guard.If != "" || guard.Shell != "bash" || strings.TrimSpace(guard.Run) == "" {
			t.Fatalf("job %s must authorize unconditionally immediately after checkout", name)
		}
		wantEnv := map[string]string{
			"RELEASE_EVENT": "${{ github.event_name }}", "RELEASE_REF": "${{ github.ref }}", "EXPECTED_SOURCE": "${{ github.sha }}",
		}
		if !maps.Equal(guard.Env, wantEnv) {
			t.Errorf("job %s authorization must receive identities as environment data: %v", name, guard.Env)
		}
		scripts = append(scripts, guard.Run)
	}
	if scripts[0] != scripts[1] || scripts[0] != scripts[2] {
		t.Error("build and publication must enforce the same source authorization")
	}
	for name, want := range map[string][]string{
		"sign-macos": {"build"}, "sign-windows": {"sign-macos"},
		"native-smoke": {"sign-windows"}, "verify-and-publish": {"sign-windows", "native-smoke", "native-linux-images", "native-windows-image", "validate-release"},
		"validate-release":    {"sign-windows", "native-smoke", "native-linux-images", "native-windows-image"},
		"native-linux-images": {"build", "sign-windows"}, "native-windows-image": {"build", "sign-windows"},
	} {
		job := workflow.Jobs[name]
		var got []string
		if job.Needs.Kind == yaml.ScalarNode {
			got = []string{job.Needs.Value}
		} else if err := job.Needs.Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) || job.If != "" {
			t.Errorf("job %s must require successful authorization/signing/smoke predecessors: needs=%v if=%q", name, got, job.If)
		}
	}
}

func TestReleaseBuildUsesAuthorizedCommit(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	for name, key := range map[string]string{
		"Build Portal asset artifact": "GOOBERS_PORTAL_COMMIT", "Build release artifacts": "COMMIT_SHA",
	} {
		found := false
		for _, step := range workflow.Jobs["build"].Steps {
			if step.Name == name {
				found = true
				if step.Env[key] != "${{ steps.release-source.outputs.commit }}" {
					t.Errorf("%s must build from the authorized peeled source commit", name)
				}
			}
		}
		if !found {
			t.Errorf("missing build step %s", name)
		}
	}
}
