package k8spreflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeCluster returns a fake clientset shaped like an otherwise conformant
// cluster: current version, NetworkPolicy API served, an inferred RWX-capable
// StorageClass, and every SelfSubjectAccessReview allowed.
func newFakeCluster(t *testing.T) *fake.Clientset {
	t.Helper()
	client := fake.NewClientset(
		&storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "goobers-files"},
			Provisioner: "file.csi.azure.com",
		},
	)
	discovery, ok := client.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatal("fake clientset discovery is not a FakeDiscovery")
	}
	discovery.FakedServerVersion = &version.Info{Major: "1", Minor: "31", GitVersion: "v1.31.2"}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "networking.k8s.io/v1",
		APIResources: []metav1.APIResource{{Name: "networkpolicies", Kind: "NetworkPolicy"}},
	}}
	allowSelfSubjectAccessReviews(client, true)
	return client
}

func allowSelfSubjectAccessReviews(client *fake.Clientset, allowed bool) {
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authorizationv1.SelfSubjectAccessReview{
				Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed},
			}, nil
		})
}

func resultByID(t *testing.T, report Report, id string) Result {
	t.Helper()
	for _, result := range report.Results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("report has no %q check; got %+v", id, report.Results)
	return Result{}
}

func TestRunConformantClusterPasses(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "https://issuer.example.com"})
	}))
	defer issuer.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A private registry answers /v2/ with 401 — that still proves
		// reachability.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	egress, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = egress.Close() }()

	otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/traces", "/v1/metrics", "/v1/logs":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer otlp.Close()

	report := Run(context.Background(), newFakeCluster(t), Options{
		OIDCIssuer:   issuer.URL,
		Registry:     registry.URL,
		Egress:       []string{egress.Addr().String()},
		OTLPEndpoint: otlp.URL,
	})

	if !report.Conformant {
		t.Fatalf("conformant cluster reported non-conformant: %+v", report.Results)
	}
	for _, result := range report.Results {
		want := StatusPass
		switch result.ID {
		case "storage-rwx", "networkpolicy-api":
			// storage-rwx: inferred, never a hard pass (§4). networkpolicy-api:
			// API-discovery only — a served API is a correlate of enforcement,
			// not proof of it, so even an "otherwise conformant" cluster warns
			// here until an in-cluster negative control runs.
			want = StatusWarn
		case "runner-class-capacity", "pod-container-health":
			want = StatusWarn
		}
		if result.Status != want {
			t.Errorf("check %s = %s (%s), want %s", result.ID, result.Status, result.Detail, want)
		}
	}
}

func TestRunnerClassCapacityDetectsAllocatableMismatch(t *testing.T) {
	client := fake.NewClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("3860m"),
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "runner-1", Namespace: "default", Labels: map[string]string{"goobers.dev/runner-class": "linux-large"}},
			Spec: corev1.PodSpec{
				NodeName: "node-a",
				Containers: []corev1.Container{{
					Name: "stage",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("4000m"),
					}},
				}},
			},
		},
	)
	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "runner-class-capacity")
	if result.Status != StatusFail {
		t.Fatalf("runner-class-capacity = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "4000m") || !strings.Contains(result.Detail, "3860m") {
		t.Fatalf("detail %q does not report the over-capacity mismatch", result.Detail)
	}
}

func TestRunnerClassCapacityAggregatesAllRunnerPodsOnNode(t *testing.T) {
	client := fake.NewClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4000m"),
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "runner-1", Namespace: "default", Labels: map[string]string{"goobers.dev/runner-class": "linux-large"}},
			Spec: corev1.PodSpec{
				NodeName: "node-a",
				Containers: []corev1.Container{{
					Name: "stage",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2500m"),
					}},
				}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "runner-2", Namespace: "default", Labels: map[string]string{"goobers.dev/runner-class": "linux-large"}},
			Spec: corev1.PodSpec{
				NodeName: "node-a",
				Containers: []corev1.Container{{
					Name: "stage",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2500m"),
					}},
				}},
			},
		},
	)
	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "runner-class-capacity")
	if result.Status != StatusFail {
		t.Fatalf("runner-class-capacity = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "5000m") || !strings.Contains(result.Detail, "4000m") {
		t.Fatalf("detail %q does not aggregate the combined runner-class demand on the node", result.Detail)
	}
}

func TestPodHealthFlagsCrashLoopingSidecar(t *testing.T) {
	client := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "sidecar", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, RestartCount: 7},
		}},
	})
	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "pod-container-health")
	if result.Status != StatusFail {
		t.Fatalf("pod-container-health = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "CrashLoopBackOff") {
		t.Fatalf("detail %q does not report the crash-looping sidecar", result.Detail)
	}
}

