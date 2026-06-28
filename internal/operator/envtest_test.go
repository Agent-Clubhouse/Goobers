package operator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/goobers/goobers/api/v1alpha1"
)

// TestEnvtestReconcile runs the reconciler against a real (envtest) API server:
// create CR -> reconcile drives desired state + updates status (M9 acceptance).
// It self-skips when KUBEBUILDER_ASSETS is unset so plain `make ci` stays
// portable; run it via `make test-envtest` (which provisions the assets).
func TestEnvtestReconcile(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make test-envtest` to provision envtest binaries")
	}

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()

	// The control-plane namespace the CRs live in must exist first.
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "goobers-system"}}); err != nil {
		t.Fatalf("create goobers-system ns: %v", err)
	}

	// Gaggle + Goober CRs in the control-plane namespace.
	gaggle := gaggleFixture()
	if err := k8s.Create(ctx, gaggle); err != nil {
		t.Fatalf("create gaggle: %v", err)
	}
	if err := k8s.Create(ctx, gooberFixture("coder", "web", 2)); err != nil {
		t.Fatalf("create goober: %v", err)
	}

	r := &GaggleReconciler{Client: k8s, Scheme: scheme, Registrar: NoopRegistrar{}, WorkerImage: "test:img"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "goobers-system", Name: "web"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Namespace created.
	if err := waitGet(ctx, k8s, types.NamespacedName{Name: "gaggle-web"}, &corev1.Namespace{}); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	// Worker deployment created with the right replicas.
	var dep appsv1.Deployment
	if err := waitGet(ctx, k8s, types.NamespacedName{Namespace: "gaggle-web", Name: "goober-coder"}, &dep); err != nil {
		t.Fatalf("worker not created: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2", dep.Spec.Replicas)
	}
	// Status persisted via the status subresource.
	var got v1alpha1.Gaggle
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: "goobers-system", Name: "web"}, &got); err != nil {
		t.Fatalf("get gaggle: %v", err)
	}
	if got.Status.GooberCount != 1 || got.Status.ObservedGeneration == 0 {
		t.Errorf("status not updated: %+v", got.Status)
	}

	// Deleting the Goober and re-reconciling prunes its worker.
	if err := k8s.Delete(ctx, gooberFixture("coder", "web", 2)); err != nil {
		t.Fatalf("delete goober: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "goobers-system", Name: "web"}}); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	var pruned appsv1.Deployment
	err = k8s.Get(ctx, types.NamespacedName{Namespace: "gaggle-web", Name: "goober-coder"}, &pruned)
	if err == nil {
		t.Error("worker should have been pruned after goober deletion")
	}
}

// waitGet polls Get for up to ~2s to absorb apiserver propagation latency.
func waitGet(ctx context.Context, c client.Client, key types.NamespacedName, obj client.Object) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = c.Get(ctx, key, obj); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}
