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
	kustomization, err := os.ReadFile("../goobers-system/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(kustomization), "priorityclasses.yaml") {
		t.Fatal("legacy overlay redefines cloud-reference PriorityClasses")
	}
	if _, err := os.Stat("../goobers-system/priorityclasses.yaml"); !os.IsNotExist(err) {
		t.Fatalf("legacy PriorityClass ownership is duplicated: %v", err)
	}
	deployment := loadManifest(t, "../goobers-system/worker-deployment.yaml")
	podSpec, found, err := unstructured.NestedMap(
		deployment, "spec", "template", "spec")
	if err != nil || !found {
		t.Fatalf("pod spec: found=%v err=%v", found, err)
	}
	if podSpec["priorityClassName"] != "goobers-control-plane" {
		t.Fatalf("priorityClassName = %v", podSpec["priorityClassName"])
	}
	if podSpec["terminationGracePeriodSeconds"] != int64(360) &&
		podSpec["terminationGracePeriodSeconds"] != float64(360) {
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
		manifestInt(t, podSpec, "terminationGracePeriodSeconds") != 360 {
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
		template, "spec", "template", "spec", "selector")
	if err != nil || !found || len(selector) < 2 {
		t.Fatalf("fail-closed selector = %+v found=%v err=%v",
			selector, found, err)
	}
	matchLabels, found, err := unstructured.NestedMap(
		selector, "matchLabels")
	if err != nil || !found || len(matchLabels) != 0 {
		t.Fatalf("injected matchLabels sentinel = %+v found=%v err=%v",
			matchLabels, found, err)
	}
	selectorYAML, err := yaml.Marshal(selector)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"temporal.io/build-id", "temporal.io/deployment-name", "Exists", "goobers",
	} {
		if !strings.Contains(string(selectorYAML), required) {
			t.Fatalf("selector lacks %q:\n%s", required, selectorYAML)
		}
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
		"v1.13.0", "0.32.0", "PodDisruptionBudget", "policy",
		"poddisruptionbudgets",
	} {
		if !strings.Contains(string(values), required) {
			t.Fatalf("controller values lack %q", required)
		}
	}

	deletePatch := loadManifest(t, "worker-ha/legacy-worker-pdb-delete.yaml")
	if deletePatch["kind"] != "PodDisruptionBudget" ||
		deletePatch["$patch"] != "delete" {
		t.Fatalf("legacy PDB delete patch = %+v", deletePatch)
	}
}

func TestRainbowPDBSelectorsDoNotOverlap(t *testing.T) {
	builds := []string{"build-a", "build-b"}
	for _, podBuild := range builds {
		matches := 0
		for _, pdbBuild := range builds {
			if podBuild == pdbBuild {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("build %q matched %d per-build PDBs", podBuild, matches)
		}
	}
	kustomization, err := os.ReadFile("worker-ha/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(kustomization), "legacy-worker-pdb-delete.yaml") {
		t.Fatal("rainbow component does not delete the broad legacy PDB")
	}
	template, err := os.ReadFile("worker-ha/worker-pdb-template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(template), "goobers.dev/build") ||
		!strings.Contains(string(template), "temporal.io/build-id") {
		t.Fatal("rainbow PDB does not use the Temporal build-label contract")
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
