package configsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/goobers/goobers/api/v1alpha1"
)

func newInterceptedApplier(t *testing.T, funcs interceptor.Funcs, seed ...client.Object) (*ClientApplier, client.Client) {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).WithInterceptorFuncs(funcs).Build()
	return &ClientApplier{Client: c}, c
}

func gaggleSet(names ...string) *RenderSet {
	objs := make([]client.Object, 0, len(names))
	for _, name := range names {
		objs = append(objs, managedGaggle(name))
	}
	return &RenderSet{Namespace: DefaultNamespace, Objects: objs}
}

func authoritative(t *testing.T, c client.Client) string {
	t.Helper()
	gen, err := AuthoritativeGeneration(context.Background(), c, DefaultNamespace)
	if err != nil {
		t.Fatalf("authoritative generation: %v", err)
	}
	return gen
}

// managedGenerations returns the distinct generation labels of every managed
// Gaggle in the namespace.
func managedGenerations(t *testing.T, c client.Client) map[string]int {
	t.Helper()
	var list v1alpha1.GaggleList
	if err := c.List(context.Background(), &list,
		client.InNamespace(DefaultNamespace),
		client.MatchingLabels{ManagedByLabel: ManagedByValue},
	); err != nil {
		t.Fatalf("list gaggles: %v", err)
	}
	seen := map[string]int{}
	for i := range list.Items {
		seen[list.Items[i].Labels[GenerationLabel]]++
	}
	return seen
}

func applyErrorOf(t *testing.T, err error) *ApplyError {
	t.Helper()
	var applyErr *ApplyError
	if !errors.As(err, &applyErr) {
		t.Fatalf("error %v is not an *ApplyError", err)
	}
	return applyErr
}

func mutationFor(mutations []Mutation, object string) (Mutation, bool) {
	for _, m := range mutations {
		if m.Object == object {
			return m, true
		}
	}
	return Mutation{}, false
}

func TestClientApplier_PublishesOneGeneration(t *testing.T) {
	a, c := newApplier(t)
	if err := a.Apply(context.Background(), gaggleSet("web", "api")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	generation := authoritative(t, c)
	if generation == "" {
		t.Fatal("authoritative generation not published")
	}
	gens := managedGenerations(t, c)
	if len(gens) != 1 || gens[generation] != 2 {
		t.Fatalf("generations = %v, want 2 objects at %s", gens, generation)
	}

	var selected v1alpha1.GaggleList
	if err := c.List(context.Background(), &selected,
		client.InNamespace(DefaultNamespace), GenerationSelector(generation),
	); err != nil {
		t.Fatalf("select generation: %v", err)
	}
	if len(selected.Items) != 2 {
		t.Fatalf("generation selector returned %d objects, want 2", len(selected.Items))
	}
}

func TestClientApplier_GenerationIsContentIdentity(t *testing.T) {
	a, c := newApplier(t)
	if err := a.Apply(context.Background(), gaggleSet("web")); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := authoritative(t, c)

	if err := a.Apply(context.Background(), gaggleSet("web")); err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	if got := authoritative(t, c); got != first {
		t.Errorf("generation = %s after unchanged re-apply, want %s", got, first)
	}

	changed := gaggleSet("web")
	changed.Objects[0].(*v1alpha1.Gaggle).Spec.DisplayName = "changed"
	if err := a.Apply(context.Background(), changed); err != nil {
		t.Fatalf("changed apply: %v", err)
	}
	second := authoritative(t, c)
	if second == first {
		t.Error("changed config reused the previous generation")
	}
	if gens := managedGenerations(t, c); len(gens) != 1 || gens[second] != 1 {
		t.Errorf("generations = %v, want only %s", gens, second)
	}
}

func TestClientApplier_ApplyFailureKeepsPreviousGeneration(t *testing.T) {
	a, c := newApplier(t, managedGaggle("web"))
	if err := a.Apply(context.Background(), gaggleSet("web")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	previous := authoritative(t, c)

	failing, failingClient := newInterceptedApplier(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == "api" {
				return apierrors.NewTimeoutError("create timed out", 1)
			}
			return c.Create(ctx, obj, opts...)
		},
	}, seedManaged(t, c)...)

	err := failing.Apply(context.Background(), gaggleSet("web", "api", "worker"))
	if err == nil {
		t.Fatal("apply should fail when a desired object cannot be committed")
	}
	applyErr := applyErrorOf(t, err)
	if applyErr.Phase != "apply" {
		t.Errorf("phase = %q, want apply", applyErr.Phase)
	}
	if m, ok := mutationFor(applyErr.Mutations, "Gaggle/"+DefaultNamespace+"/web"); !ok || m.Status != MutationCommitted {
		t.Errorf("web mutation = %v (found %t), want committed", m, ok)
	}
	if m, ok := mutationFor(applyErr.Mutations, "Gaggle/"+DefaultNamespace+"/api"); !ok || m.Status != MutationAmbiguous {
		t.Errorf("api mutation = %v (found %t), want ambiguous", m, ok)
	}
	if got := authoritative(t, failingClient); got != previous {
		t.Errorf("authoritative generation = %s after failed apply, want unchanged %s", got, previous)
	}
}

