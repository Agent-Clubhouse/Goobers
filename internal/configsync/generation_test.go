package configsync

import (
	"context"
	"errors"
	"sort"
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

func selectedGaggleNames(t *testing.T, c client.Client, generation string) []string {
	t.Helper()
	var selected v1alpha1.GaggleList
	if err := c.List(context.Background(), &selected,
		client.InNamespace(DefaultNamespace), GenerationSelector(generation),
	); err != nil {
		t.Fatalf("select generation: %v", err)
	}
	names := make([]string, 0, len(selected.Items))
	for i := range selected.Items {
		name := selected.Items[i].Annotations[OriginalNameAnnotation]
		if name == "" {
			name = selected.Items[i].Name
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func selectedGaggle(t *testing.T, c client.Client, generation, name string) v1alpha1.Gaggle {
	t.Helper()
	var selected v1alpha1.GaggleList
	if err := c.List(context.Background(), &selected,
		client.InNamespace(DefaultNamespace), GenerationSelector(generation),
	); err != nil {
		t.Fatalf("select generation: %v", err)
	}
	for i := range selected.Items {
		got := selected.Items[i].Annotations[OriginalNameAnnotation]
		if got == "" {
			got = selected.Items[i].Name
		}
		if got == name {
			return selected.Items[i]
		}
	}
	t.Fatalf("generation %s did not select Gaggle %q; selected %v", generation, name, selectedGaggleNames(t, c, generation))
	return v1alpha1.Gaggle{}
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
	if got, want := selectedGaggleNames(t, c, generation), []string{"api", "web"}; !equalStrings(got, want) {
		t.Fatalf("selected gaggles = %v, want %v", got, want)
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
			if obj.GetAnnotations()[OriginalNameAnnotation] == "api" {
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
	if got, want := selectedGaggleNames(t, failingClient, previous), []string{"web"}; !equalStrings(got, want) {
		t.Errorf("previous generation selected gaggles = %v, want %v", got, want)
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
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetAnnotations()[OriginalNameAnnotation] == "web" {
				labels := obj.GetLabels()
				delete(labels, GenerationLabel)
				obj.SetLabels(labels)
			}
			return c.Create(ctx, obj, opts...)
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
	if got, want := selectedGaggleNames(t, failingClient, previous), []string{"web"}; !equalStrings(got, want) {
		t.Errorf("previous generation selected gaggles = %v, want %v", got, want)
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
	if got, want := selectedGaggleNames(t, failingClient, previous), []string{"stale"}; !equalStrings(got, want) {
		t.Errorf("previous generation selected gaggles = %v, want %v", got, want)
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
	if got, want := selectedGaggleNames(t, failingClient, published), []string{"web"}; !equalStrings(got, want) {
		t.Errorf("published generation selected gaggles = %v, want %v", got, want)
	}
}

func TestClientApplier_RetryPrunesAfterAmbiguousCommittedSwitch(t *testing.T) {
	a, c := newApplier(t, managedGaggle("stale"))
	if err := a.Apply(context.Background(), gaggleSet("stale")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	previous := authoritative(t, c)
	staleName := selectedGaggle(t, c, previous, "stale").Name

	failing, failingClient := newInterceptedApplier(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				if err := c.Update(ctx, obj, opts...); err != nil {
					return err
				}
				return apierrors.NewTimeoutError("pointer switch timed out after commit", 1)
			}
			return c.Update(ctx, obj, opts...)
		},
	}, seedManaged(t, c)...)

	if err := failing.Apply(context.Background(), gaggleSet("web")); err == nil {
		t.Fatal("apply should report the ambiguous pointer switch")
	}
	published := authoritative(t, failingClient)
	if published == previous {
		t.Fatalf("authoritative generation = %s, want committed new generation", published)
	}

	retry, retryClient := newApplier(t, seedManaged(t, failingClient)...)
	if err := retry.Apply(context.Background(), gaggleSet("web")); err != nil {
		t.Fatalf("retry apply: %v", err)
	}
	var stale v1alpha1.Gaggle
	err := retryClient.Get(context.Background(), types.NamespacedName{Namespace: DefaultNamespace, Name: staleName}, &stale)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("retry should prune replaced generation object, got err=%v", err)
	}
	if got, want := selectedGaggleNames(t, retryClient, published), []string{"web"}; !equalStrings(got, want) {
		t.Fatalf("published generation selected gaggles = %v, want %v", got, want)
	}
	assertNoPendingPrune(t, retryClient)
}

func TestClientApplier_NewGenerationCompletesPendingPrune(t *testing.T) {
	a, c := newApplier(t, managedGaggle("stale"))
	if err := a.Apply(context.Background(), gaggleSet("stale")); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	failing, failingClient := newInterceptedApplier(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return apierrors.NewTimeoutError("delete timed out", 1)
		},
	}, seedManaged(t, c)...)
	if err := failing.Apply(context.Background(), gaggleSet("web")); err == nil {
		t.Fatal("apply should fail while pruning the replaced generation")
	}
	published := authoritative(t, failingClient)

	next, nextClient := newApplier(t, seedManaged(t, failingClient)...)
	if err := next.Apply(context.Background(), gaggleSet("api")); err != nil {
		t.Fatalf("new generation should complete the pending prune and apply: %v", err)
	}
	if got := authoritative(t, nextClient); got == published {
		t.Fatalf("authoritative generation still %s, want new generation", got)
	}
	if got, want := selectedGaggleNames(t, nextClient, authoritative(t, nextClient)), []string{"api"}; !equalStrings(got, want) {
		t.Fatalf("selected gaggles = %v, want %v", got, want)
	}
	assertNoPendingPrune(t, nextClient)
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
	if got := set.Objects[0].GetName(); got != "web" {
		t.Errorf("apply renamed caller object to %q, want web", got)
	}
}

func assertNoPendingPrune(t *testing.T, c client.Client) {
	t.Helper()
	var pointer corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: DefaultNamespace, Name: GenerationConfigMapName}, &pointer); err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	if _, ok := pointer.Data[PreviousGenerationConfigMapKey]; ok {
		t.Fatalf("pending prune marker still set in pointer data: %v", pointer.Data)
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

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