func TestOTLPSignalSetRejectsMissingMetricsPipeline(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/traces":
			w.WriteHeader(http.StatusOK)
		case "/v1/metrics":
			http.Error(w, "no metrics pipeline configured", http.StatusBadRequest)
		case "/v1/logs":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer collector.Close()

	report := Run(context.Background(), newFakeCluster(t), Options{OTLPEndpoint: collector.URL})
	result := resultByID(t, report, "otlp-signal-set")
	if result.Status != StatusFail {
		t.Fatalf("otlp-signal-set = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "metrics") {
		t.Fatalf("detail %q does not mention the rejected metrics signal", result.Detail)
	}
}

func TestRunUnreachableClusterFailsClosed(t *testing.T) {
	client := newFakeCluster(t)
	client.PrependReactor("get", "version",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "cluster-version")
	if result.Status != StatusFail {
		t.Fatalf("cluster-version = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "connection refused") {
		t.Fatalf("detail %q does not carry the reason", result.Detail)
	}
	if report.Conformant {
		t.Fatal("unreachable cluster must not be conformant")
	}
}

func TestOldClusterVersionWarns(t *testing.T) {
	client := newFakeCluster(t)
	discovery := client.Discovery().(*fakediscovery.FakeDiscovery)
	discovery.FakedServerVersion = &version.Info{Major: "1", Minor: "24", GitVersion: "v1.24.0"}

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "cluster-version")
	if result.Status != StatusWarn {
		t.Fatalf("cluster-version = %s, want warn", result.Status)
	}
	// A warn is not a required-check failure: the report stays conformant.
	if !report.Conformant {
		t.Fatal("old-but-reachable version must warn, not break conformance")
	}
}

func TestRBACDeniedFails(t *testing.T) {
	client := newFakeCluster(t)
	allowSelfSubjectAccessReviews(client, false) // prepended, wins over the allow

	report := Run(context.Background(), client, Options{})
	for _, id := range []string{"rbac-install", "rbac-gaggle"} {
		result := resultByID(t, report, id)
		if result.Status != StatusFail {
			t.Errorf("%s = %s, want fail", id, result.Status)
		}
		if !strings.Contains(result.Detail, "denied:") {
			t.Errorf("%s detail %q does not list denials", id, result.Detail)
		}
	}
	if report.Conformant {
		t.Fatal("denied install permissions must not be conformant")
	}
}

func TestRBACProbeErrorFailsClosed(t *testing.T) {
	client := newFakeCluster(t)
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("selfsubjectaccessreviews is forbidden")
		})

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "rbac-install")
	if result.Status != StatusFail {
		t.Fatalf("rbac-install = %s, want fail (fail-closed on unverifiable)", result.Status)
	}
	if !strings.Contains(result.Detail, "unable to verify") {
		t.Fatalf("detail %q does not say the check could not run", result.Detail)
	}
}

func TestStorageWithoutRWXClassRecommendsSingleNodeRWO(t *testing.T) {
	client := newFakeCluster(t)
	// Replace the RWX class with a block-only one — RWO-only is the
	// documented safe topology (§4), so this must not fail.
	if err := client.StorageV1().StorageClasses().Delete(context.Background(), "goobers-files", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "managed-disk"},
		Provisioner: "disk.csi.azure.com",
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "storage-rwx")
	if result.Status != StatusPass {
		t.Fatalf("storage-rwx = %s, want pass", result.Status)
	}
	if !strings.Contains(result.Hint, "single node") {
		t.Fatalf("hint %q does not recommend single-node RWO mounting", result.Hint)
	}
	if !report.Conformant {
		t.Fatal("RWO-only storage is the recommended safe topology and must be conformant (§4)")
	}
}

