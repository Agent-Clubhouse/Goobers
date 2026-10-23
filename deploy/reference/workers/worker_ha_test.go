package workers

import (
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func loadManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := yaml.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestLegacyWorkerHAContract(t *testing.T) {
	deployment := loadManifest(t, "../goobers-system/worker-deployment.yaml")
	podSpec, found, err := unstructured.NestedMap(
		deployment, "spec", "template", "spec")
	if err != nil || !found {
		t.Fatalf("pod spec: found=%v err=%v", found, err)
	}
	if podSpec["priorityClassName"] != "goobers-control-plane" {
		t.Fatalf("priorityClassName = %v", podSpec["priorityClassName"])
	}
	if podSpec["terminationGracePeriodSeconds"] != int64(90) &&
		podSpec["terminationGracePeriodSeconds"] != float64(90) {
		t.Fatalf("termination grace = %v", podSpec["terminationGracePeriodSeconds"])
	}
	assertZoneSpread(t, podSpec)

	pdb := loadManifest(t, "../goobers-system/worker-pdb.yaml")
	maxUnavailable := manifestInt(t, pdb, "spec", "maxUnavailable")
	if maxUnavailable != 1 {
		t.Fatalf("maxUnavailable = %d", maxUnavailable)
	}
	policy, _, _ := unstructured.NestedString(
		pdb, "spec", "unhealthyPodEvictionPolicy")
	if policy != "AlwaysAllow" {
		t.Fatalf("unhealthyPodEvictionPolicy = %q", policy)
	}
}

func TestRainbowWorkerHAContract(t *testing.T) {
	patch := loadManifest(t, "worker-ha/worker-deployment-patch.yaml")
	podSpec, found, err := unstructured.NestedMap(
		patch, "spec", "deployment", "template", "spec")
	if err != nil || !found {
		t.Fatalf("worker patch pod spec: found=%v err=%v", found, err)
	}
	if podSpec["priorityClassName"] != "goobers-control-plane" ||
		manifestInt(t, podSpec, "terminationGracePeriodSeconds") != 90 {
		t.Fatalf("worker patch = %+v", podSpec)
	}
	assertZoneSpread(t, podSpec)

	template := loadManifest(t, "worker-ha/worker-pdb-template.yaml")
	if template["kind"] != "WorkerResourceTemplate" {
		t.Fatalf("resource kind = %v", template["kind"])
	}
	maxUnavailable := manifestInt(
		t, template, "spec", "template", "spec", "maxUnavailable")
	if maxUnavailable != 1 {
		t.Fatalf("template maxUnavailable = %d", maxUnavailable)
	}

	selector, found, err := unstructured.NestedMap(
		template, "spec", "template", "spec", "selector", "matchLabels")
	if err != nil || !found || len(selector) != 0 {
		t.Fatalf("injected selector sentinel = %+v found=%v err=%v",
			selector, found, err)
	}
	policy, _, _ := unstructured.NestedString(
		template, "spec", "template", "spec", "unhealthyPodEvictionPolicy")
	if policy != "AlwaysAllow" {
		t.Fatalf("template unhealthyPodEvictionPolicy = %q", policy)
	}

	values, err := os.ReadFile("worker-ha/controller-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"PodDisruptionBudget", "policy", "poddisruptionbudgets",
	} {
		if !strings.Contains(string(values), required) {
			t.Fatalf("controller values lack %q", required)
		}
	}
}

func manifestInt(t *testing.T, object map[string]any, fields ...string) int {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(object, fields...)
	if err != nil || !found {
		t.Fatalf("read %v: found=%v err=%v", fields, found, err)
	}
	switch number := value.(type) {
	case int64:
		return int(number)
	case float64:
		return int(number)
	default:
		t.Fatalf("read %v: value %v has type %T", fields, value, value)
		return 0
	}
}

func assertZoneSpread(t *testing.T, podSpec map[string]any) {
	t.Helper()
	constraints, found, err := unstructured.NestedSlice(
		podSpec, "topologySpreadConstraints")
	if err != nil || !found || len(constraints) != 1 {
		t.Fatalf("topology spread = %+v found=%v err=%v",
			constraints, found, err)
	}
	constraint := constraints[0].(map[string]any)
	if constraint["topologyKey"] != "topology.kubernetes.io/zone" ||
		constraint["whenUnsatisfiable"] != "ScheduleAnyway" {
		t.Fatalf("zone spread = %+v", constraint)
	}
}
