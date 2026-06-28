package operator

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/goobers/goobers/api/v1alpha1"
)

// --- pure helpers ----------------------------------------------------------

func TestDesiredWorkerDeployment(t *testing.T) {
	g := &v1alpha1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec:       v1alpha1.GaggleSpec{Isolation: v1alpha1.GaggleIsolation{Namespace: "gaggle-web"}},
	}
	gb := &v1alpha1.Goober{
		ObjectMeta: metav1.ObjectMeta{Name: "coder"},
		Spec:       v1alpha1.GooberSpec{Gaggle: "web", Role: "coder", ScaleFactor: 3},
	}
	dep := desiredWorkerDeployment(g, gb, "img:1")

	if dep.Name != "goober-coder" {
		t.Errorf("name = %q, want goober-coder", dep.Name)
	}
	if dep.Namespace != "gaggle-web" {
		t.Errorf("namespace = %q, want gaggle-web", dep.Namespace)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want 3", dep.Spec.Replicas)
	}
	if dep.Labels[gaggleLabel] != "web" || dep.Labels[gooberLabel] != "coder" || dep.Labels[managedByLabel] != managedByValue {
		t.Errorf("labels missing/incorrect: %v", dep.Labels)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "img:1" {
		t.Errorf("image = %q, want img:1", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["GOOBER_GAGGLE"] != "web" || env["GOOBER_ROLE"] != "coder" || env["GOOBER_NAME"] != "coder" {
		t.Errorf("env not wired: %v", env)
	}
}

func TestDesiredWorkerDeployment_DefaultsReplicasToOne(t *testing.T) {
	g := &v1alpha1.Gaggle{Spec: v1alpha1.GaggleSpec{Isolation: v1alpha1.GaggleIsolation{Namespace: "ns"}}}
	gb := &v1alpha1.Goober{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Spec: v1alpha1.GooberSpec{ScaleFactor: 0}}
	dep := desiredWorkerDeployment(g, gb, "img")
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1 (default)", dep.Spec.Replicas)
	}
}

func TestDeploymentReady(t *testing.T) {
	three := int32(3)
	cases := []struct {
		name     string
		replicas *int32
		avail    int32
		want     bool
	}{
		{"nil replicas, 1 avail", nil, 1, true},
		{"nil replicas, 0 avail", nil, 0, false},
		{"3 desired, 3 avail", &three, 3, true},
		{"3 desired, 2 avail", &three, 2, false},
	}
	for _, tc := range cases {
		d := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: tc.replicas}, Status: appsv1.DeploymentStatus{AvailableReplicas: tc.avail}}
		if got := deploymentReady(d); got != tc.want {
			t.Errorf("%s: deploymentReady = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWorkflowNames_DedupAndSort(t *testing.T) {
	goobers := []v1alpha1.Goober{
		{Spec: v1alpha1.GooberSpec{Workflows: []string{"impl", "review"}}},
		{Spec: v1alpha1.GooberSpec{Workflows: []string{"review", "audit"}}},
		{Spec: v1alpha1.GooberSpec{}},
	}
	got := workflowNames(goobers)
	want := []string{"audit", "impl", "review"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestGaggleNamespaceObject(t *testing.T) {
	g := &v1alpha1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: v1alpha1.GaggleSpec{Isolation: v1alpha1.GaggleIsolation{Namespace: "gaggle-web"}}}
	ns := gaggleNamespaceObject(g)
	if ns.Name != "gaggle-web" {
		t.Errorf("ns name = %q", ns.Name)
	}
	if ns.Labels[gaggleLabel] != "web" {
		t.Errorf("ns missing gaggle label: %v", ns.Labels)
	}
}

func TestComputeStatus(t *testing.T) {
	ready := computeStatus(5, 2, 2, v1alpha1.GaggleStatus{})
	if ready.Phase != v1alpha1.GagglePhaseReady || ready.ObservedGeneration != 5 || ready.GooberCount != 2 || ready.ReadyWorkers != 2 {
		t.Errorf("ready status wrong: %+v", ready)
	}
	if c := apimeta.FindStatusCondition(ready.Conditions, readyConditionType); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ready condition not True: %+v", ready.Conditions)
	}

	degraded := computeStatus(6, 3, 1, ready)
	if degraded.Phase != v1alpha1.GagglePhaseDegraded {
		t.Errorf("phase = %q, want Degraded", degraded.Phase)
	}
	if c := apimeta.FindStatusCondition(degraded.Conditions, readyConditionType); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "WorkersNotReady" {
		t.Errorf("degraded condition wrong: %+v", degraded.Conditions)
	}
}

func TestStatusEqual(t *testing.T) {
	a := computeStatus(1, 1, 1, v1alpha1.GaggleStatus{})
	b := computeStatus(1, 1, 1, a)
	if !statusEqual(a, b) {
		t.Error("identical statuses should be equal (ignoring timestamps)")
	}
	c := computeStatus(2, 1, 1, a)
	if statusEqual(a, c) {
		t.Error("different generation should not be equal")
	}
	if statusEqual(a, v1alpha1.GaggleStatus{}) {
		t.Error("populated vs empty should differ (nil condition)")
	}
}

func TestMapFuncs(t *testing.T) {
	reqs := gooberToGaggle(context.Background(), &v1alpha1.Goober{
		ObjectMeta: metav1.ObjectMeta{Namespace: "goobers-system"},
		Spec:       v1alpha1.GooberSpec{Gaggle: "web"},
	})
	if len(reqs) != 1 || reqs[0].Name != "web" || reqs[0].Namespace != "goobers-system" {
		t.Errorf("gooberToGaggle = %+v", reqs)
	}
	if got := gooberToGaggle(context.Background(), &v1alpha1.Goober{}); got != nil {
		t.Errorf("goober with no gaggle should map to nil, got %+v", got)
	}

	// A worker lives in the isolation namespace (gaggle-web) but must map back to
	// the Gaggle CR's own namespace (goobers-system), recorded on the worker.
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "gaggle-web",
		Labels:    map[string]string{managedByLabel: managedByValue, gaggleLabel: "web", gaggleNamespaceLabel: "goobers-system"},
	}}
	reqs = workerToGaggle(context.Background(), dep)
	if len(reqs) != 1 || reqs[0].Name != "web" || reqs[0].Namespace != "goobers-system" {
		t.Errorf("workerToGaggle = %+v, want web/goobers-system (Gaggle CR ns, not isolation ns)", reqs)
	}
	// Missing the gaggle-namespace label => cannot resolve the Gaggle CR => nil.
	noNS := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "gaggle-web", Labels: map[string]string{managedByLabel: managedByValue, gaggleLabel: "web"}}}
	if got := workerToGaggle(context.Background(), noNS); got != nil {
		t.Errorf("worker without gaggle-namespace label should map to nil, got %+v", got)
	}
	unmanaged := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"foo": "bar"}}}
	if got := workerToGaggle(context.Background(), unmanaged); got != nil {
		t.Errorf("unmanaged deployment should map to nil, got %+v", got)
	}
}

