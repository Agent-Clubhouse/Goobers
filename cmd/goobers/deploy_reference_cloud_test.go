package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/yaml"
)

const cloudRefDir = "../../deploy/reference/goobers-cloud"

func cloudDoc(t *testing.T, file, kind string, out any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cloudRefDir, file))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range splitYAMLDocs(raw) {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind == kind {
			if err := yaml.UnmarshalStrict(doc, out); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("%s: no %s", file, kind)
}

func TestDeployReferenceCloudControlShape(t *testing.T) {
	var d appsv1.Deployment
	cloudDoc(t, "control-deployment.yaml", "Deployment", &d)
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("P3 control must be 1 replica, got %v", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q, want Recreate (lease-gated readiness deadlocks RollingUpdate)", d.Spec.Strategy.Type)
	}
	spec := d.Spec.Template.Spec
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 30 {
		t.Fatalf("grace = %v, want 30", spec.TerminationGracePeriodSeconds)
	}
	if spec.PriorityClassName != "goobers-control-plane" {
		t.Fatalf("priorityClassName = %q", spec.PriorityClassName)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("control must not automount a service account token")
	}
	tolerations := map[string]int64{}
	for _, tol := range spec.Tolerations {
		if tol.TolerationSeconds != nil {
			tolerations[tol.Key] = *tol.TolerationSeconds
		}
	}
	for _, key := range []string{"node.kubernetes.io/unreachable", "node.kubernetes.io/not-ready"} {
		if tolerations[key] != 30 {
			t.Fatalf("toleration %s seconds = %d, want 30", key, tolerations[key])
		}
	}
	if len(spec.TopologySpreadConstraints) == 0 {
		t.Fatal("missing topology spread")
	}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Fatalf("control must have no PVC, found volume %q", v.Name)
		}
		if strings.Contains(strings.ToLower(v.Name), "workspace") || strings.Contains(strings.ToLower(v.Name), "harness") {
			t.Fatalf("control must not carry stage volume %q", v.Name)
		}
	}
	c := spec.Containers[0]
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Exec == nil ||
		strings.Join(c.Lifecycle.PreStop.Exec.Command, " ") != "/usr/local/bin/goobers-cloud sleep 5" {
		t.Fatalf("preStop = %+v", c.Lifecycle)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatal("readiness must be /readyz")
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" || c.LivenessProbe.TerminationGracePeriodSeconds == nil {
		t.Fatal("liveness needs /healthz and a probe-level terminationGracePeriodSeconds (KEP-2238)")
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("control root filesystem must be read-only")
	}
	assertNoInlineSecrets(t, "control", c)
}

// Secrets travel by secretKeyRef or mounted file; a credential in argv or an
// inline env value is visible in the process table and pod spec.
func assertNoInlineSecrets(t *testing.T, name string, c corev1.Container) {
	t.Helper()
	for _, a := range append(append([]string{}, c.Command...), c.Args...) {
		l := strings.ToLower(a)
		if strings.Contains(a, "://") || strings.Contains(l, "password=") || strings.Contains(l, "token=") {
			t.Fatalf("%s: credential-looking argv %q", name, a)
		}
	}
	for _, e := range c.Env {
		l := strings.ToLower(e.Name)
		if e.Value != "" && (strings.Contains(l, "password") || strings.Contains(l, "secret") || strings.Contains(l, "token") || strings.HasSuffix(l, "_url")) {
			t.Fatalf("%s: env %s has an inline value", name, e.Name)
		}
	}
}

func TestDeployReferenceCloudPDBIsSingleReplicaSafe(t *testing.T) {
	var p policyv1.PodDisruptionBudget
	cloudDoc(t, "control-pdb.yaml", "PodDisruptionBudget", &p)
	if p.Spec.MinAvailable != nil {
		t.Fatal("minAvailable on a single replica blocks node drains")
	}
	if p.Spec.MaxUnavailable == nil || p.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("maxUnavailable = %v, want 1", p.Spec.MaxUnavailable)
	}
	if p.Spec.UnhealthyPodEvictionPolicy == nil || *p.Spec.UnhealthyPodEvictionPolicy != policyv1.AlwaysAllow {
		t.Fatal("unhealthyPodEvictionPolicy must be AlwaysAllow")
	}
}

func TestDeployReferenceCloudMigrateJob(t *testing.T) {
	var j batchv1.Job
	cloudDoc(t, "migrate-job.yaml", "Job", &j)
	if j.Annotations["argocd.argoproj.io/hook"] != "PreSync" {
		t.Fatal("migrate must run as a PreSync hook so it precedes the control rollout")
	}
	if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 {
		t.Fatal("backoffLimit must be 0")
	}
	if j.Spec.ActiveDeadlineSeconds == nil || *j.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatal("activeDeadlineSeconds must be 1800")
	}
	spec := j.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("migrate must not automount a token")
	}
	c := spec.Containers[0]
	assertNoInlineSecrets(t, "migrate", c)
	var fromSecret bool
	for _, e := range c.Env {
		if e.Name == "GOOBERS_DATABASE_DIRECT_URL" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			fromSecret = true
		}
	}
	if !fromSecret {
		t.Fatal("direct URL must come from a secretKeyRef")
	}
}

func TestDeployReferenceCloudNamespaceAndPolicies(t *testing.T) {
	var ns corev1.Namespace
	cloudDoc(t, "namespace.yaml", "Namespace", &ns)
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatal("namespace must enforce PSS restricted")
	}
	raw, err := os.ReadFile(filepath.Join(cloudRefDir, "networkpolicies.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "name: default-deny-all") {
		t.Fatal("missing default-deny floor")
	}
}

// TestDeployReferenceTemporalHAValues pins the HA-P3-22 sizing: a drift in
// shards or replica counts is a design change (shards are fixed forever).
func TestDeployReferenceTemporalHAValues(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/temporal/values-ha.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Server map[string]any `json:"server"`
		Web    map[string]any `json:"web"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for svc, want := range map[string]float64{"frontend": 3, "history": 3, "matching": 3, "worker": 2} {
		s := v.Server[svc].(map[string]any)
		if s["replicaCount"] != want {
			t.Fatalf("%s replicaCount = %v, want %v", svc, s["replicaCount"], want)
		}
		if s["podDisruptionBudget"].(map[string]any)["maxUnavailable"] != float64(1) {
			t.Fatalf("%s PDB maxUnavailable != 1", svc)
		}
	}
	if v.Web["replicaCount"] != float64(2) {
		t.Fatalf("web replicaCount = %v", v.Web["replicaCount"])
	}
	persistence := v.Server["config"].(map[string]any)["persistence"].(map[string]any)
	if persistence["numHistoryShards"] != float64(512) {
		t.Fatalf("numHistoryShards = %v, want 512", persistence["numHistoryShards"])
	}
	for _, store := range []string{"default", "visibility"} {
		sql := persistence["datastores"].(map[string]any)[store].(map[string]any)["sql"].(map[string]any)
		addr, _ := sql["connectAddr"].(string)
		if sql["user"] != "temporal_app" || !strings.HasSuffix(addr, ":5432") {
			t.Fatalf("%s store must use temporal_app on direct 5432, got %v/%v", store, sql["user"], addr)
		}
	}
}
