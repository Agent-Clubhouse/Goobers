package main

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/instance"
)

// gateCapabilityFixture layers the run-controls fixture with four goobers:
// one scoped to the gaggle with grants, one global with grants, one with none
// at all, and one belonging to a DIFFERENT gaggle. The last is what makes the
// projection's scoping checkable: engine-start pins the goobers the worker for
// this gaggle will admit (workerwiring.go's resolveGoobersForGaggle rule), not
// the daemon's instance-wide gateGooberCaps map — see
// engineStartGateGooberCapabilities for why the two differ and in which
// direction.
func gateCapabilityFixture() (*instance.Config, *instance.ConfigSet) {
	cfg, set, _ := runControlsFixture()
	set.Goobers = []apiv1.Goober{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "reviewer"},
			Spec:       apiv1.GooberSpec{Gaggle: "web", Capabilities: []string{"repo:read", "model:invoke"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-reviewer"},
			Spec:       apiv1.GooberSpec{Capabilities: []string{"repo:read"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ungranted"},
			Spec:       apiv1.GooberSpec{Gaggle: "web"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "other-gaggle-reviewer"},
			Spec:       apiv1.GooberSpec{Gaggle: "mobile", Capabilities: []string{"repo:write"}},
		},
	}
	return cfg, set
}

// TestEngineStartSpecPinsGateGooberCapabilities is #294's pin on the
// engine-start path.
//
// An agentic gate's reviewer capabilities are instance policy, and every
// post-start consumer resolves them from the run's own pinned snapshot
// (runner.PinnedGateGooberCapabilities) rather than the currently-served
// config — the daemon credential plane's gate branch most of all. A starter
// that leaves StartSpec.GateGooberCapabilities empty therefore does not
// "inherit later": it pins an empty map, and every gate on the run resolves
// to no reviewer grants at all. The daemon's scheduler entry has always
// filled this in; engine-start did not.
func TestEngineStartSpecPinsGateGooberCapabilities(t *testing.T) {
	cfg, set := gateCapabilityFixture()
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	want := map[string][]string{
		"reviewer":        {"repo:read", "model:invoke"},
		"shared-reviewer": {"repo:read"},
	}
	if !reflect.DeepEqual(spec.GateGooberCapabilities, want) {
		t.Fatalf("StartSpec.GateGooberCapabilities = %v, want %v — an empty map pins NO reviewer grants into the run, it does not defer the question",
			spec.GateGooberCapabilities, want)
	}
}

// TestEngineStartGateGooberCapabilitiesReachRunInput carries the pin one seam
// further, through the registry that builds the RunInput the workflow
// actually receives: the engine's newRunJournal reads RunInput, so a spec
// field the registry drops never reaches run.yaml.
func TestEngineStartGateGooberCapabilitiesReachRunInput(t *testing.T) {
	cfg, set := gateCapabilityFixture()
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	reg, _, err := bootstrap.RegisterGaggleWorkflows(set, "web")
	if err != nil {
		t.Fatalf("register gaggle workflows: %v", err)
	}
	in, err := reg.StartInput("implementation", spec)
	if err != nil {
		t.Fatalf("StartInput: %v", err)
	}
	want := map[string][]string{
		"reviewer":        {"repo:read", "model:invoke"},
		"shared-reviewer": {"repo:read"},
	}
	if !reflect.DeepEqual(in.GateGooberCapabilities, want) {
		t.Fatalf("RunInput.GateGooberCapabilities = %v, want %v", in.GateGooberCapabilities, want)
	}
}

// TestEngineStartGateGooberCapabilitiesEmptyWithoutGrants keeps the
// zero-declaration arm honest: an instance whose goobers declare no
// capabilities pins no snapshot at all, exactly as before, rather than an
// empty-but-present one.
func TestEngineStartGateGooberCapabilitiesEmptyWithoutGrants(t *testing.T) {
	cfg, set, _ := runControlsFixture()
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	if spec.GateGooberCapabilities != nil {
		t.Fatalf("StartSpec.GateGooberCapabilities = %v, want nil for an instance declaring no goober capabilities", spec.GateGooberCapabilities)
	}
}
