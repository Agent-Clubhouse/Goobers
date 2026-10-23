package engine

// The engine's half of the #3884 goober-digest pin: the run pins a kit
// digest, and EVERY stage envelope it dispatches carries it, because the
// worker on the far side resolves its kit by that value or refuses.
//
// Before this, GooberDigest reached run.yaml and stopped there — it was
// provenance the worker never saw, so a worker whose config tree had rolled
// forward silently ran attempt N+1 as a different curator than attempt N. The
// field on the envelope is the whole mechanism by which that became
// detectable; a regression that dropped it would leave every pin test on the
// worker side green (they drive the resolver directly) and put the gap back.

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestBuildInvocationCarriesThePinnedGooberDigest(t *testing.T) {
	const pin = "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	in := RunInput{
		RunID:        "run-3884",
		Gaggle:       "web",
		WorkflowName: "pin",
		GooberDigest: pin,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}

	// A task envelope and a reviewer-gate envelope: both dispatch to the same
	// worker seam, and a gate reviewer executes a goober kit exactly as a task
	// does, so neither may travel unpinned.
	task := buildInvocation(in, "implement", "do the work", nil, nil, apiv1.Limits{}, nil, "coder")
	if task.GooberDigest != pin {
		t.Fatalf("task envelope GooberDigest = %q, want the run's pin %q; the worker cannot honour a pin it never receives", task.GooberDigest, pin)
	}
	reviewer := buildInvocation(in, "review", "review the work", nil, nil, apiv1.Limits{}, nil, "reviewer")
	if reviewer.GooberDigest != pin {
		t.Fatalf("gate envelope GooberDigest = %q, want the run's pin %q", reviewer.GooberDigest, pin)
	}

	// An unpinned run dispatches unpinned envelopes — the compatibility floor
	// that keeps every run started before D1 executing exactly as before.
	in.GooberDigest = ""
	if unpinned := buildInvocation(in, "implement", "do the work", nil, nil, apiv1.Limits{}, nil, "coder"); unpinned.GooberDigest != "" {
		t.Fatalf("unpinned envelope GooberDigest = %q, want empty", unpinned.GooberDigest)
	}
}
