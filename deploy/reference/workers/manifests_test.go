package workers

import (
	"fmt"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/instance"
)

func readObject(t *testing.T, path string) map[string]any {
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

func nestedInt(object map[string]any, fields ...string) (int, error) {
	value, found, err := unstructured.NestedFieldNoCopy(object, fields...)
	if err != nil || !found {
		return 0, fmt.Errorf("read %v: found=%v err=%v", fields, found, err)
	}
	switch number := value.(type) {
	case float64:
		return int(number), nil
	case int64:
		return int(number), nil
	default:
		return 0, fmt.Errorf("read %v: value %v has type %T", fields, value, value)
	}
}

func TestCurrentVersionNeverSetWithoutPollers(t *testing.T) {
	object := readObject(t, "worker-deployment.yaml")
	if object["apiVersion"] != "temporal.io/v1alpha1" || object["kind"] != "WorkerDeployment" {
		t.Fatalf("rainbow resource = %v %v", object["apiVersion"], object["kind"])
	}
	strategy, found, err := unstructured.NestedString(object, "spec", "rollout", "strategy")
	if err != nil || !found || strategy != "AllAtOnce" {
		t.Fatalf("rollout strategy = %q, found=%v err=%v", strategy, found, err)
	}
	replicas, err := nestedInt(object, "spec", "deployment", "replicas")
	if err != nil || replicas != 2 {
		t.Fatalf("active replicas = %d, err=%v", replicas, err)
	}
	deploymentStrategy, found, err := unstructured.NestedString(object, "spec", "deployment", "strategy", "type")
	if err != nil || !found || deploymentStrategy != "Recreate" {
		t.Fatalf("per-build Deployment strategy = %q, found=%v err=%v", deploymentStrategy, found, err)
	}
	draining, err := nestedInt(object, "spec", "drainingDeployment", "replicas")
	if err != nil || draining != 2 {
		t.Fatalf("draining replicas = %d, err=%v", draining, err)
	}
	containers, found, err := unstructured.NestedSlice(object, "spec", "deployment", "template", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("worker containers = %d, found=%v err=%v", len(containers), found, err)
	}
	container := containers[0].(map[string]any)
	for _, probe := range []string{"startupProbe", "readinessProbe"} {
		path, found, err := unstructured.NestedString(container, probe, "httpGet", "path")
		if err != nil || !found || path == "" {
			t.Fatalf("%s does not gate controller health: path=%q found=%v err=%v", probe, path, found, err)
		}
	}
}

func TestWorkerControllerVersionCapAndDrainPolicy(t *testing.T) {
	values := readObject(t, "controller-values.yaml")
	env, found, err := unstructured.NestedSlice(values, "extraEnv")
	if err != nil || !found {
		t.Fatalf("controller extraEnv missing: %v", err)
	}
	capValue := ""
	for _, raw := range env {
		entry := raw.(map[string]any)
		if entry["name"] == "CONTROLLER_MAX_DEPLOYMENT_VERSIONS_INELIGIBLE_FOR_DELETION" {
			capValue, _ = entry["value"].(string)
		}
	}
	if capValue != "2" {
		t.Fatalf("deprecated-version cap = %q, want 2 plus current = 3 live versions", capValue)
	}
	worker := readObject(t, "worker-deployment.yaml")
	scaleDelay, _, _ := unstructured.NestedString(worker, "spec", "sunset", "scaledownDelay")
	deleteDelay, _, _ := unstructured.NestedString(worker, "spec", "sunset", "deleteDelay")
	if scaleDelay != "0s" || deleteDelay != "24h" {
		t.Fatalf("sunset = scale %q delete %q", scaleDelay, deleteDelay)
	}
	config, err := instance.LoadConfig("../instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !config.EffectiveEngineConfig().WorkerVersioning {
		t.Fatal("reference instance does not enable worker versioning")
	}
}
