package configsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

func newInterceptedApplier(t *testing.T, funcs interceptor.Funcs, seed ...client.Object) (*ClientApplier, client.Client) {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).WithInterceptorFuncs(funcs).Build()
	return &ClientApplier{Client: c}, c
}

func applyErr(t *testing.T, err error) *ApplyError {
	t.Helper()
	if err == nil {
		t.Fatal("apply: want error, got nil")
	}
	var ae *ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("apply error = %T (%v), want *ApplyError", err, err)
	}
	return ae
}

func authoritative(t *testing.T, c client.Client) string {
	t.Helper()
	gen, err := AuthoritativeGeneration(context.Background(), c, DefaultNamespace)
	if err != nil {
		t.Fatalf("authoritative generation: %v", err)
	}
	return gen
}

func TestClientApplier_PublishesOneGeneration(t *testing.T) {
	a, c := newApplier(t)
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web"), managedGaggle("api")}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}

	want, err := GenerationID(set)
	if err != nil {
		t.Fatalf("generation id: %v", err)
	}
	if got := authoritative(t, c); got != want {
		t.Errorf("authoritative generation = %q, want %q", got, want)
	}
	// The RenderSet the caller handed us must not be mutated by stamping.
	for _, obj := range set.Objects {
		if _, ok := obj.GetLabels()[GenerationLabel]; ok {
			t.Errorf("caller object %s was stamped in place", objKey(obj))
		}
	}

	selector, err := GenerationSelector(context.Background(), c, DefaultNamespace)
	if err != nil {
		t.Fatalf("generation selector: %v", err)
	}
	var list v1alpha1.GaggleList
	if err := c.List(context.Background(), &list, client.InNamespace(DefaultNamespace), selector); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("selected %d gaggles, want 2", len(list.Items))
	}
	for _, item := range list.Items {
		if got := item.Labels[GenerationLabel]; got != want {
			t.Errorf("gaggle %s generation = %q, want %q", item.Name, got, want)
		}
	}
}

func TestGenerationSelector_UnpublishedIsNotSelectable(t *testing.T) {
	_, c := newApplier(t, managedGaggle("web"))
	if _, err := GenerationSelector(context.Background(), c, DefaultNamespace); !errors.Is(err, ErrNoAuthoritativeGeneration) {
		t.Fatalf("selector err = %v, want ErrNoAuthoritativeGeneration", err)
	}
}

func TestClientApplier_ReapplyIsIdempotent(t *testing.T) {
	a, c := newApplier(t)
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := authoritative(t, c)
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second := authoritative(t, c); second != first {
		t.Errorf("generation = %q on re-apply, want stable %q", second, first)
	}
}

func TestClientApplier_ApplyFailureKeepsPreviousGeneration(t *testing.T) {
	a, c := newApplier(t, managedGaggle("stale"))
	published := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	if err := a.Apply(context.Background(), published); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	previous := authoritative(t, c)

	failing, c := newInterceptedApplier(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == "api" {
				return apierrors.NewTimeoutError("apply timed out", 1)
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, managedGaggle("stale"), managedGaggle("web"))
	if err := seedGeneration(c, previous); err != nil {
		t.Fatalf("seed generation: %v", err)
	}

	next := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web"), managedGaggle("api")}}
	ae := applyErr(t, failing.Apply(context.Background(), next))
	if ae.Phase != PhaseApply {
		t.Errorf("phase = %q, want %q", ae.Phase, PhaseApply)
	}
	if ae.Authoritative != previous {
		t.Errorf("authoritative = %q, want previous %q", ae.Authoritative, previous)
	}
	if got := authoritative(t, c); got != previous {
		t.Errorf("published generation = %q, want unchanged %q", got, previous)
	}
	if len(ae.Committed()) != 1 || ae.Committed()[0].Key != objKey(managedGaggle("web")) {
		t.Errorf("committed = %v, want the web update", ae.Committed())
	}
	if len(ae.Ambiguous()) != 1 || ae.Ambiguous()[0].Op != OpCreate {
		t.Errorf("ambiguous = %v, want the api create", ae.Ambiguous())
	}
	if !strings.Contains(ae.Error(), "ambiguous") {
		t.Errorf("error %q does not report ambiguous mutations", ae.Error())
	}
	var stale v1alpha1.Gaggle
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "stale"}, &stale); err != nil {
		t.Errorf("prune must not run after a failed apply: %v", err)
	}
}

func TestClientApplier_ValidationFailureBlocksSwitchAndPrune(t *testing.T) {
	// An update that silently does not land leaves the object on its old
	// generation; validation must catch that before anything is published.
	a, c := newInterceptedApplier(t, interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return nil
		},
	}, managedGaggle("web"), managedGaggle("stale"))

	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	ae := applyErr(t, a.Apply(context.Background(), set))
	if ae.Phase != PhaseValidate {
		t.Errorf("phase = %q, want %q", ae.Phase, PhaseValidate)
	}
	if ae.Authoritative != "" {
		t.Errorf("authoritative = %q, want none", ae.Authoritative)
	}
	if got := authoritative(t, c); got != "" {
		t.Errorf("published generation = %q, want none", got)
	}
	var stale v1alpha1.Gaggle
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "stale"}, &stale); err != nil {
		t.Errorf("prune must not run before the switch: %v", err)
	}
}

func TestClientApplier_SwitchFailureKeepsPreviousGeneration(t *testing.T) {
	a, c := newInterceptedApplier(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == GenerationConfigMapName {
				return apierrors.NewTimeoutError("switch timed out", 1)
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, managedGaggle("stale"))

	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	ae := applyErr(t, a.Apply(context.Background(), set))
	if ae.Phase != PhaseSwitch {
		t.Errorf("phase = %q, want %q", ae.Phase, PhaseSwitch)
	}
	if got := authoritative(t, c); got != "" {
		t.Errorf("published generation = %q, want none", got)
	}
	if len(ae.Ambiguous()) != 1 || ae.Ambiguous()[0].Key != generationRef(DefaultNamespace) {
		t.Errorf("ambiguous = %v, want the generation switch", ae.Ambiguous())
	}
	var stale v1alpha1.Gaggle
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: "stale"}, &stale); err != nil {
		t.Errorf("prune must not run when the switch fails: %v", err)
	}
}

func TestClientApplier_PruneFailureAfterSwitchReportsMutations(t *testing.T) {
	a, c := newInterceptedApplier(t, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return apierrors.NewTimeoutError("prune timed out", 1)
		},
	}, managedGaggle("stale"))

	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	ae := applyErr(t, a.Apply(context.Background(), set))
	if ae.Phase != PhasePrune {
		t.Errorf("phase = %q, want %q", ae.Phase, PhasePrune)
	}
	if ae.Authoritative != ae.Generation {
		t.Errorf("authoritative = %q, want the new generation %q", ae.Authoritative, ae.Generation)
	}
	if got := authoritative(t, c); got != ae.Generation {
		t.Errorf("published generation = %q, want %q", got, ae.Generation)
	}
	if len(ae.Ambiguous()) != 1 || ae.Ambiguous()[0].Op != OpDelete {
		t.Errorf("ambiguous = %v, want the stale delete", ae.Ambiguous())
	}
	if len(ae.Committed()) != 2 {
		t.Errorf("committed = %v, want the web create and the generation switch", ae.Committed())
	}
}

func seedGeneration(c client.Client, generation string) error {
	return c.Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: GenerationConfigMapName, Namespace: DefaultNamespace},
		Data:       map[string]string{GenerationKey: generation},
	})
}
