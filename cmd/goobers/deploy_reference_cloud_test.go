package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	cloudDoc(t, "workload/control-deployment.yaml", "Deployment", &d)
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
	if c.Lifecycle != nil && c.Lifecycle.PreStop != nil {
		t.Fatal("the control role ships no sleep subcommand; rely on SIGTERM lease release, not a preStop exec")
	}
	args := strings.Join(c.Args, " ")
	for _, want := range []string{"control", "--listen=0.0.0.0:8080", "--tls-cert=", "--tls-key=", "--pod-key-file=", "--temporal-hostport=", "--database-auth=$(GOOBERS_DATABASE_AUTH)"} {
		if !strings.Contains(args, want) {
			t.Fatalf("control args missing %q: %s", want, args)
		}
	}
	env := map[string]bool{}
	for _, e := range c.Env {
		env[e.Name] = true
	}
	for _, name := range []string{"GOOBERS_INSTANCE_ID", "GOOBERS_REPLICA_ID", "GOOBERS_DATABASE_AUTH"} {
		if !env[name] {
			t.Fatalf("control env missing %s", name)
		}
	}
	// The role serves probes on --listen (TLS off loopback), so probes must hit
	// the serving port over HTTPS.
	for name, probe := range map[string]*corev1.Probe{"readiness": c.ReadinessProbe, "liveness": c.LivenessProbe} {
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Scheme != corev1.URISchemeHTTPS || probe.HTTPGet.Port.String() != "http" {
			t.Fatalf("%s probe must be HTTPS on the serving port", name)
		}
	}
	if c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatal("readiness must be /readyz")
	}
	if c.LivenessProbe.HTTPGet.Path != "/healthz" || c.LivenessProbe.TerminationGracePeriodSeconds == nil {
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
	cloudDoc(t, "workload/control-pdb.yaml", "PodDisruptionBudget", &p)
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
	cloudDoc(t, "workload/migrate-job.yaml", "Job", &j)
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
	cloudDoc(t, "prereqs/namespace.yaml", "Namespace", &ns)
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatal("namespace must enforce PSS restricted")
	}
	raw, err := os.ReadFile(filepath.Join(cloudRefDir, "prereqs/networkpolicies.yaml"))
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

// TestDeployReferenceCloudControlArgsAreRealFlags checks every --flag in the
// deployment against the flag set the control role registers, once that role is
// in the tree.
func TestDeployReferenceCloudControlArgsAreRealFlags(t *testing.T) {
	src, err := os.ReadFile("../goobers-cloud/control.go")
	if err != nil {
		t.Skip("control role not in tree yet")
	}
	known := map[string]bool{}
	for _, m := range regexp.MustCompile(`f\.StringVar\(&o\.\w+, "([a-z-]+)"`).FindAllStringSubmatch(string(src), -1) {
		known[m[1]] = true
	}
	// Database flags are shared by every database role (runDatabaseRoleConfigured).
	for _, f := range []string{"database-url-file", "database-direct-url-file", "database-auth"} {
		known[f] = true
	}
	if len(known) < 8 {
		t.Fatalf("flag extraction found %d flags; control.go layout changed", len(known))
	}
	var d appsv1.Deployment
	cloudDoc(t, "workload/control-deployment.yaml", "Deployment", &d)
	for _, a := range d.Spec.Template.Spec.Containers[0].Args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !known[name] {
			t.Fatalf("control arg %q is not a flag of the control role", a)
		}
	}
}

func cloudPolicy(t *testing.T, name string) networkingv1.NetworkPolicy {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cloudRefDir, "prereqs/networkpolicies.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range splitYAMLDocs(raw) {
		var p networkingv1.NetworkPolicy
		if err := yaml.UnmarshalStrict(doc, &p); err != nil {
			t.Fatal(err)
		}
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no NetworkPolicy %s", name)
	return networkingv1.NetworkPolicy{}
}

func policyAllowsPort(p networkingv1.NetworkPolicy, port int) bool {
	for _, rule := range p.Spec.Egress {
		for _, rp := range rule.Ports {
			if rp.Port != nil && rp.Port.IntValue() == port {
				return true
			}
		}
	}
	return false
}

