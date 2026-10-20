package configsync

import (
	"context"
	"reflect"
	"testing"

	api "github.com/goobers/goobers/api/v1alpha1"
)

// Uses the real normal validator, loader, deep copies and generation applier;
// only the Kubernetes transport is fake. It does not access a live cluster.
func TestInteractiveNormalApplyPreservesReviewedConfiguration(t *testing.T) {
	loader, err := NewLoader("")
	if err != nil {
		t.Fatal(err)
	}
	set, report, err := loader.Load("../../samples/interactive-config")
	if err != nil {
		t.Fatalf("load: %v %+v", err, report)
	}
	a, c := newApplier(t)
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	generation := authoritative(t, c)
	g := selectedGaggle(t, c, generation, "interactive")
	if g.DSLVersion != "3.0" || g.Spec.Interactive == nil || len(g.Spec.Interactive.Surfaces) != 2 {
		t.Fatalf("applier dropped interactive config: %+v", g)
	}
	before := g.Spec.Interactive.DeepCopy()
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if authoritative(t, c) != generation {
		t.Fatal("unchanged interactive apply changed generation")
	}
	g = selectedGaggle(t, c, generation, "interactive")
	if !reflect.DeepEqual(before, g.Spec.Interactive) {
		t.Fatal("apply replay changed policy")
	}
	for _, object := range set.Objects {
		if declaration, ok := object.(*api.Gaggle); ok {
			declaration.Spec.Interactive.Surfaces[0].Session.MaxChildren = 1
		}
	}
	if err := a.Apply(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	next := authoritative(t, c)
	if next == generation {
		t.Fatal("changed interactive policy retained old generation")
	}
	updated := selectedGaggle(t, c, next, "interactive")
	if updated.Spec.Interactive.Surfaces[0].Session.MaxChildren != 1 {
		t.Fatal("policy update not published")
	}
	if before.Surfaces[0].Session.MaxChildren != 3 {
		t.Fatal("published policy aliases prior reviewed snapshot")
	}
}
