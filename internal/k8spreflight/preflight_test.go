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

	authorizationv1 "k8s.io/api/authorization/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeCluster returns a fake clientset shaped like a conformant cluster:
// current version, NetworkPolicy API served, an RWX-capable StorageClass, and
// every SelfSubjectAccessReview allowed.
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

	report := Run(context.Background(), newFakeCluster(t), Options{
		OIDCIssuer: issuer.URL,
		Registry:   registry.URL,
		Egress:     []string{egress.Addr().String()},
	})

	if !report.Conformant {
		t.Fatalf("conformant cluster reported non-conformant: %+v", report.Results)
	}
	for _, result := range report.Results {
		if result.Status != StatusPass {
			t.Errorf("check %s = %s (%s), want pass", result.ID, result.Status, result.Detail)
		}
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

func TestStorageWithoutRWXClassFails(t *testing.T) {
	client := newFakeCluster(t)
	// Replace the RWX class with a block-only one.
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
	if result.Status != StatusFail {
		t.Fatalf("storage-rwx = %s, want fail", result.Status)
	}
	if !strings.Contains(result.Detail, "managed-disk") {
		t.Fatalf("detail %q does not name the non-RWX classes", result.Detail)
	}
	if result.Hint == "" {
		t.Fatal("storage failure must carry a remediation hint")
	}
	if report.Conformant {
		t.Fatal("no RWX storage must not be conformant (§4)")
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
