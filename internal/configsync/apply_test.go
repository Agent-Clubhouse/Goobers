package configsync

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/goobers/goobers/api/v1alpha1"
)

func managedGaggle(name string) *v1alpha1.Gaggle {
	g := &v1alpha1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
		Spec: v1alpha1.GaggleSpec{
			Project:   v1alpha1.RepoRef{Provider: v1alpha1.ProviderGitHub, Owner: "acme", Name: name},
			Backlog:   v1alpha1.BacklogRef{Provider: v1alpha1.ProviderGitHub, Project: "acme/" + name},
			Isolation: v1alpha1.GaggleIsolation{Namespace: "gaggle-" + name},
		},
	}
	g.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Gaggle"))
	return g
}

func managedGoober(name, gaggle string) *v1alpha1.Goober {
	g := &v1alpha1.Goober{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
		Spec: v1alpha1.GooberSpec{
			Gaggle:       gaggle,
			Role:         "coder",
			Instructions: "coder.md",
		},
	}
	g.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Goober"))
	return g
}

func managedManifest(name string, gaggles ...string) *v1alpha1.Manifest {
	m := &v1alpha1.Manifest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
		Spec: v1alpha1.ManifestSpec{
			Instance: v1alpha1.InstanceRef{Name: "acme", Environment: v1alpha1.EnvironmentDev},
			Gaggles:  gaggles,
		},
	}
	m.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Manifest"))
	return m
}

func managedWorkflow(name, gaggle string) *v1alpha1.Workflow {
	w := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
		Spec: v1alpha1.WorkflowSpec{Gaggle: gaggle},
	}
	w.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Workflow"))
	return w
}

func newApplier(t *testing.T, seed ...client.Object) (*ClientApplier, client.Client) {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).Build()
	return &ClientApplier{Client: c}, c
}

func TestClientApplier_CreatesDesired(t *testing.T) {
	a, c := newApplier(t)
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}
	generation := authoritative(t, c)
	if got, want := selectedGaggleNames(t, c, generation), []string{"web"}; !equalStrings(got, want) {
		t.Fatalf("selected gaggles = %v, want %v", got, want)
	}
}

func TestClientApplier_UpdatesExisting(t *testing.T) {
	existing := managedGaggle("web")
	existing.Spec.DisplayName = "old"
	a, c := newApplier(t, existing)

	updated := managedGaggle("web")
	updated.Spec.DisplayName = "new"
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{updated}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}
	generation := authoritative(t, c)
	g := selectedGaggle(t, c, generation, "web")
	if g.Spec.DisplayName != "new" {
		t.Errorf("displayName = %q, want new (update should overwrite)", g.Spec.DisplayName)
	}
}

func TestClientApplier_RewritesPublishedGaggleReferences(t *testing.T) {
	a, c := newApplier(t)
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{
		managedManifest("instance", "web"),
		managedGaggle("web"),
		managedGoober("coder", "web"),
		managedWorkflow("implement", "web"),
	}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}

	generation := authoritative(t, c)
	gaggle := selectedGaggle(t, c, generation, "web")
	var goobers v1alpha1.GooberList
	if err := c.List(context.Background(), &goobers,
		client.InNamespace(DefaultNamespace), GenerationSelector(generation),
	); err != nil {
		t.Fatalf("select goobers: %v", err)
	}
	if len(goobers.Items) != 1 {
		t.Fatalf("selected goobers = %d, want 1", len(goobers.Items))
	}
	if got := goobers.Items[0].Spec.Gaggle; got != gaggle.Name {
		t.Fatalf("published Goober gaggle ref = %q, want published Gaggle name %q", got, gaggle.Name)
	}
	var manifests v1alpha1.ManifestList
	if err := c.List(context.Background(), &manifests,
		client.InNamespace(DefaultNamespace), GenerationSelector(generation),
	); err != nil {
		t.Fatalf("select manifests: %v", err)
	}
	if len(manifests.Items) != 1 || len(manifests.Items[0].Spec.Gaggles) != 1 {
		t.Fatalf("selected manifests = %#v, want one manifest with one gaggle", manifests.Items)
	}
	if got := manifests.Items[0].Spec.Gaggles[0]; got != gaggle.Name {
		t.Fatalf("published Manifest gaggle ref = %q, want published Gaggle name %q", got, gaggle.Name)
	}
	var workflows v1alpha1.WorkflowList
	if err := c.List(context.Background(), &workflows,
		client.InNamespace(DefaultNamespace), GenerationSelector(generation),
	); err != nil {
		t.Fatalf("select workflows: %v", err)
	}
	if len(workflows.Items) != 1 {
		t.Fatalf("selected workflows = %d, want 1", len(workflows.Items))
	}
	if got := workflows.Items[0].Spec.Gaggle; got != gaggle.Name {
		t.Fatalf("published Workflow gaggle ref = %q, want published Gaggle name %q", got, gaggle.Name)
	}
}

