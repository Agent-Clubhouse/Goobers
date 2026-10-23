package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHANightlyStaysOffPullRequestGate(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "ha-nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			RunsOn         string `yaml:"runs-on"`
			TimeoutMinutes int    `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.On.Kind != yaml.MappingNode {
		t.Fatalf("on = %#v, want mapping", workflow.On)
	}
	var schedule, dispatch bool
	for i := 0; i < len(workflow.On.Content); i += 2 {
		switch workflow.On.Content[i].Value {
		case "schedule":
			schedule = true
		case "workflow_dispatch":
			dispatch = true
		case "pull_request", "pull_request_target", "merge_group":
			t.Fatalf("HA nightly must stay off required paths; found %q trigger", workflow.On.Content[i].Value)
		}
	}
	if !schedule || !dispatch {
		t.Fatalf("HA nightly triggers: schedule=%v workflow_dispatch=%v", schedule, dispatch)
	}
	job := workflow.Jobs["smoke"]
	if job.RunsOn != "ubuntu-24.04" || job.TimeoutMinutes != 90 {
		t.Fatalf("smoke runner/timeout = %q/%d, want ubuntu-24.04/90", job.RunsOn, job.TimeoutMinutes)
	}
}

func TestHANightlyDeclaresRequiredTopologyAndOperations(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ha-nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := os.ReadFile(filepath.Join(root, "test", "ha", "kind", "cmd", "nightly", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := os.ReadFile(filepath.Join(root, "test", "ha", "kind", "cnpg-cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	kindConfig, err := os.ReadFile(filepath.Join(root, "test", "ha", "kind", "kind.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	all := string(workflow) + "\n" + string(run) + "\n" + string(cluster) + "\n" + string(kindConfig)
	for _, want := range []string{
		"HA_NIGHTLY_OWNER_ROTATION: control-plane-ha-maintainers",
		"disableDefaultCNI: true",
		"calico.yaml",
		"instances: 3",
		"temporal.tgz",
		"temporal-0.64.0.tgz",
		"temporal-0.68.1.tgz",
		"temporal-0.74.0.tgz",
		"temporal-1.3.0.tgz",
		"toxiproxy-server",
		`"kind", "load", "docker-image", "goobers:ha-nightly"`,
		`"kubectl", "apply", "-k", "test/ha/kind/reference"`,
		`Test(E2|E4|E11)WalkHistoryReplays$`,
		"Benchmarks: not yet registered; future benchmark values are reported, never asserted",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("HA nightly environment is missing %q", want)
		}
	}
}