func TestReconcile_GoobersAreNamespaceScoped(t *testing.T) {
	// A same-named goober in a DIFFERENT namespace must not be pulled into this
	// gaggle (bug: cross-namespace selection). The gaggle CR is in goobers-system.
	foreign := gooberFixture("coder", "web", 5)
	foreign.Namespace = "other-tenant"
	r, c := newReconciler(t, nil, gaggleFixture(), gooberFixture("coder", "web", 1), foreign)
	reconcileOnce(t, r)

	var g v1alpha1.Gaggle
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "goobers-system", Name: "web"}, &g)
	if g.Status.GooberCount != 1 {
		t.Fatalf("GooberCount = %d, want 1 (foreign-namespace goober must be excluded)", g.Status.GooberCount)
	}
	// Exactly one worker, built from the in-namespace goober (scaleFactor 1, not 5).
	var deps appsv1.DeploymentList
	_ = c.List(context.Background(), &deps, client.InNamespace("gaggle-web"))
	if len(deps.Items) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(deps.Items))
	}
	if *deps.Items[0].Spec.Replicas != 1 {
		t.Errorf("worker replicas = %d, want 1 (from in-namespace goober, not foreign scale 5)", *deps.Items[0].Spec.Replicas)
	}
	// The worker must record the Gaggle CR namespace for event mapping.
	if deps.Items[0].Labels[gaggleNamespaceLabel] != "goobers-system" {
		t.Errorf("worker missing/incorrect gaggle-namespace label: %v", deps.Items[0].Labels)
	}
}

// --- reconcile (fake client) ----------------------------------------------

func newReconciler(t *testing.T, reg WorkflowRegistrar, objs ...client.Object) (*GaggleReconciler, client.Client) {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Gaggle{}).
		Build()
	if reg == nil {
		reg = NoopRegistrar{}
	}
	return &GaggleReconciler{Client: c, Scheme: scheme, Registrar: reg, WorkerImage: "test:img"}, c
}

func gaggleFixture() *v1alpha1.Gaggle {
	return &v1alpha1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "goobers-system", Generation: 1},
		Spec: v1alpha1.GaggleSpec{
			Project:   v1alpha1.RepoRef{Provider: v1alpha1.ProviderGitHub, Owner: "acme", Name: "web"},
			Backlog:   v1alpha1.BacklogRef{Provider: v1alpha1.ProviderGitHub, Project: "acme/web"},
			Isolation: v1alpha1.GaggleIsolation{Namespace: "gaggle-web"},
		},
	}
}