// Default-deny means every flow the roles need has to be named: migrate reaches
// the identity provider (Entra) and workers reach the API server to run stages.
func TestDeployReferenceCloudPoliciesCoverRequiredFlows(t *testing.T) {
	migrate := cloudPolicy(t, "allow-migrate-identity")
	if migrate.Spec.PodSelector.MatchLabels["goobers.dev/role"] != "migrate" || !policyAllowsPort(migrate, 443) {
		t.Fatal("migrate needs HTTPS egress for Entra")
	}
	worker := cloudPolicy(t, "allow-worker-kube-apiserver")
	if worker.Spec.PodSelector.MatchLabels["goobers.dev/role"] != "worker" || !policyAllowsPort(worker, 443) || !policyAllowsPort(worker, 6443) {
		t.Fatal("workers need egress to the Kubernetes API server on 443 and 6443")
	}
	// NetworkPolicy matches the post-DNAT endpoint (kind node IP, AKS public IP),
	// so a private-range default would silently block workers.
	for _, rule := range worker.Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock == nil || to.IPBlock.CIDR != "0.0.0.0/0" || len(to.IPBlock.Except) != 0 {
				t.Fatalf("worker API-server default must allow any destination, got %+v", to.IPBlock)
			}
		}
	}
}

// Both workloads read these through configMapKeyRef; a missing ConfigMap leaves
// the Job in CreateContainerConfigError.
func TestDeployReferenceCloudShipsReferencedConfigMaps(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(cloudRefDir, "prereqs/configmaps.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]map[string]string{}
	for _, doc := range splitYAMLDocs(raw) {
		var cm corev1.ConfigMap
		if err := yaml.UnmarshalStrict(doc, &cm); err != nil {
			t.Fatal(err)
		}
		have[cm.Name] = cm.Data
	}
	var j batchv1.Job
	cloudDoc(t, "workload/migrate-job.yaml", "Job", &j)
	var d appsv1.Deployment
	cloudDoc(t, "workload/control-deployment.yaml", "Deployment", &d)
	for _, c := range append(j.Spec.Template.Spec.Containers, d.Spec.Template.Spec.Containers...) {
		for _, e := range c.Env {
			if e.ValueFrom == nil || e.ValueFrom.ConfigMapKeyRef == nil {
				continue
			}
			ref := e.ValueFrom.ConfigMapKeyRef
			if _, ok := have[ref.Name][ref.Key]; !ok {
				t.Fatalf("%s/%s references ConfigMap %s key %s that the base does not ship", c.Name, e.Name, ref.Name, ref.Key)
			}
		}
	}
	if have["goobers-cloud"]["database-auth"] != "entra" {
		t.Fatal("reference database-auth must be entra")
	}
}

const temporalHAChart = "GOOBERS_TEMPORAL_CHART_1_3_0" // path to the sha-pinned temporal-1.3.0.tgz

// TestDeployReferenceTemporalHAValuesRender renders values-ha.yaml with the
// pinned chart and asserts the effective configuration, so a key the chart
// ignores (as 0.x did for user and maxConns) fails here. The nightly workflow
// supplies the sha-verified chart; locally the test skips without it.
func TestDeployReferenceTemporalHAValuesRender(t *testing.T) {
	chart := os.Getenv(temporalHAChart)
	if chart == "" {
		t.Skipf("%s not set", temporalHAChart)
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	out, err := exec.Command(helm, "template", "temporal", chart, "--namespace", "goobers-temporal",
		"-f", "../../deploy/reference/temporal/values-ha.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	rendered := string(out)
	for _, want := range []string{"numHistoryShards: 512", "user: temporal_app", "maxConns: 12", "maxConns: 6", "maxIdleConns: 12", "unhealthyPodEvictionPolicy: AlwaysAllow"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered chart missing %q", want)
		}
	}
	if strings.Contains(rendered, "user: temporal\n") {
		t.Fatal("a datastore still uses the shared temporal role")
	}
	replicas := map[string]int{}
	pdbs := 0
	for _, doc := range strings.Split(rendered, "\n---") {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			continue
		}
		switch obj.Kind {
		case "PodDisruptionBudget":
			pdbs++
		case "Deployment":
			if obj.Spec.Replicas != nil {
				replicas[obj.Metadata.Labels["app.kubernetes.io/component"]] = *obj.Spec.Replicas
			}
		}
	}
	for svc, want := range map[string]int{"frontend": 3, "history": 3, "matching": 3, "worker": 2, "web": 2} {
		if replicas[svc] != want {
			t.Fatalf("%s replicas = %d, want %d (%v)", svc, replicas[svc], want, replicas)
		}
	}
	if pdbs != 5 {
		t.Fatalf("rendered %d PDBs, want 5", pdbs)
	}
}
