package dispatcher

import (
	"context"
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
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action kubetesting.Action) (bool, runtime.Object, error) {
		review := action.(kubetesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
}

func TestPreflightNamespacesAllReadyReportsNoError(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gaggle-alpha"}})
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
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gaggle-alpha"}})
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
