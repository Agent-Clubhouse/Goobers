package engine

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestVersionPinning(t *testing.T) {
	r := NewRegistryWithPreviewFeatures(true)

	v1, err := r.Register("flow", linearSpec())
	if err != nil {
		t.Fatalf("register v1: %v", err)
	}
	if v1 != 1 {
		t.Fatalf("first version = %d, want 1", v1)
	}

	// A run started now pins v1.
	in1, err := r.StartInput("flow", StartSpec{RunID: "run-1", Gaggle: "web"})
	if err != nil {
		t.Fatalf("start v1: %v", err)
	}
	if in1.Version != 1 {
		t.Errorf("pinned version = %d, want 1", in1.Version)
	}
	if in1.PreviewFeaturesEnabled == nil || !*in1.PreviewFeaturesEnabled {
		t.Error("pinned input did not carry the registry preview-feature policy")
	}
	if len(in1.Spec.Gates) != 0 {
		t.Errorf("v1 should have no gates, got %d", len(in1.Spec.Gates))
	}

	// Register a new version (adds a gate).
	v2, err := r.Register("flow", gatedSpec())
	if err != nil {
		t.Fatalf("register v2: %v", err)
	}
	if v2 != 2 {
		t.Fatalf("second version = %d, want 2", v2)
	}

	// The earlier RunInput is unchanged — it still carries the v1 snapshot.
	if in1.Version != 1 || len(in1.Spec.Gates) != 0 {
		t.Errorf("in-flight run drifted: version=%d gates=%d", in1.Version, len(in1.Spec.Gates))
	}

	// A new run pins v2.
	in2, err := r.StartInput("flow", StartSpec{RunID: "run-2", Gaggle: "web"})
	if err != nil {
		t.Fatalf("start v2: %v", err)
	}
	if in2.Version != 2 {
		t.Errorf("new run pinned version = %d, want 2", in2.Version)
	}
	if len(in2.Spec.Gates) != 1 {
		t.Errorf("v2 should have 1 gate, got %d", len(in2.Spec.Gates))
	}
}

func TestGetAndLatest(t *testing.T) {
	r := NewRegistryWithPreviewFeatures(true)
	if _, ok := r.Latest("nope"); ok {
		t.Error("Latest on unknown should be false")
	}
	_, _ = r.Register("flow", linearSpec())
	_, _ = r.Register("flow", gatedSpec())

	def, ok := r.Get("flow", 1)
	if !ok || def.Version != 1 || len(def.Spec.Gates) != 0 {
		t.Errorf("Get v1 = %+v ok=%v", def, ok)
	}
	latest, ok := r.Latest("flow")
	if !ok || latest.Version != 2 {
		t.Errorf("Latest = %+v ok=%v; want version 2", latest, ok)
	}
	if _, ok := r.Get("flow", 99); ok {
		t.Error("Get of nonexistent version should be false")
	}
}

func TestRegisterInvalidRejected(t *testing.T) {
	r := NewRegistry()
	bad := apiv1.WorkflowSpec{Start: "ghost"} // start not defined
	if _, err := r.Register("bad", bad); err == nil {
		t.Fatal("expected registration of invalid workflow to fail")
	}
	if _, ok := r.Latest("bad"); ok {
		t.Error("invalid workflow should not be registered")
	}
}

func TestRegisterDefinitionRetainsDSLVersion(t *testing.T) {
	r := NewRegistryWithPreviewFeatures(true)
	if _, err := r.RegisterDefinition(wf.Definition{
		Name: "flow", DSLVersion: "1.4", Spec: linearSpec(),
	}); err != nil {
		t.Fatalf("RegisterDefinition: %v", err)
	}
	def, ok := r.Latest("flow")
	if !ok {
		t.Fatal("registered definition not found")
	}
	if def.DSLVersion != "1.4" {
		t.Fatalf("definition dslVersion = %q, want 1.4", def.DSLVersion)
	}
	in, err := r.StartInput("flow", StartSpec{RunID: "run-1", Gaggle: "web"})
	if err != nil {
		t.Fatalf("StartInput: %v", err)
	}
	if in.DSLVersion != "1.4" {
		t.Fatalf("run input dslVersion = %q, want 1.4", in.DSLVersion)
	}
}

func TestRegisterPreviewFeaturesRequiresOptIn(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Register("flow", previewSpec()); err == nil {
		t.Fatal("expected preview workflow registration without opt-in to fail")
	}
}

func TestStartUnregistered(t *testing.T) {
	r := NewRegistry()
	if _, err := r.StartInput("missing", StartSpec{RunID: "x"}); err == nil {
		t.Error("starting an unregistered workflow should error")
	}
}