func TestClientApplier_PrunesRemoved(t *testing.T) {
	// Two managed gaggles exist; the desired set contains only one -> prune the other.
	a, c := newApplier(t, managedGaggle("keep"), managedGaggle("remove"))
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("keep")}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}

	generation := authoritative(t, c)
	if got, want := selectedGaggleNames(t, c, generation), []string{"keep"}; !equalStrings(got, want) {
		t.Errorf("selected gaggles = %v, want %v", got, want)
	}
	var gone v1alpha1.Gaggle
	err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "remove"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("removed gaggle should be pruned, got err=%v", err)
	}
}

func TestClientApplier_PrunesOnlyReplacedGeneration(t *testing.T) {
	a, c := newApplier(t)
	if err := a.Apply(context.Background(), gaggleSet("stale")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	inFlight := managedGaggle("future")
	inFlight.Name = versionedObjectName("future", "gfuture")
	inFlight.Labels[GenerationLabel] = "gfuture"
	inFlight.Annotations = map[string]string{OriginalNameAnnotation: "future"}
	if err := c.Create(context.Background(), inFlight); err != nil {
		t.Fatalf("seed in-flight generation: %v", err)
	}

	if err := a.Apply(context.Background(), gaggleSet("web")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var future v1alpha1.Gaggle
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(inFlight), &future); err != nil {
		t.Fatalf("in-flight generation object was pruned: %v", err)
	}
}

func TestClientApplier_DoesNotPruneUnmanaged(t *testing.T) {
	// A gaggle without the managed-by label must never be pruned.
	unmanaged := managedGaggle("hand-rolled")
	unmanaged.Labels = nil
	a, c := newApplier(t, unmanaged)
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var g v1alpha1.Gaggle
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "hand-rolled"}, &g); err != nil {
		t.Errorf("unmanaged gaggle must not be pruned: %v", err)
	}
}

func TestNoopApplier(t *testing.T) {
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	if err := (NoopApplier{}).Apply(context.Background(), set); err != nil {
		t.Fatalf("noop apply: %v", err)
	}
}

type failDeleteClient struct {
	client.Client
	fail bool
}

func (f *failDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if f.fail {
		return apierrors.NewInternalError(context.DeadlineExceeded)
	}
	return f.Client.Delete(ctx, obj, opts...)
}

func TestClientApplier_RecoversFromPendingPruneOnNewGeneration(t *testing.T) {
	a, c := newApplier(t)
	ctx := context.Background()
	if err := a.Apply(ctx, gaggleSet("one")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	failing := &failDeleteClient{Client: c, fail: true}
	a.Client = failing
	if err := a.Apply(ctx, gaggleSet("two")); err == nil {
		t.Fatal("apply with failing prune should error")
	}
	if err := a.Apply(ctx, gaggleSet("three")); err == nil {
		t.Fatal("apply must still fail while prune keeps failing")
	}
	failing.fail = false
	if err := a.Apply(ctx, gaggleSet("three")); err != nil {
		t.Fatalf("apply after pending prune: %v", err)
	}
	generation := authoritative(t, c)
	if got, want := selectedGaggleNames(t, c, generation), []string{"three"}; !equalStrings(got, want) {
		t.Fatalf("selected gaggles = %v, want %v", got, want)
	}
	var all v1alpha1.GaggleList
	if err := c.List(ctx, &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 1 {
		t.Fatalf("expected only current generation objects, got %d", len(all.Items))
	}
}