func TestStorageWithNoClassesFails(t *testing.T) {
	client := newFakeCluster(t)
	if err := client.StorageV1().StorageClasses().Delete(context.Background(), "goobers-files", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "storage-rwx")
	if result.Status != StatusFail {
		t.Fatalf("storage-rwx = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "no StorageClasses") {
		t.Fatalf("detail %q does not name the absence of StorageClasses", result.Detail)
	}
	if result.Hint == "" {
		t.Fatal("storage failure must carry a remediation hint")
	}
	if report.Conformant {
		t.Fatal("no StorageClasses at all must not be conformant (§4)")
	}
}

func TestStorageInferredRWXWarnsAboutCoordinationSafety(t *testing.T) {
	report := Run(context.Background(), newFakeCluster(t), Options{})
	result := resultByID(t, report, "storage-rwx")

	if result.Status != StatusWarn || result.Severity != SeverityRequired {
		t.Fatalf("storage-rwx = %s/%s, want required warn", result.Status, result.Severity)
	}
	for _, caveat := range []string{"flock", "SQLite WAL"} {
		if !strings.Contains(result.Detail, caveat) {
			t.Errorf("detail %q does not name %s safety", result.Detail, caveat)
		}
	}
	if !strings.Contains(result.Hint, "RWO") || !strings.Contains(result.Hint, "single node") {
		t.Errorf("hint %q does not recommend safe storage topology", result.Hint)
	}
	if !report.Conformant {
		t.Fatal("inferred RWX capability must warn, not break conformance")
	}
}

func TestMixedOSPlacementRejectsUntaintedWindowsNode(t *testing.T) {
	client := newFakeCluster(t)
	if _, err := client.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "windows-1",
			Labels: map[string]string{corev1.LabelOSStable: "windows"},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "mixed-os-placement")
	if result.Status != StatusFail {
		t.Fatalf("mixed-os-placement = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "windows-1") || !strings.Contains(result.Detail, "NoSchedule") {
		t.Fatalf("detail %q does not identify the unsafe Windows node", result.Detail)
	}
	if report.Conformant {
		t.Fatal("an untainted Windows node must make the cluster non-conformant")
	}
}

func TestMixedOSPlacementRejectsUnpinnedControlPlaneWorkload(t *testing.T) {
	client := newFakeCluster(t)
	if _, err := client.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "windows-1",
			Labels: map[string]string{corev1.LabelOSStable: "windows"},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: corev1.LabelOSStable, Value: "windows", Effect: corev1.TaintEffectNoSchedule,
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments(controlPlaneNamespace).Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "goobers-api", Namespace: controlPlaneNamespace},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "mixed-os-placement")
	if result.Status != StatusFail {
		t.Fatalf("mixed-os-placement = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "Deployment/goobers-api") {
		t.Fatalf("detail %q does not identify the unpinned workload", result.Detail)
	}
}

func TestMixedOSPlacementRejectsUnpinnedTemporalJob(t *testing.T) {
	client := newFakeCluster(t)
	if _, err := client.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "windows-1",
			Labels: map[string]string{corev1.LabelOSStable: "windows"},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: corev1.LabelOSStable, Value: "windows", Effect: corev1.TaintEffectNoSchedule,
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BatchV1().Jobs(temporalNamespace).Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "temporal-schema-setup", Namespace: temporalNamespace},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "mixed-os-placement")
	if result.Status != StatusFail {
		t.Fatalf("mixed-os-placement = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "goobers-temporal/Job/temporal-schema-setup") {
		t.Fatalf("detail %q does not identify the unpinned Temporal job", result.Detail)
	}
}

func TestMixedOSPlacementAcceptsTaintedNodesAndPinnedWorkloads(t *testing.T) {
	client := newFakeCluster(t)
	if _, err := client.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "windows-1",
			Labels: map[string]string{corev1.LabelOSStable: "windows"},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: corev1.LabelOSStable, Value: "windows", Effect: corev1.TaintEffectNoSchedule,
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments(controlPlaneNamespace).Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "goobers-api", Namespace: controlPlaneNamespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{NodeSelector: map[string]string{corev1.LabelOSStable: "linux"}},
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "mixed-os-placement")
	if result.Status != StatusPass {
		t.Fatalf("mixed-os-placement = %s (%s), want pass", result.Status, result.Detail)
	}
	if !report.Conformant {
		t.Fatal("tainted Windows nodes and pinned Linux workloads must be conformant")
	}
}

func TestNetworkPolicyAPINotServedFails(t *testing.T) {
	client := newFakeCluster(t)
	client.Discovery().(*fakediscovery.FakeDiscovery).Resources = nil

	report := Run(context.Background(), client, Options{})
	result := resultByID(t, report, "networkpolicy-api")
	if result.Status != StatusFail {
		t.Fatalf("networkpolicy-api = %s, want fail", result.Status)
	}
}

// TestNetworkPolicyAPIServedWarnsNotProof pins #3516: a served
// networking.k8s.io/v1 API is only a correlate of enforcement — a CNI can
// serve the API and still ignore policies silently — so this read-only check
// must warn, never pass, and must say so rather than claim enforcement.
func TestNetworkPolicyAPIServedWarnsNotProof(t *testing.T) {
	report := Run(context.Background(), newFakeCluster(t), Options{})
	result := resultByID(t, report, "networkpolicy-api")
	if result.Status != StatusWarn {
		t.Fatalf("networkpolicy-api = %s, want warn when the API is served (enforcement is unverified by this check)", result.Status)
	}
	if strings.Contains(result.Title, "deny-first defaults enforceable") {
		t.Errorf("networkpolicy-api title %q still claims enforcement is proven", result.Title)
	}
	if !strings.Contains(result.Detail, "unverified") {
		t.Errorf("networkpolicy-api detail %q does not say enforcement is unverified", result.Detail)
	}
	if !strings.Contains(result.Hint, "correlate") || !strings.Contains(result.Hint, "denied attempt") {
		t.Errorf("networkpolicy-api hint %q does not explain a served API is only a correlate proven by a denied attempt", result.Hint)
	}
	if !report.Conformant {
		t.Fatal("a networkpolicy-api warn must not flip an otherwise-conformant report")
	}
}

