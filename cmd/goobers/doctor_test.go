package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
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
		return client, "https://fake-cluster.example.com", nil
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
		"target: https://fake-cluster.example.com",
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
	if report.Target != "https://fake-cluster.example.com" {
		t.Fatalf("report.Target = %q", report.Target)
	}
}