func TestClientApplier_IncompleteGenerationDoesNotBecomeAuthoritative(t *testing.T) {
	// A write that silently drops the generation stamp must be caught by
	// validation, leaving the previous generation authoritative.
	a, c := newApplier(t)
	if err := a.Apply(context.Background(), gaggleSet("web")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	previous := authoritative(t, c)

	failing, failingClient := newInterceptedApplier(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == "web" {
				labels := obj.GetLabels()
				delete(labels, GenerationLabel)
				obj.SetLabels(labels)
			}
			return c.Update(ctx, obj, opts...)
		},
	}, seedManaged(t, c)...)

	changed := gaggleSet("web")
	changed.Objects[0].(*v1alpha1.Gaggle).Spec.DisplayName = "changed"
	err := failing.Apply(context.Background(), changed)
	if err == nil {
		t.Fatal("apply should fail when the generation is incomplete")
	}
	if phase := applyErrorOf(t, err).Phase; phase != "validate" {
		t.Errorf("phase = %q, want validate", phase)
	}
	if got := authoritative(t, failingClient); got != previous {
		t.Errorf("authoritative generation = %s, want unchanged %s", got, previous)
	}
}

func TestClientApplier_SwitchFailureLeavesPreviousGenerationAndSkipsPrune(t *testing.T) {
	a, c := newApplier(t, managedGaggle("stale"))
	if err := a.Apply(context.Background(), gaggleSet("stale")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	previous := authoritative(t, c)

	failing, failingClient := newInterceptedApplier(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return apierrors.NewTimeoutError("pointer switch timed out", 1)
			}
			return c.Update(ctx, obj, opts...)
		},
	}, seedManaged(t, c)...)

	err := failing.Apply(context.Background(), gaggleSet("web"))
	if err == nil {
		t.Fatal("apply should fail when the authoritative switch fails")
	}
	applyErr := applyErrorOf(t, err)
	if applyErr.Phase != "switch" {
		t.Errorf("phase = %q, want switch", applyErr.Phase)
	}
	if m, ok := mutationFor(applyErr.Mutations, "ConfigMap/"+DefaultNamespace+"/"+GenerationConfigMapName); !ok || m.Status != MutationAmbiguous {
		t.Errorf("pointer mutation = %v (found %t), want ambiguous", m, ok)
	}
	if got := authoritative(t, failingClient); got != previous {
		t.Errorf("authoritative generation = %s, want unchanged %s", got, previous)
	}
	var stale v1alpha1.Gaggle
	if err := failingClient.Get(context.Background(),
		types.NamespacedName{Namespace: DefaultNamespace, Name: "stale"}, &stale); err != nil {
		t.Errorf("prune must not run before the switch commits: %v", err)
	}
}

