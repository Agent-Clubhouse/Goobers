package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// TestDispatcherRBACMatchesKubePodAPIVerbSet is #4286's drift guard:
// dispatcher-rbac.yaml's verb set must exactly match what
// internal/dispatcher/kubepods.go's kubePodAPI actually calls — the whole
// point of a narrow Role is that it stays narrow, not that it looked narrow
// once at authoring time. Parses the Role from the manifest and the client
// calls from the Go source, rather than hardcoding the expected verb list
// twice (in the manifest and in this test) with no link between them.
func TestDispatcherRBACMatchesKubePodAPIVerbSet(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "gaggle-namespace", "base", "dispatcher-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	for _, doc := range splitYAMLDocs(raw) {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "Role":
			if err := yaml.Unmarshal(doc, &role); err != nil {
				t.Fatal(err)
			}
		case "RoleBinding":
			if err := yaml.Unmarshal(doc, &binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	if role.Name == "" {
		t.Fatal("dispatcher-rbac.yaml has no Role")
	}
	if binding.Name == "" {
		t.Fatal("dispatcher-rbac.yaml has no RoleBinding")
	}

	source, err := os.ReadFile(filepath.Join("..", "..", "internal", "dispatcher", "kubepods.go"))
	if err != nil {
		t.Fatal(err)
	}

	// The exact four client-go pod calls kubepods.go makes today, and
	// nothing else — a fifth (e.g. Watch, GetLogs) appearing in the source
	// without a matching verb here should fail this test, not silently ship
	// under-permissioned RBAC.
	podCalls := map[string]*regexp.Regexp{
		"create": regexp.MustCompile(`Pods\([^)]*\)\.Create\(`),
		"get":    regexp.MustCompile(`Pods\([^)]*\)\.Get\(`),
		"delete": regexp.MustCompile(`Pods\([^)]*\)\.Delete\(`),
		"list":   regexp.MustCompile(`Pods\([^)]*\)\.List\(`),
	}
	for verb, pattern := range podCalls {
		if !pattern.Match(source) {
			t.Errorf("kubepods.go no longer calls Pods().%s — dispatcher-rbac.yaml grants %q but nothing uses it; narrow the Role", verb, verb)
		}
	}
	// No OTHER pod verbs (watch, patch, update, deletecollection) appear.
	otherPodCalls := regexp.MustCompile(`Pods\([^)]*\)\.(Watch|Patch|Update|DeleteCollection|Apply)\(`)
	if otherPodCalls.Match(source) {
		t.Fatal("kubepods.go now calls a pod verb this RBAC does not grant — update dispatcher-rbac.yaml's verb list")
	}
	deploymentCalls := regexp.MustCompile(`Deployments\([^)]*\)\.Get\(`)
	if !deploymentCalls.Match(source) {
		t.Error("kubepods.go no longer reads Deployments — dispatcher-rbac.yaml grants apps/deployments get but nothing uses it; narrow the Role")
	}
	otherDeploymentCalls := regexp.MustCompile(`Deployments\([^)]*\)\.(Create|Delete|List|Watch|Patch|Update|DeleteCollection|Apply)\(`)
	if otherDeploymentCalls.Match(source) {
		t.Fatal("kubepods.go now calls a deployments verb this RBAC does not grant — update dispatcher-rbac.yaml's verb list")
	}

	var podRule, deploymentRule *rbacv1.PolicyRule
	for i := range role.Rules {
		rule := &role.Rules[i]
		for _, apiGroup := range rule.APIGroups {
			switch {
			case apiGroup == "" && containsString(rule.Resources, "pods"):
				podRule = rule
			case apiGroup == "apps" && containsString(rule.Resources, "deployments"):
				deploymentRule = rule
			}
		}
	}
	if podRule == nil {
		t.Fatal("dispatcher-rbac.yaml Role has no core/pods rule")
	}
	wantPodVerbs := []string{"create", "get", "delete", "list"}
	for _, verb := range wantPodVerbs {
		if !containsString(podRule.Verbs, verb) {
			t.Errorf("pods rule verbs = %v, missing %q (kubepods.go calls it)", podRule.Verbs, verb)
		}
	}
	if len(podRule.Verbs) != len(wantPodVerbs) {
		t.Errorf("pods rule verbs = %v, want exactly %v — no wider than kubepods.go's own calls", podRule.Verbs, wantPodVerbs)
	}
	if deploymentRule == nil {
		t.Fatal("dispatcher-rbac.yaml Role has no apps/deployments rule")
	}
	if len(deploymentRule.Verbs) != 1 || deploymentRule.Verbs[0] != "get" {
		t.Errorf("deployments rule verbs = %v, want exactly [get]", deploymentRule.Verbs)
	}

	// The binding must target the EXISTING goobers-worker ServiceAccount from
	// goobers-system — never an invented identity (the live-proven trap
	// dispatcher-rbac.yaml's own comment documents).
	if len(binding.Subjects) != 1 {
		t.Fatalf("RoleBinding subjects = %+v, want exactly one", binding.Subjects)
	}
	subject := binding.Subjects[0]
	if subject.Kind != "ServiceAccount" || subject.Name != "goobers-worker" || subject.Namespace != "goobers-system" {
		t.Errorf("RoleBinding subject = %+v, want ServiceAccount goobers-worker in goobers-system", subject)
	}

	workerRBACRaw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "goobers-system", "worker-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workerSA corev1.ServiceAccount
	for _, doc := range splitYAMLDocs(workerRBACRaw) {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind == "ServiceAccount" {
			if err := yaml.Unmarshal(doc, &workerSA); err != nil {
				t.Fatal(err)
			}
		}
	}
	if workerSA.Name != subject.Name {
		t.Errorf("dispatcher-rbac.yaml binds ServiceAccount %q, but goobers-system/worker-rbac.yaml ships %q", subject.Name, workerSA.Name)
	}
}

// TestDeployReferenceWorkerSetsDispatchNamespace is #4286's second acceptance
// criterion: the worker Deployment must set --dispatch-namespace (or the
// test itself would need to assert a documented CHANGE-ME — it is set, so
// this asserts the flag is present and paired with the dispatcher RBAC).
func TestDeployReferenceWorkerSetsDispatchNamespace(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "goobers-system", "worker-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(deployment.Spec.Template.Spec.Containers))
	}
	args := deployment.Spec.Template.Spec.Containers[0].Args
	found := false
	for i, arg := range args {
		if arg == "--dispatch-namespace" {
			found = true
			if i+1 >= len(args) || args[i+1] == "" {
				t.Fatal("--dispatch-namespace has no value")
			}
		}
	}
	if !found {
		t.Fatal("worker-deployment.yaml does not set --dispatch-namespace — mode 3 (pod-per-stage) is not enabled")
	}
}

