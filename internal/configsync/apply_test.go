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
	var g v1alpha1.Gaggle
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "web"}, &g); err != nil {
		t.Fatalf("gaggle not created: %v", err)
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
	var g v1alpha1.Gaggle
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "web"}, &g)
	if g.Spec.DisplayName != "new" {
		t.Errorf("displayName = %q, want new (update should overwrite)", g.Spec.DisplayName)
	}
}

func TestClientApplier_PrunesRemoved(t *testing.T) {
	// Two managed gaggles exist; the desired set contains only one -> prune the other.
	a, c := newApplier(t, managedGaggle("keep"), managedGaggle("remove"))
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("keep")}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var kept v1alpha1.Gaggle
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "keep"}, &kept); err != nil {
		t.Errorf("kept gaggle should survive: %v", err)
	}
	var gone v1alpha1.Gaggle
	err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "remove"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("removed gaggle should be pruned, got err=%v", err)
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
