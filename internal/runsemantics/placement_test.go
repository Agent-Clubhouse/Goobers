package runsemantics

import (
	"os"
	"runtime"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestSelfPlacementSeparatesNodeFromHost: node is a CLUSTER NODE and comes
// only from the deployment's downward API. The process hostname — which inside
// a pod IS the pod name — is recorded as `host`, the field whose name is true
// of it, so the node field the mode-3 dispatcher fills with real node names is
// never polluted with pod names by a local run.
func TestSelfPlacementSeparatesNodeFromHost(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}

	t.Setenv(EnvPlacementNode, "")
	t.Setenv(EnvPlacementPod, "")
	t.Setenv(EnvPlacementImage, "")
	t.Setenv(EnvPlacementBuild, "")
	t.Setenv(EnvPlacementWorker, "")
	bare := SelfPlacement()
	if bare.Runner != journal.PlacementRunnerSelf || bare.OS != runtime.GOOS {
		t.Fatalf("bare placement = %+v, want runner=self os=%s", bare, runtime.GOOS)
	}
	if bare.Node != "" {
		t.Fatalf("bare placement node = %q, want empty — no authority declared a node, and the hostname is not one", bare.Node)
	}
	if bare.Host != hostname {
		t.Fatalf("bare placement host = %q, want hostname %q", bare.Host, hostname)
	}
	if bare.Build != "" || bare.Worker != "" {
		t.Fatalf("bare placement invented build/worker identity: %+v", bare)
	}
	if bare.Pod != "" || bare.Image != "" {
		t.Fatalf("bare placement invented pod/image identity: %+v", bare)
	}

	// A pod that declares only its pod name (the common downward-API subset)
	// must still leave node empty rather than borrowing the hostname, which
	// here is exactly the pod name.
	t.Setenv(EnvPlacementPod, hostname)
	podOnly := SelfPlacement()
	if podOnly.Node != "" {
		t.Fatalf("pod-only placement node = %q, want empty (the hostname is the POD name in a pod)", podOnly.Node)
	}
	if podOnly.Pod != hostname || podOnly.Host != hostname {
		t.Fatalf("pod-only placement = %+v, want pod=host=%q", podOnly, hostname)
	}

	t.Setenv(EnvPlacementNode, "aks-linux-0001")
	t.Setenv(EnvPlacementPod, "goobers-daemon-0")
	t.Setenv(EnvPlacementImage, "ghcr.io/goobers/goobers-base:v0.2.0")
	declared := SelfPlacement()
	if declared.Node != "aks-linux-0001" || declared.Pod != "goobers-daemon-0" ||
		declared.Image != "ghcr.io/goobers/goobers-base:v0.2.0" || declared.Host != hostname {
		t.Fatalf("deployment-declared placement = %+v", declared)
	}
}

// TestPlacementDeclaredGate is the zero-declaration invariance rule
// (goobernetes-architecture.md §11 item 1) expressed directly: provenance is
// recorded only once the deployment has said something about placement. The
// env arm is a NAMESPACE rule, not an enumeration — a GOOBERS_RUNNER_* var
// this file has never heard of still counts.
func TestPlacementDeclaredGate(t *testing.T) {
	cases := []struct {
		name            string
		runnersDeclared bool
		environ         []string
		want            bool
	}{
		{name: "zero declaration", environ: []string{"PATH=/usr/bin", "HOME=/root"}, want: false},
		{name: "runners inventory declared", runnersDeclared: true, want: true},
		{name: "node env declared", environ: []string{EnvPlacementNode + "=aks-linux-0001"}, want: true},
		{name: "pod env declared", environ: []string{EnvPlacementPod + "=goobers-daemon-0"}, want: true},
		{name: "image env declared", environ: []string{EnvPlacementImage + "=ghcr.io/x:v1"}, want: true},
		{
			// The gate must not be a list of the three variables that exist
			// today: a future placement identity variable declares placement
			// the moment the deployment sets it.
			name:    "future placement env declared",
			environ: []string{PlacementEnvNamespace + "ZONE=westus2-1"},
			want:    true,
		},
		{name: "empty-valued placement env is not a declaration", environ: []string{EnvPlacementNode + "="}, want: false},
		{name: "unrelated goobers env is not a declaration", environ: []string{"GOOBERS_BRANCH_NAMESPACE=goobers"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlacementDeclared(tc.runnersDeclared, tc.environ); got != tc.want {
				t.Fatalf("placementDeclared(%v, %v) = %v, want %v", tc.runnersDeclared, tc.environ, got, tc.want)
			}
		})
	}
}