// TestDeployReferenceAPIMountsSharedBlobStore is #3839's regression guard:
// the daemon's SpanSource reads a hardcoded <instance-root>/blobstore
// (internal/instance/instance.go's BlobStoreDir, joined onto the api
// container's own --instance root arg) and the worker's --blob-store
// writes worker-executed agentic stages' spans into a SEPARATE PVC
// (goobers-blobs) the daemon never mounted. Both sides must resolve to the
// SAME underlying PersistentVolumeClaim, or the daemon's projection reports
// span_unavailable for every worker-executed stage forever, regardless of
// how correctly each side is individually wired.
func TestDeployReferenceAPIMountsSharedBlobStore(t *testing.T) {
	apiRaw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "goobers-system", "api-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var api appsv1.Deployment
	if err := yaml.Unmarshal(apiRaw, &api); err != nil {
		t.Fatal(err)
	}
	if len(api.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("api-deployment.yaml: got %d containers, want 1", len(api.Spec.Template.Spec.Containers))
	}
	apiContainer := api.Spec.Template.Spec.Containers[0]
	if len(apiContainer.Args) < 2 {
		t.Fatalf("api-deployment.yaml args = %v, want at least [up, <instance-root>]", apiContainer.Args)
	}
	instanceRoot := apiContainer.Args[len(apiContainer.Args)-1]
	wantBlobMount := filepath.Join(instanceRoot, "blobstore")

	var blobsClaim string
	volumeClaims := make(map[string]string, len(api.Spec.Template.Spec.Volumes))
	for _, volume := range api.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			volumeClaims[volume.Name] = volume.PersistentVolumeClaim.ClaimName
		}
	}
	var apiMountFound bool
	for _, mount := range apiContainer.VolumeMounts {
		if mount.MountPath == wantBlobMount {
			apiMountFound = true
			blobsClaim = volumeClaims[mount.Name]
		}
	}
	if !apiMountFound {
		t.Fatalf("api-deployment.yaml mounts no volume at %q (the daemon's own BlobStoreDir()) — worker-executed agentic stage spans will report span_unavailable", wantBlobMount)
	}
	if blobsClaim == "" {
		t.Fatalf("api-deployment.yaml mounts %q from a volume with no PersistentVolumeClaim", wantBlobMount)
	}

	workerRaw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "goobers-system", "worker-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var worker appsv1.Deployment
	if err := yaml.Unmarshal(workerRaw, &worker); err != nil {
		t.Fatal(err)
	}
	if len(worker.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("worker-deployment.yaml: got %d containers, want 1", len(worker.Spec.Template.Spec.Containers))
	}
	workerContainer := worker.Spec.Template.Spec.Containers[0]
	var blobStoreArg string
	for i, arg := range workerContainer.Args {
		if arg == "--blob-store" && i+1 < len(workerContainer.Args) {
			blobStoreArg = workerContainer.Args[i+1]
		}
	}
	if blobStoreArg == "" {
		t.Fatal("worker-deployment.yaml does not set --blob-store")
	}
	workerVolumeClaims := make(map[string]string, len(worker.Spec.Template.Spec.Volumes))
	for _, volume := range worker.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			workerVolumeClaims[volume.Name] = volume.PersistentVolumeClaim.ClaimName
		}
	}
	var workerBlobsClaim string
	for _, mount := range workerContainer.VolumeMounts {
		if mount.MountPath == blobStoreArg {
			workerBlobsClaim = workerVolumeClaims[mount.Name]
		}
	}
	if workerBlobsClaim == "" {
		t.Fatalf("worker-deployment.yaml's --blob-store %q is not mounted from any PersistentVolumeClaim", blobStoreArg)
	}
	if workerBlobsClaim != blobsClaim {
		t.Fatalf("worker --blob-store resolves to PVC %q, but the daemon's BlobStoreDir() (%q) resolves to PVC %q — the daemon cannot read worker-recorded spans (#3839)",
			workerBlobsClaim, wantBlobMount, blobsClaim)
	}
}