func gooberFixture(name, gaggle string, scale int32) *v1alpha1.Goober {
	return &v1alpha1.Goober{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "goobers-system"},
		Spec:       v1alpha1.GooberSpec{Gaggle: gaggle, Role: name, Instructions: "x.md", ScaleFactor: scale, Workflows: []string{"impl"}},
	}
}

func reconcileOnce(t *testing.T, r *GaggleReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "goobers-system", Name: "web"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestReconcile_CreatesNamespaceAndWorker(t *testing.T) {
	r, c := newReconciler(t, nil, gaggleFixture(), gooberFixture("coder", "web", 2))
	reconcileOnce(t, r)

	var ns corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gaggle-web"}, &ns); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gaggle-web", Name: "goober-coder"}, &dep); err != nil {
		t.Fatalf("worker deployment not created: %v", err)
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *dep.Spec.Replicas)
	}

	var g v1alpha1.Gaggle
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "goobers-system", Name: "web"}, &g)
	if g.Status.GooberCount != 1 || g.Status.ReadyWorkers != 0 {
		t.Errorf("status counts = %d/%d, want 1/0", g.Status.ReadyWorkers, g.Status.GooberCount)
	}
	if g.Status.Phase != v1alpha1.GagglePhaseDegraded {
		t.Errorf("phase = %q, want Degraded (worker not yet available)", g.Status.Phase)
	}
	if g.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", g.Status.ObservedGeneration)
	}
}

func TestReconcile_ReadyWhenWorkerAvailable(t *testing.T) {
	// Pre-seed the worker Deployment as fully available so the reconcile sees Ready.
	one := int32(1)
	ready := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "goober-coder", Namespace: "gaggle-web",
			Labels: map[string]string{managedByLabel: managedByValue, gaggleLabel: "web", gooberLabel: "coder"},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	r, c := newReconciler(t, nil, gaggleFixture(), gooberFixture("coder", "web", 1), ready)
	reconcileOnce(t, r)

	var g v1alpha1.Gaggle
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "goobers-system", Name: "web"}, &g)
	if g.Status.Phase != v1alpha1.GagglePhaseReady || g.Status.ReadyWorkers != 1 {
		t.Errorf("status = %q ready=%d, want Ready/1", g.Status.Phase, g.Status.ReadyWorkers)
	}
	if cnd := apimeta.FindStatusCondition(g.Status.Conditions, readyConditionType); cnd == nil || cnd.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition not True: %+v", g.Status.Conditions)
	}
}

func TestReconcile_PrunesOrphanWorker(t *testing.T) {
	// An orphan worker (its goober no longer exists) must be deleted.
	orphan := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "goober-gone", Namespace: "gaggle-web",
			Labels: map[string]string{managedByLabel: managedByValue, gaggleLabel: "web", gooberLabel: "gone"},
		},
	}
	r, c := newReconciler(t, nil, gaggleFixture(), gooberFixture("coder", "web", 1), orphan)
	reconcileOnce(t, r)

	var dep appsv1.Deployment
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "gaggle-web", Name: "goober-gone"}, &dep)
	if !apierrors.IsNotFound(err) {
		t.Errorf("orphan worker should be pruned, got err=%v", err)
	}
}

func TestReconcile_GaggleNotFound(t *testing.T) {
	r, _ := newReconciler(t, nil) // no gaggle
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "goobers-system", Name: "web"}}); err != nil {
		t.Errorf("missing gaggle should not error, got %v", err)
	}
}

type failingRegistrar struct{ called bool }

func (f *failingRegistrar) EnsureRegistered(context.Context, string, []string) error {
	f.called = true
	return errors.New("engine down")
}

func TestReconcile_RegistrarErrorIsNonFatal(t *testing.T) {
	reg := &failingRegistrar{}
	r, c := newReconciler(t, reg, gaggleFixture(), gooberFixture("coder", "web", 1))
	reconcileOnce(t, r) // must not return error despite registrar failure

	if !reg.called {
		t.Error("registrar was not invoked")
	}
	var g v1alpha1.Gaggle
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "goobers-system", Name: "web"}, &g)
	if g.Status.GooberCount != 1 {
		t.Errorf("status should still be updated despite registrar error, got %+v", g.Status)
	}
}

func TestReconcile_Idempotent(t *testing.T) {
	r, c := newReconciler(t, nil, gaggleFixture(), gooberFixture("coder", "web", 1))
	reconcileOnce(t, r)
	reconcileOnce(t, r) // second pass must be stable

	var deps appsv1.DeploymentList
	if err := c.List(context.Background(), &deps, client.InNamespace("gaggle-web")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(deps.Items) != 1 {
		t.Errorf("expected exactly 1 worker after 2 reconciles, got %d", len(deps.Items))
	}
}
