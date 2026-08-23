package dispatcher

import (
	"testing"

	"github.com/goobers/goobers/internal/netpolrender"
	"github.com/goobers/goobers/internal/runnercap"
)

// The load-bearing cross-producer guarantee (delivery decisions 015/016): the
// restriction-set preimage the dispatcher stamps on a stage pod
// (runnercap.AnnotationRunnerClassRestrictions, via stampClassRestrictionsAnnotation)
// and the preimage the per-runner-class NetworkPolicy renderer stamps on the
// class's policy MUST be the SAME string for the same restriction set, because
// both derive it from the ONE function runnercap.RunnerClassPreimage — never a
// second, drift-prone derivation. An operator reading the pod (case A: a stage
// that hangs because NO policy selects it, so there is nothing to read on the
// netpol side) and an operator reading the policy must see the identical
// preimage. This test renders both sides for the same inventory entry and pins
// them equal; a future edit that reintroduces a parallel derivation on either
// side fails HERE.
func TestPodAndNetworkPolicyRestrictionAnnotationsAgree(t *testing.T) {
	runner := linuxRunner() // network:allowlist + fs:readonly + tmp:ephemeral

	// Dispatcher side: the annotation stamped on the created pod.
	pod, err := RenderPod(testConfig(), testAttempt(), runner)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	podPreimage := pod.Annotations[runnercap.AnnotationRunnerClassRestrictions]
	if podPreimage == "" {
		t.Fatal("dispatcher stamped no runner-class-restrictions annotation on a restricted pod")
	}

	// Renderer side: the annotation stamped on the class's NetworkPolicy for
	// the SAME restriction set.
	rendered, err := netpolrender.Render(netpolrender.Input{
		Runners: []netpolrender.Runner{{Name: runner.Name, Restrictions: runner.Restrictions}},
		Allowlist: []netpolrender.AllowlistGroup{
			{Name: "github-provider", Kind: netpolrender.GroupKindProvider, CIDRs: []string{"140.82.112.0/20"}},
		},
	})
	if err != nil {
		t.Fatalf("netpolrender.Render: %v", err)
	}
	classValue := runnercap.RunnerClassValue(runner.Restrictions)
	policy := rendered.Policies[classValue]
	if policy == nil {
		t.Fatalf("renderer produced no policy for class %q", classValue)
	}
	netpolPreimage := policy.Annotations[runnercap.AnnotationRunnerClassRestrictions]

	if podPreimage != netpolPreimage {
		t.Fatalf("pod annotation %q != NetworkPolicy annotation %q — the two producers disagree; "+
			"both must derive from runnercap.RunnerClassPreimage (decision 015)", podPreimage, netpolPreimage)
	}
	// And both must equal the single function's output — proof there is no
	// third value hiding a coincidental match.
	if want := runnercap.RunnerClassPreimage(runner.Restrictions); podPreimage != want {
		t.Fatalf("stamped preimage %q != runnercap.RunnerClassPreimage %q", podPreimage, want)
	}
}