func TestClientApplier_PruneBeginsOnlyAfterSwitch(t *testing.T) {
	a, c := newApplier(t, managedGaggle("stale"))
	if err := a.Apply(context.Background(), gaggleSet("stale")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	previous := authoritative(t, c)

	var atDelete string
	failing, failingClient := newInterceptedApplier(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var pointer corev1.ConfigMap
			if err := c.Get(ctx, types.NamespacedName{
				Namespace: DefaultNamespace, Name: GenerationConfigMapName,
			}, &pointer); err != nil {
				return err
			}
			atDelete = pointer.Data[GenerationConfigMapKey]
			return apierrors.NewTimeoutError("delete timed out", 1)
		},
	}, seedManaged(t, c)...)

	err := failing.Apply(context.Background(), gaggleSet("web"))
	if err == nil {
		t.Fatal("apply should fail when a prune delete fails")
	}
	applyErr := applyErrorOf(t, err)
	if applyErr.Phase != "prune" {
		t.Errorf("phase = %q, want prune", applyErr.Phase)
	}
	published := authoritative(t, failingClient)
	if published == previous {
		t.Errorf("authoritative generation = %s, want the new generation published before prune", published)
	}
	if atDelete != published {
		t.Errorf("generation at delete = %q, want the new authoritative %q", atDelete, published)
	}
	if m, ok := mutationFor(applyErr.Mutations, "Gaggle/"+DefaultNamespace+"/stale"); !ok || m.Status != MutationAmbiguous {
		t.Errorf("prune mutation = %v (found %t), want ambiguous", m, ok)
	}
}

func TestClientApplier_DoesNotStampCallerObjects(t *testing.T) {
	a, _ := newApplier(t)
	set := gaggleSet("web")
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := set.Objects[0].GetLabels()[GenerationLabel]; ok {
		t.Error("apply must not mutate the caller's render set")
	}
}

func TestMutationStatus(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     MutationStatus
		reported bool
	}{
		{name: "success", err: nil, want: MutationCommitted, reported: true},
		{name: "timeout", err: apierrors.NewTimeoutError("timed out", 1), want: MutationAmbiguous, reported: true},
		{name: "internal", err: apierrors.NewInternalError(errors.New("boom")), want: MutationAmbiguous, reported: true},
		{name: "conflict", err: apierrors.NewConflict(
			v1alpha1.GroupVersion.WithResource("gaggles").GroupResource(), "web", errors.New("stale")), reported: false},
		{name: "forbidden", err: apierrors.NewForbidden(
			v1alpha1.GroupVersion.WithResource("gaggles").GroupResource(), "web", errors.New("nope")), reported: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reported := mutationStatus(tc.err)
			if reported != tc.reported {
				t.Fatalf("reported = %t, want %t", reported, tc.reported)
			}
			if reported && got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyErrorMessage(t *testing.T) {
	err := &ApplyError{
		Generation: "gabc",
		Phase:      "apply",
		Mutations: []Mutation{
			{Object: "Gaggle/goobers-system/web", Operation: "create", Status: MutationCommitted},
			{Object: "Gaggle/goobers-system/api", Operation: "update", Status: MutationAmbiguous},
		},
		Err: errors.New("boom"),
	}
	msg := err.Error()
	for _, want := range []string{"gabc", "apply", "create Gaggle/goobers-system/web (committed)", "update Gaggle/goobers-system/api (ambiguous)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	empty := (&ApplyError{Generation: "gabc", Phase: "switch", Err: errors.New("boom")}).Error()
	if !strings.Contains(empty, "no mutations committed") {
		t.Errorf("error %q should say no mutations committed", empty)
	}
}

// seedManaged snapshots the managed objects and the generation pointer of a
// client so a second (fault-injecting) client starts from the same state.
func seedManaged(t *testing.T, c client.Client) []client.Object {
	t.Helper()
	var gaggles v1alpha1.GaggleList
	if err := c.List(context.Background(), &gaggles, client.InNamespace(DefaultNamespace)); err != nil {
		t.Fatalf("list gaggles: %v", err)
	}
	seed := make([]client.Object, 0, len(gaggles.Items)+1)
	for i := range gaggles.Items {
		obj := gaggles.Items[i].DeepCopy()
		obj.SetResourceVersion("")
		obj.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Gaggle"))
		seed = append(seed, obj)
	}
	var pointer corev1.ConfigMap
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: DefaultNamespace, Name: GenerationConfigMapName}, &pointer)
	if err == nil {
		copied := pointer.DeepCopy()
		copied.SetResourceVersion("")
		seed = append(seed, copied)
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("read pointer: %v", err)
	}
	return seed
}
