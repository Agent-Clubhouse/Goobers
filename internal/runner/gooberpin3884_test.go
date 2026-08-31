package runner

// The local runner's half of the #3884 goober-digest pin.
//
// The runner has never needed the pin to SELECT anything: it resolves the kit
// in the same process that computed the digest, so its executor cannot drift
// from the identity it recorded. What it does owe is PARITY on the wire — its
// envelopes must name the run's kit exactly as the engine's do, or the two
// drivers' dispatch payloads stop being comparable and the pin becomes an
// engine-only concept that nothing can diff against a runner baseline.
//
// This is the assertion that would fail if someone added GooberDigest to the
// engine's buildInvocation and stopped there.

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/worktree"
)

func TestRunnerStampsThePinnedGooberDigestOnEveryEnvelope(t *testing.T) {
	root := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	r, err := New(Config{
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  wtMgr,
		RunsDir:    filepath.Join(root, "runs"),
		ScratchDir: filepath.Join(root, "scratch"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const pin = "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	in := StartInput{
		RunID:        "run-3884",
		Gaggle:       "acme-web",
		Machine:      fixtureMachine(t),
		GooberDigest: pin,
	}
	env, workspace, err := r.buildEnvelope(
		context.Background(), in, "implement", "do the work", nil,
		nil, apiv1.Limits{}, nil, apiv1.WorkspaceScratch, false, "",
	)
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	t.Cleanup(func() { _ = workspace.Remove(context.Background()) })

	if env.GooberDigest != pin {
		t.Fatalf("runner envelope GooberDigest = %q, want the run's pin %q — the engine's envelopes carry it "+
			"(engine.buildInvocation) and the two drivers' dispatch payloads must name the same kit", env.GooberDigest, pin)
	}

	// An unpinned run leaves it empty rather than inventing one, which is what
	// keeps runs started before the pin existed byte-identical on the wire.
	in.GooberDigest = ""
	unpinned, unpinnedWorkspace, err := r.buildEnvelope(
		context.Background(), in, "implement", "do the work", nil,
		nil, apiv1.Limits{}, nil, apiv1.WorkspaceScratch, false, "",
	)
	if err != nil {
		t.Fatalf("buildEnvelope (unpinned): %v", err)
	}
	t.Cleanup(func() { _ = unpinnedWorkspace.Remove(context.Background()) })
	if unpinned.GooberDigest != "" {
		t.Fatalf("unpinned runner envelope GooberDigest = %q, want empty", unpinned.GooberDigest)
	}
}
