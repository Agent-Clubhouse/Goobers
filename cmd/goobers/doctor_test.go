package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/goobers/goobers/internal/k8spreflight"
)

// withFakeDoctorCluster substitutes the kubeconfig-backed client factory with
// a fake clientset shaped like a conformant cluster, restoring it after the
// test. The check logic itself is covered in internal/k8spreflight; these
// tests cover the CLI contract (flags, report formats, exit codes).
func withFakeDoctorCluster(t *testing.T) {
	t.Helper()
	client := fake.NewClientset(
		&storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "goobers-files"},
			Provisioner: "file.csi.azure.com",
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "api-server-egress", Namespace: "goobers-system", Labels: map[string]string{k8spreflight.APIServerEgressLabel: "true"}},
			Spec: networkingv1.NetworkPolicySpec{Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "127.0.0.1/32"}}},
			}}},
		},
	)
	discovery := client.Discovery().(*fakediscovery.FakeDiscovery)
	discovery.FakedServerVersion = &version.Info{Major: "1", Minor: "31", GitVersion: "v1.31.2"}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "networking.k8s.io/v1",
		APIResources: []metav1.APIResource{{Name: "networkpolicies", Kind: "NetworkPolicy"}},
	}}
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authorizationv1.SelfSubjectAccessReview{
				Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true},
			}, nil
		})

	orig := doctorKubeClient
	doctorKubeClient = func(string, string, time.Duration) (kubernetes.Interface, string, error) {
		return client, "https://127.0.0.1", nil
	}
	t.Cleanup(func() { doctorKubeClient = orig })
}

func TestDoctorRequiresK8sFlag(t *testing.T) {
	code, _, stderr := runArgs(t, "doctor")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--k8s") {
		t.Fatalf("stderr %q does not point at --k8s", stderr)
	}
}

func TestDoctorOverlayFlagsRejectIgnoredInputs(t *testing.T) {
	for _, args := range [][]string{
		{"doctor", "--repo", "--overlay-dir", "overlay"},
		{"doctor", "--av-exclusions", "--image-ca", "root.pem"},
		{"doctor", "--k8s", "--image-tools", "git"},
		{"doctor", "--k8s", "--overlay-dir", "overlay", "--image-runtime", "arbitrary-command"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, stderr := runArgs(t, args...)
			if code != 2 || !strings.Contains(stderr, "goobers doctor:") {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
		})
	}
}

func TestDoctorOverlayDirectoryReachesBothChecks(t *testing.T) {
	withFakeDoctorCluster(t)
	code, stdout, stderr := runArgs(t, "doctor", "--k8s", "--overlay-dir", t.TempDir(), "--report", "json")
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var report k8spreflight.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, result := range report.Results {
		if result.ID == "overlay-pin-agreement" || result.ID == "overlay-image-contract" {
			found++
			if result.Status != k8spreflight.StatusFail {
				t.Fatalf("empty overlay did not fail closed: %+v", result)
			}
		}
	}
	if found != 2 {
		t.Fatalf("overlay checks registered=%d", found)
	}
}

func TestDoctorRejectsBadReportFormat(t *testing.T) {
	code, _, stderr := runArgs(t, "doctor", "--k8s", "--report", "xml")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "text or json") {
		t.Fatalf("stderr %q does not explain the formats", stderr)
	}
}

func TestDoctorRejectsPositionalArgs(t *testing.T) {
	code, _, _ := runArgs(t, "doctor", "--k8s", "extra")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestDoctorKubeconfigErrorIsUsageError(t *testing.T) {
	orig := doctorKubeClient
	doctorKubeClient = func(string, string, time.Duration) (kubernetes.Interface, string, error) {
		return nil, "", errors.New("load kubeconfig: no configuration provided")
	}
	t.Cleanup(func() { doctorKubeClient = orig })

	code, _, stderr := runArgs(t, "doctor", "--k8s")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "load kubeconfig") {
		t.Fatalf("stderr %q does not carry the client error", stderr)
	}
}

func TestDoctorK8sTextReportConformant(t *testing.T) {
	withFakeDoctorCluster(t)

	code, stdout, _ := runArgs(t, "doctor", "--k8s")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout:\n%s", code, stdout)
	}
	for _, want := range []string{
		"target: https://127.0.0.1",
		"cluster-version",
		"storage-rwx",
		"cluster conforms",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report missing %q:\n%s", want, stdout)
		}
	}
}

func TestDoctorK8sJSONReportAndFailExitCode(t *testing.T) {
	withFakeDoctorCluster(t)

	// An unparseable egress target makes the required egress check fail, so
	// the JSON path and the nonzero-exit contract are covered together.
	code, stdout, _ := runArgs(t, "doctor", "--k8s", "--report", "json", "--egress", "no-port-here")
	if code != 1 {
		t.Fatalf("code = %d, want 1 on a required-check failure", code)
	}
	var report k8spreflight.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not the JSON report: %v\n%s", err, stdout)
	}
	if report.Conformant {
		t.Fatal("report.Conformant = true, want false")
	}
	if report.Target != "https://127.0.0.1" {
		t.Fatalf("report.Target = %q", report.Target)
	}
}

func TestDoctorCheckSelectionAndValidation(t *testing.T) {
	withFakeDoctorCluster(t)
	code, stdout, stderr := runArgs(t, "doctor", "--k8s", "--checks", "apiserver-ipblock-drift", "--report", "json")
	if code != 0 {
		t.Fatalf("selected check failed: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var report k8spreflight.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].ID != "apiserver-ipblock-drift" {
		t.Fatalf("selection ignored: %+v", report)
	}
	for _, args := range [][]string{{"doctor", "--k8s", "--checks", "typo"}, {"doctor", "--k8s", "--checks", "pod-health,pod-health"}, {"doctor", "--k8s", "--checks", ","}, {"doctor", "--repo", "--checks", "pod-health"}} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 {
			t.Fatalf("%v code=%d stderr=%s, want usage error", args, code, stderr)
		}
	}
}

func TestDoctorAPIServerComparisonOverrideDoesNotChangeClientEndpoint(t *testing.T) {
	withFakeDoctorCluster(t)
	existing := doctorKubeClient
	doctorKubeClient = func(config, context string, timeout time.Duration) (kubernetes.Interface, string, error) {
		client, _, err := existing(config, context, timeout)
		// An in-cluster client uses a Service VIP; the labeled policy allows the
		// actual control-plane endpoint after destination NAT instead.
		return client, "https://10.96.0.1", err
	}
	code, stdout, stderr := runArgs(t, "doctor", "--k8s", "--checks", "apiserver-ipblock-drift", "--apiserver-endpoint", "https://127.0.0.1", "--report", "json")
	if code != 0 {
		t.Fatalf("comparison override did not reach check: %d %s %s", code, stdout, stderr)
	}
	var report k8spreflight.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Target != "https://10.96.0.1" {
		t.Fatalf("comparison override changed client/report target: %+v", report)
	}
}