func TestSkippedProbesWarnWithoutBlocking(t *testing.T) {
	report := Run(context.Background(), newFakeCluster(t), Options{})
	for _, id := range []string{"oidc-issuer", "registry", "egress"} {
		result := resultByID(t, report, id)
		if result.Status != StatusWarn {
			t.Errorf("%s = %s, want warn when unconfigured", id, result.Status)
		}
		if result.Severity != SeverityOptional {
			t.Errorf("%s severity = %s, want optional when unconfigured", id, result.Severity)
		}
		if !strings.Contains(result.Detail, "skipped") {
			t.Errorf("%s detail %q does not say skipped", id, result.Detail)
		}
	}
	if !report.Conformant {
		t.Fatal("skipped optional probes must not break conformance")
	}
}

func TestOIDCIssuerUnreachableFails(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	issuer.Close() // reachable URL shape, refused connection

	report := Run(context.Background(), newFakeCluster(t), Options{OIDCIssuer: issuer.URL})
	result := resultByID(t, report, "oidc-issuer")
	if result.Status != StatusFail || result.Severity != SeverityRequired {
		t.Fatalf("oidc-issuer = %s/%s, want required fail", result.Status, result.Severity)
	}
	if report.Conformant {
		t.Fatal("unreachable configured issuer must not be conformant")
	}
}

func TestOIDCIssuerInvalidDocumentFails(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer issuer.Close()

	report := Run(context.Background(), newFakeCluster(t), Options{OIDCIssuer: issuer.URL})
	result := resultByID(t, report, "oidc-issuer")
	if result.Status != StatusFail {
		t.Fatalf("oidc-issuer = %s, want fail on a non-OIDC document", result.Status)
	}
}

func TestRegistryUnreachableWarnsOnly(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	registry.Close()

	report := Run(context.Background(), newFakeCluster(t), Options{Registry: registry.URL})
	result := resultByID(t, report, "registry")
	if result.Status != StatusWarn {
		t.Fatalf("registry = %s, want warn (host-side sanity only)", result.Status)
	}
	if !report.Conformant {
		t.Fatal("registry reachability is host-side sanity — it must not block conformance")
	}
}

func TestEgressUnreachableTargetFails(t *testing.T) {
	// A listener opened then closed yields a port that refuses connections.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := listener.Addr().String()
	_ = listener.Close()

	report := Run(context.Background(), newFakeCluster(t), Options{
		Egress: []string{closedAddr, "bad-target-without-port"},
	})
	result := resultByID(t, report, "egress")
	if result.Status != StatusFail || result.Severity != SeverityRequired {
		t.Fatalf("egress = %s/%s, want required fail", result.Status, result.Severity)
	}
	if !strings.Contains(result.Detail, closedAddr) || !strings.Contains(result.Detail, "want host:port") {
		t.Fatalf("detail %q does not list both failure modes", result.Detail)
	}
	if report.Conformant {
		t.Fatal("unreachable required egress must not be conformant")
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	report := Run(context.Background(), newFakeCluster(t), Options{})
	report.Target = "https://cluster.example.com"

	var buf bytes.Buffer
	if err := WriteJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("emitted JSON does not parse: %v", err)
	}
	if decoded.Target != report.Target || len(decoded.Results) != len(report.Results) {
		t.Fatalf("round-trip mismatch: %+v", decoded)
	}
	if !decoded.Conformant {
		t.Fatal("decoded report lost conformance")
	}
}

func TestWriteTextRendersRowsHintsAndVerdict(t *testing.T) {
	report := Run(context.Background(), newFakeCluster(t), Options{
		Egress: []string{"bad-target-without-port"},
	})
	report.Target = "https://cluster.example.com"

	var buf bytes.Buffer
	WriteText(&buf, report)
	out := buf.String()

	for _, want := range []string{
		"target: https://cluster.example.com",
		"cluster-version",
		"storage-rwx",
		"FAIL",
		"remediation:",
		"does NOT conform",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q:\n%s", want, out)
		}
	}
}
