package dispatcher

import (
	"context"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/utils/ptr"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

// allowAllSelfSubjectAccessReviews makes the fake clientset's
// SelfSubjectAccessReview answer Allowed for every review — the fake's
// default reactor otherwise leaves Status.Allowed false, which reads as
// "every grant is missing" regardless of what was actually asked.
func allowAllSelfSubjectAccessReviews(client *fake.Clientset) {
	client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "32", GitVersion: "v1.32.0"}
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action kubetesting.Action) (bool, runtime.Object, error) {
		review := action.(kubetesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
}

func TestPreflightNamespacesAllReadyReportsNoError(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gaggle-alpha"}}, stageAccount("gaggle-alpha", "goobers-stage", ptr.To(false)))
	allowAllSelfSubjectAccessReviews(client)
	results, err := PreflightNamespaces(context.Background(), client, map[string]string{"alpha": "gaggle-alpha"})
	if err != nil {
		t.Fatalf("PreflightNamespaces: %v", err)
	}
	if len(results) != 1 || !results[0].Ready() {
		t.Fatalf("results = %+v, want one ready namespace", results)
	}
}

func TestPreflightNamespacesFailsOnMissingNamespace(t *testing.T) {
	client := fake.NewClientset()
	allowAllSelfSubjectAccessReviews(client)
	results, err := PreflightNamespaces(context.Background(), client, map[string]string{"alpha": "does-not-exist"})
	if err == nil {
		t.Fatal("PreflightNamespaces accepted a namespace that does not exist")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error %v does not name the missing namespace", err)
	}
	if len(results) != 1 || results[0].Exists || results[0].Ready() {
		t.Fatalf("results = %+v, want the missing namespace reported not-exists/not-ready", results)
	}
}

func TestPreflightNamespacesFailsOnMissingRBACGrant(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gaggle-alpha"}}, stageAccount("gaggle-alpha", "goobers-stage", ptr.To(false)))
	client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "32", GitVersion: "v1.32.0"}
	// The fake clientset has no real object-tracker support for this
	// subresource type, so a review must be answered explicitly rather than
	// relying on an unconfigured default.
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action kubetesting.Action) (bool, runtime.Object, error) {
		review := action.(kubetesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = false
		return true, review, nil
	})
	results, err := PreflightNamespaces(context.Background(), client, map[string]string{"alpha": "gaggle-alpha"})
	if err == nil {
		t.Fatal("PreflightNamespaces accepted a namespace with no RBAC grants")
	}
	if !strings.Contains(err.Error(), "gaggle-alpha") {
		t.Fatalf("error %v does not name the namespace missing grants", err)
	}
	if len(results) != 1 || !results[0].Exists || len(results[0].MissingGrants) == 0 || results[0].Ready() {
		t.Fatalf("results = %+v, want the namespace reported exists but not ready, naming missing grants", results)
	}
}

func TestPreflightNamespacesChecksEveryDistinctNamespaceOnce(t *testing.T) {
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gaggle-alpha"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gaggle-beta"}},
		stageAccount("gaggle-alpha", "goobers-stage", ptr.To(false)),
		stageAccount("gaggle-beta", "goobers-stage", ptr.To(false)),
	)
	allowAllSelfSubjectAccessReviews(client)
	results, err := PreflightNamespaces(context.Background(), client, map[string]string{
		"alpha": "gaggle-alpha",
		"beta":  "gaggle-beta",
		// A third gaggle explicitly sharing alpha's namespace must not be
		// checked twice.
		"gamma": "gaggle-alpha",
	})
	if err != nil {
		t.Fatalf("PreflightNamespaces: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want exactly the 2 DISTINCT namespaces checked once each", results)
	}
	for _, r := range results {
		if !r.Ready() {
			t.Fatalf("namespace %s not ready: %+v", r.Namespace, r)
		}
	}
}

func stageAccount(namespace, name string, automount *bool) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, AutomountServiceAccountToken: automount}
}

func TestPreflightStageAccountsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		automount *bool
		missing   bool
		wantError bool
	}{
		{"missing", nil, true, true}, {"unset", nil, false, true}, {"enabled", ptr.To(true), false, true}, {"disabled", ptr.To(false), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared"}}, stageAccount("shared", "goobers-stage", ptr.To(false)))
			if !tc.missing {
				_, err := client.CoreV1().ServiceAccounts("shared").Create(context.Background(), stageAccount("shared", "custom", tc.automount), metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			allowAllSelfSubjectAccessReviews(client)
			results, err := PreflightNamespaces(context.Background(), client, map[string]string{"alpha": "shared", "beta": "shared"}, map[string]string{"beta": "custom"})
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			if len(results) != 1 || len(results[0].ServiceAccounts) != 2 || results[0].Ready() == tc.wantError {
				t.Fatalf("results = %+v", results)
			}
			if err != nil && (!strings.Contains(err.Error(), "STAGE_SERVICE_ACCOUNT") || !strings.Contains(err.Error(), "custom")) {
				t.Fatalf("unhelpful diagnostic: %v", err)
			}
			if tc.missing && !strings.Contains(err.Error(), "deploy/reference/gaggle-namespace/base/serviceaccount.yaml") {
				t.Fatal(err)
			}
		})
	}
}

func TestPreflightPodOSVersionFloor(t *testing.T) {
	for _, minor := range []string{"24", "25", "32+", "unknown"} {
		t.Run(minor, func(t *testing.T) {
			client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}, stageAccount("ns", "goobers-stage", ptr.To(false)))
			allowAllSelfSubjectAccessReviews(client)
			client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: minor, GitVersion: "v1." + minor}
			results, err := PreflightNamespaces(context.Background(), client, map[string]string{"g": "ns"})
			rejected := minor == "24" || minor == "unknown"
			if (err != nil) != rejected || results[0].ServerVersion != "v1."+minor {
				t.Fatalf("results=%+v, err=%v", results, err)
			}
			if rejected && !strings.Contains(err.Error(), "K8S_POD_OS_VERSION") {
				t.Fatal(err)
			}
		})
	}
}