// TestDeployReferenceTemporalNamespaceJobIsIdempotentAndOrdered is #4287's
// regression guard: the Job must be safe to reapply (idempotent) and its
// registered namespace must match the one the worker actually connects to,
// or the Job silently registers the wrong namespace while the worker keeps
// failing.
func TestDeployReferenceTemporalNamespaceJobIsIdempotentAndOrdered(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "temporal", "namespace-job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("namespace-job.yaml restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(job.Spec.Template.Spec.Containers))
	}
	container := job.Spec.Template.Spec.Containers[0]
	script := ""
	for _, arg := range container.Args {
		script += arg
	}
	if !regexp.MustCompile(`namespace describe`).MatchString(script) {
		t.Error("namespace-job.yaml does not describe-before-register — it is not idempotent")
	}
	if !regexp.MustCompile(`namespace register`).MatchString(script) {
		t.Error("namespace-job.yaml never registers the namespace")
	}

	var namespaceEnv string
	for _, env := range container.Env {
		if env.Name == "TEMPORAL_NAMESPACE" {
			namespaceEnv = env.Value
		}
	}
	if namespaceEnv == "" {
		t.Fatal("namespace-job.yaml does not set TEMPORAL_NAMESPACE")
	}

	workerRaw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "goobers-system", "worker-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var worker appsv1.Deployment
	if err := yaml.Unmarshal(workerRaw, &worker); err != nil {
		t.Fatal(err)
	}
	if len(worker.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(worker.Spec.Template.Spec.Containers))
	}
	var workerNamesTemporalNamespace bool
	for i, arg := range worker.Spec.Template.Spec.Containers[0].Args {
		if arg == "--temporal-namespace" && i+1 < len(worker.Spec.Template.Spec.Containers[0].Args) {
			workerNamesTemporalNamespace = true
			if worker.Spec.Template.Spec.Containers[0].Args[i+1] != namespaceEnv {
				t.Errorf("worker --temporal-namespace = %q, want it to match namespace-job.yaml's TEMPORAL_NAMESPACE %q",
					worker.Spec.Template.Spec.Containers[0].Args[i+1], namespaceEnv)
			}
		}
	}
	// The worker doesn't set --temporal-namespace explicitly in the reference
	// (it falls back to internal/instance/config.go's DefaultTemporalNamespace,
	// "default") — in that case the Job's namespace must be exactly "default".
	if !workerNamesTemporalNamespace && namespaceEnv != "default" {
		t.Errorf(`worker-deployment.yaml does not set --temporal-namespace (falls back to "default"), but namespace-job.yaml registers %q instead`, namespaceEnv)
	}
}
